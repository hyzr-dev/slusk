package soulseek

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
)

// speedStaleAfter bounds how long a transfer's last-sampled speed is trusted
// once its progress callback goes quiet (see transfer.speedAt): a stalled
// transfer's most recent sample would otherwise be reported by ListDownloads
// forever, since nothing else ever overwrites tr.speed/tr.speedAvg (issue
// #157).
const speedStaleAfter = 3 * time.Second

// speedEWMAAlpha weights the most recent sample when smoothing tr.speedAvg
// (issue #157): a higher value tracks recent changes more closely, a lower
// value smooths out jitter more. 0.3 favors smoothness — speedAvg backs ETA
// math, which should not swing wildly between two consecutive samples.
const speedEWMAAlpha = 0.3

// ewma folds sample into prev with weight alpha, the standard exponentially
// weighted moving average update.
func ewma(prev, sample int64, alpha float64) int64 {
	return int64(alpha*float64(sample) + (1-alpha)*float64(prev))
}

// destLeaf returns the local subdirectory name a downloaded file is written
// into: the base name of the file's remote directory, with Soulseek's "\"
// path separators normalized to "/". It returns "" when the file has no
// meaningful parent directory (the file is written directly under the
// downloads root then).
//
// This deliberately mirrors internal/pipeline/paths.go:commonLeaf for a single
// file, so a natively-downloaded file lands in exactly the same place slskd
// would have written it and the Importing module's AlbumFolder scan still
// finds it. It is a copy rather than a shared import because internal/soulseek
// is a low-level protocol provider and importing the high-level pipeline
// package would invert the layering (cf. nextBackoff copied in client.go).
// TestDownloadDestPathMatchesPipelineAlbumFolder locks the two against drift.
func destLeaf(filename string) string {
	dir := path.Dir(strings.ReplaceAll(filename, `\`, "/"))
	if dir == "." || dir == "/" || dir == "" {
		return ""
	}
	return path.Base(dir)
}

// downloadDestPath returns the absolute local path a downloaded file is written
// to under completeDir, matching the completeDir/<leaf>/<base> layout slskd
// produces and pipeline.AlbumFolder expects. It is a pure path-layout function
// and performs no safety checks — callers that touch disk MUST route through
// safeDownloadDest, which enforces containment.
func downloadDestPath(completeDir, filename string) string {
	base := path.Base(strings.ReplaceAll(filename, `\`, "/"))
	if leaf := destLeaf(filename); leaf != "" {
		return filepath.Join(completeDir, leaf, base)
	}
	return filepath.Join(completeDir, base)
}

// errUnsafeDownloadPath marks a peer-supplied filename that resolves outside the
// configured download directory (a ".." path-traversal attempt). It is a
// security boundary, not a convenience check: filename comes verbatim from the
// remote peer's share path, so a hostile peer can name a file `..\evil` to try
// to write, delete, or overwrite files outside the download root.
var errUnsafeDownloadPath = errors.New("soulseek: download path escapes the download directory")

// safeDownloadDest returns the local path filename is written to under
// downloadDir, or errUnsafeDownloadPath if the peer-controlled filename would
// escape downloadDir. Every disk-touching path (write, .part removal) resolves
// its destination through here so a crafted `..`-laden filename can never land
// bytes — or aim a delete — outside the download root. filepath.Join cleans
// `..` but does not contain it (Join(root, "..", "x") == parent/x), so the
// pathWithinRoot check after joining is what actually enforces the boundary.
func safeDownloadDest(downloadDir, filename string) (string, error) {
	dest := downloadDestPath(downloadDir, filename)
	if !pathWithinRoot(downloadDir, dest) {
		return "", fmt.Errorf("%w: %q", errUnsafeDownloadPath, filename)
	}
	return dest, nil
}

// permanentUploadFailureReasons are the substrings that mark a peer's upload
// rejection as permanent (not worth re-queueing). Kept identical to
// internal/slskd/client.go:isTransientFailure so the native downloader reports
// failures in the same vocabulary the pipeline's retry logic already
// understands.
var permanentUploadFailureReasons = []string{"file not shared", "not shared", "banned"}

// categorizeUploadFailure classifies a peer's upload rejection or failure
// reason as permanent (retryable == false) or transient (retryable == true),
// mirroring internal/slskd/client.go:isTransientFailure. The reason is echoed
// back unchanged as failure. It expresses no opinion on non-failure reasons
// such as "Queued" (a peer keeping us in its queue): the download orchestration
// decides those before ever calling this.
func categorizeUploadFailure(reason string) (failure string, retryable bool) {
	lower := strings.ToLower(reason)
	for _, permanent := range permanentUploadFailureReasons {
		if strings.Contains(lower, permanent) {
			return reason, false
		}
	}
	return reason, true
}

// transfer is one file download's in-memory state. It is the shared record
// the P-session download hooks (Group E: TransferRequest/PlaceInQueueResponse/
// UploadDenied/UploadFailed) and the F-connection handler
// (handleInboundFileConn, this group) both act on, and that ListDownloads
// (Group E) reports from - all without either side needing to know which P
// session, if any, is currently registered for the peer, since the shared
// session can be torn down and re-established at any point while a download
// is queued (see getOrConnectPeerSession).
type transfer struct {
	id       string
	username string
	filename string
	// logger, set by Enqueue after construction, records the reason a download
	// errors so it is observable in the log instead of only via a ListDownloads
	// field the caller discards. nil (e.g. in tests) disables that logging.
	logger *slog.Logger
	// size is the enqueued file size, updated once (under mu) to the peer's
	// authoritative TransferRequest.FileSize when the transfer is negotiated,
	// so it is read under mu by a concurrent ListDownloads snapshot even though
	// it is declared above mu here to keep it beside the other identity fields.
	size int64
	// token is the token from the peer's TransferRequest, echoed back to us
	// unchanged in the F connection's TransferInit. It is the zero value
	// until the download hook (Group E) observes a TransferRequest and calls
	// downloadRegistry.registerToken; guarded by mu like the other fields a
	// concurrent ListDownloads snapshot reads, even though it is declared
	// above mu here to keep the wire-correlation fields grouped together.
	token soul.Token

	mu            sync.Mutex // guards state, failure, retryable, queuePosition, speed, speedAvg, speedAt, size, token, awaitingFileConn
	state         core.TransferState
	failure       string
	retryable     bool
	queuePosition uint32
	speed         int64
	// speedAvg is an EWMA-smoothed transfer rate in bytes per second, updated
	// alongside speed by the same progress-callback sample; it backs
	// core.RemoteTransfer.SpeedAverage (issue #157), which ETA math divides
	// remaining bytes by rather than the jumpy instantaneous speed.
	speedAvg int64
	// speedAt is when speed/speedAvg were last updated. A transfer whose
	// progress callback has gone quiet (stalled peer, no more reads) keeps
	// speed frozen at its last value forever unless something notices the
	// silence — ListDownloads checks speedAt against speedStaleAfter and
	// reports zero instead of a stale speed once it goes quiet for longer
	// than that.
	speedAt time.Time
	// awaitingFileConn is true only while runDownload is parked in the
	// negotiation select waiting for the peer's F connection on fileConnCh.
	// attachFileConn checks it under mu so an F connection that arrives after
	// runDownload has moved on (cancelled, failed, or timed out) is refused and
	// closed by its caller rather than delivered into a buffered channel nobody
	// will ever read - which would leak the socket and its inbound lease.
	awaitingFileConn bool

	// bytesDone is written by streamFile's progress callback as bytes land on
	// disk and read concurrently by ListDownloads (Group E); kept outside mu
	// so a slow snapshot reader can never block the write path mid-transfer.
	bytesDone atomic.Int64

	// cancel is set by Enqueue (Group E). Cancel() invokes it to unwind
	// whichever step of the orchestration goroutine is currently running.
	cancel context.CancelFunc

	// transferRequestCh, fileConnCh and failCh are how the P-session download
	// hooks (Group E) and handleInboundFileConn (this group) deliver protocol
	// events to the orchestration goroutine waiting on them (Group E's
	// runDownload). All are buffered 1, so a delivery never blocks the
	// delivering hook on a goroutine that (e.g. after a Cancel, or a
	// duplicate delivery) is no longer reading; attachFileConn's bool return
	// lets its caller tell the two cases apart.
	transferRequestCh chan peer.TransferRequest
	fileConnCh        chan fileConnHandoff
	failCh            chan transferFailure
}

// fileConnHandoff is delivered on transfer.fileConnCh by
// handleInboundFileConn once an F connection's TransferInit token has been
// matched to a pending transfer: the raw, not-yet-owned socket, plus the
// inbound lease it holds (nil on the outgoing/mirror-dial path, which
// consumes no inbound lease - see handleConnectToPeer). Whoever reads it off
// the channel owns closing conn and releasing lease, whether the stream
// completes, fails, or the download is cancelled mid-transfer.
type fileConnHandoff struct {
	conn  net.Conn
	lease *inboundLease
}

// transferFailure is delivered on transfer.failCh by the download hooks
// (Group E, for UploadDenied/UploadFailed) to report a peer-originated
// failure to the orchestration goroutine.
type transferFailure struct {
	reason    string
	retryable bool
}

// newTransfer allocates a transfer in the initial core.TransferQueued state
// with its handoff channels ready to receive.
func newTransfer(id, username, filename string, size int64) *transfer {
	return &transfer{
		id:                id,
		username:          username,
		filename:          filename,
		size:              size,
		state:             core.TransferQueued,
		transferRequestCh: make(chan peer.TransferRequest, 1),
		fileConnCh:        make(chan fileConnHandoff, 1),
		failCh:            make(chan transferFailure, 1),
	}
}

// attachFileConn hands an established F connection to the orchestration
// goroutine parked in runDownload's negotiation select, reporting whether the
// handoff landed. It only delivers while tr.awaitingFileConn is set - the
// window in which runDownload is actually reading fileConnCh - checked under
// tr.mu so the decision is atomic with runDownload's own give-up
// (stopAwaitingFileConn also runs under tr.mu). A false return means runDownload
// is not, or is no longer, waiting: the caller (handleInboundFileConn) then
// closes conn and releases lease itself, since ownership was never transferred.
// This must not treat the buffered channel merely having a free slot as a
// waiting receiver, or an F connection arriving after runDownload gave up would
// sit unread and leak the socket and its inbound lease.
func (tr *transfer) attachFileConn(conn net.Conn, lease *inboundLease) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if !tr.awaitingFileConn {
		return false
	}
	select {
	case tr.fileConnCh <- fileConnHandoff{conn: conn, lease: lease}:
		return true
	default:
		// awaitingFileConn implies fileConnCh is empty (claimByToken admits at
		// most one F connection per transfer), so this is unreachable; refuse
		// rather than block under mu if it ever is reached.
		return false
	}
}

// stopAwaitingFileConn clears the awaiting flag and returns any F connection
// attachFileConn delivered just before it was cleared, for runDownload to close
// on a give-up path. Clearing the flag under tr.mu serialises against
// attachFileConn: after this returns, attachFileConn refuses (returns false)
// any not-yet-delivered F connection, so its caller closes that one instead.
func (tr *transfer) stopAwaitingFileConn() (fileConnHandoff, bool) {
	tr.mu.Lock()
	tr.awaitingFileConn = false
	tr.mu.Unlock()
	select {
	case handoff := <-tr.fileConnCh:
		return handoff, true
	default:
		return fileConnHandoff{}, false
	}
}

// closeLeftoverFileConn closes an F connection returned by stopAwaitingFileConn
// (if any), releasing its inbound lease - the give-up counterpart of
// runDownload's normal defer conn.Close()/lease.Release().
func closeLeftoverFileConn(handoff fileConnHandoff, ok bool) {
	if !ok {
		return
	}
	_ = handoff.conn.Close()
	handoff.lease.Release()
}

// downloadKey returns the byKey registry key for a (username, filename)
// pair - the pair the Soulseek protocol actually correlates a queued download
// by, since an inbound TransferRequest carries no memory of which prior P
// session originated the QueueUpload it is answering.
func downloadKey(username, filename string) string {
	return username + "\x00" + filename
}

// downloadRegistry is the client-wide, in-memory table of in-flight
// downloads, indexed three ways: by id (ListDownloads/Cancel/Remove, Group E,
// address a transfer this way), by (username, filename) (so an inbound
// TransferRequest or PlaceInQueueResponse can be routed to the right transfer
// without knowing which P session it arrived on), and by token, once the
// peer's TransferRequest reveals it, so handleInboundFileConn can match an
// incoming F connection's TransferInit to the transfer expecting it.
type downloadRegistry struct {
	mu      sync.Mutex
	byID    map[string]*transfer
	byKey   map[string]*transfer
	byToken map[soul.Token]*transfer
}

func newDownloadRegistry() *downloadRegistry {
	return &downloadRegistry{
		byID:    make(map[string]*transfer),
		byKey:   make(map[string]*transfer),
		byToken: make(map[soul.Token]*transfer),
	}
}

// insert registers tr under its id and (username, filename) key. It does not
// touch byToken: the token is not known until the peer's TransferRequest
// arrives (see registerToken).
func (r *downloadRegistry) insert(tr *transfer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[tr.id] = tr
	r.byKey[downloadKey(tr.username, tr.filename)] = tr
}

// insertIfAbsent registers tr unless a transfer for its (username, filename)
// key already exists, in which case it registers nothing and returns that
// existing transfer. The check and insert are one atomic step under the
// registry lock, so two concurrent Enqueue calls for the same file never both
// create an orchestration goroutine over the same destination.
func (r *downloadRegistry) insertIfAbsent(tr *transfer) (existing *transfer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := downloadKey(tr.username, tr.filename)
	if cur, ok := r.byKey[key]; ok {
		return cur
	}
	r.byID[tr.id] = tr
	r.byKey[key] = tr
	return nil
}

// lookupByKey returns the transfer registered for (username, filename), or
// nil if none is.
func (r *downloadRegistry) lookupByKey(username, filename string) *transfer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byKey[downloadKey(username, filename)]
}

// lookupByID returns the transfer registered under id, or nil if none is.
func (r *downloadRegistry) lookupByID(id string) *transfer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[id]
}

// registerToken records the token a peer's TransferRequest assigned to tr,
// both on tr itself (guarded by tr.mu) and in byToken, so a subsequent F
// connection's TransferInit can be matched back to tr via claimByToken.
func (r *downloadRegistry) registerToken(tr *transfer, token soul.Token) {
	tr.mu.Lock()
	old := tr.token
	tr.token = token
	tr.mu.Unlock()

	r.mu.Lock()
	// A peer re-sending TransferRequest with a fresh token (e.g. after its own
	// retry) must not leave the previous token stranded in byToken - a stale
	// entry both leaks map memory and could misroute a late F connection.
	if old != token {
		if cur, ok := r.byToken[old]; ok && cur == tr {
			delete(r.byToken, old)
		}
	}
	r.byToken[token] = tr
	r.mu.Unlock()
}

// claimByToken looks up and atomically removes token's transfer: an F
// connection's TransferInit is matched to at most one transfer, ever, since
// the incoming socket is a one-shot resource that either lands on the
// transfer waiting for it or must be closed by the caller as unowned. It
// reports nil for an unknown or already-claimed token.
func (r *downloadRegistry) claimByToken(token soul.Token) *transfer {
	r.mu.Lock()
	defer r.mu.Unlock()
	tr, ok := r.byToken[token]
	if !ok {
		return nil
	}
	delete(r.byToken, token)
	return tr
}

// remove deregisters tr from every index, including byToken. It compares the
// map's current value at tr's token against tr itself (rather than assuming
// presence) before deleting, so it can never remove a different transfer's
// entry even in the edge case of an untokened transfer's zero-value token
// colliding with another transfer's real, peer-chosen token.
func (r *downloadRegistry) remove(tr *transfer) {
	tr.mu.Lock()
	token := tr.token
	tr.mu.Unlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, tr.id)
	delete(r.byKey, downloadKey(tr.username, tr.filename))
	if cur, ok := r.byToken[token]; ok && cur == tr {
		delete(r.byToken, token)
	}
}

// snapshot returns a point-in-time copy of every registered transfer, for
// ListDownloads (Group E).
func (r *downloadRegistry) snapshot() []*transfer {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*transfer, 0, len(r.byID))
	for _, tr := range r.byID {
		out = append(out, tr)
	}
	return out
}

// downloadID deterministically derives a transfer's registry id from its
// (username, filename) key: hex(sha1(username\x00filename))[:16]. Being
// deterministic rather than random makes Enqueue idempotent - a second
// Enqueue call for the same pair returns the same id a caller (e.g. the
// pipeline reconciler, after a restart or a retried request) may already be
// tracking, without either side needing to remember which call was first.
func downloadID(username, filename string) string {
	sum := sha1.Sum([]byte(downloadKey(username, filename)))
	return hex.EncodeToString(sum[:])[:16]
}

// downloadSessionHooks is the P-session hook (issue #55) that claims the
// download side of the protocol: TransferRequest, PlaceInQueueResponse,
// UploadFailed and UploadDenied - the codes a peer sends us while we are
// downloading from them. Every other P code (including TransferResponse,
// which we send rather than receive for a download) is reported unhandled
// via errUnhandledPeerFrame so a sibling hook, or composedSessionHooks
// itself for a genuinely unclaimed code, can decide what to do with it (see
// composedSessionHooks.frame in tree.go).
//
// Dispatch is keyed by (session.key.username, message.Filename) via
// downloadRegistry.lookupByKey rather than by which P session the frame
// arrived on, since the shared P session backing a download can be torn down
// and re-established at any point while the download is queued (see
// getOrConnectPeerSession): an inbound TransferRequest or
// PlaceInQueueResponse carries no memory of which session originated the
// QueueUpload it answers.
type downloadSessionHooks struct {
	downloads *downloadRegistry
	logger    *slog.Logger
}

func (*downloadSessionHooks) established(*peerSession) {}

func (h *downloadSessionHooks) frame(session *peerSession, frame sessionFrame) error {
	if frame.connType != peer.ConnectionType {
		return errUnhandledPeerFrame
	}

	switch peer.Code(frame.code) {
	case peer.CodeTransferRequest:
		msg := &peer.TransferRequest{}
		if err := msg.Deserialize(bytes.NewReader(frame.wire)); err != nil {
			return fmt.Errorf("deserialize transfer request: %w", err)
		}
		if msg.Direction == peer.DownloadFromPeer {
			// Soulseek.NET/slskd still initiates downloads with the legacy
			// direction-0 TransferRequest. That is an upload request from our
			// perspective, so leave it for uploadSessionHooks.
			return errUnhandledPeerFrame
		}
		tr := h.downloads.lookupByKey(session.key.username, msg.Filename)
		if tr == nil {
			if h.logger != nil {
				h.logger.Debug("transfer request for unknown or already-finished download", "username", session.key.username, "filename", msg.Filename)
			}
			return nil
		}
		h.downloads.registerToken(tr, msg.Token)
		select {
		case tr.transferRequestCh <- *msg:
		default:
			if h.logger != nil {
				h.logger.Debug("transfer request channel already full; orchestration not ready for it", "username", session.key.username, "filename", msg.Filename)
			}
		}
		return nil

	case peer.CodeTransferResponse:
		// TransferResponse is received by the upload side; yield it to the
		// upload hook while preserving all download routing for code 40.
		return errUnhandledPeerFrame

	case peer.CodePlaceInQueueResponse:
		msg := &peer.PlaceInQueueResponse{}
		if err := msg.Deserialize(bytes.NewReader(frame.wire)); err != nil {
			return fmt.Errorf("deserialize place in queue response: %w", err)
		}
		if tr := h.downloads.lookupByKey(session.key.username, msg.Filename); tr != nil {
			tr.mu.Lock()
			tr.queuePosition = msg.Place
			tr.mu.Unlock()
		}
		return nil

	case peer.CodeUploadFailed:
		msg := &peer.UploadFailed{}
		if err := msg.Deserialize(bytes.NewReader(frame.wire)); err != nil {
			return fmt.Errorf("deserialize upload failed: %w", err)
		}
		if tr := h.downloads.lookupByKey(session.key.username, msg.Filename); tr != nil {
			deliverTransferFailure(tr, transferFailure{reason: "upload failed", retryable: true})
		}
		return nil

	case peer.CodeUploadDenied:
		msg := &peer.UploadDenied{}
		if err := msg.Deserialize(bytes.NewReader(frame.wire)); err != nil {
			return fmt.Errorf("deserialize upload denied: %w", err)
		}
		if tr := h.downloads.lookupByKey(session.key.username, msg.Filename); tr != nil {
			reason := ""
			if msg.Reason != nil {
				reason = msg.Reason.Error()
			}
			failure, retryable := categorizeUploadFailure(reason)
			deliverTransferFailure(tr, transferFailure{reason: failure, retryable: retryable})
		}
		return nil

	default:
		return errUnhandledPeerFrame
	}
}

func (*downloadSessionHooks) closed(*peerSession, error) {}

// deliverTransferFailure delivers f on tr.failCh without blocking: the
// channel is buffered 1, so a second failure arriving before runDownload
// reads the first - or after runDownload has already stopped reading it, e.g.
// following a Cancel - is dropped rather than stalling the P session's sole
// reader.
func deliverTransferFailure(tr *transfer, f transferFailure) {
	select {
	case tr.failCh <- f:
	default:
	}
}

// Enqueue starts a native download of (username, filename), fire-and-forget:
// it registers the transfer in the client-wide registry and starts its
// orchestration goroutine (runDownload), then returns the transfer's
// deterministic id immediately without waiting for any protocol exchange.
// ctx does not bound the transfer's lifetime - runDownload runs for as long
// as the client's own lifecycle does, or until Cancel/Remove is called - it
// is accepted only to satisfy pipeline.PeerSearcher's signature.
//
// A second Enqueue for a (username, filename) pair that already has an
// active transfer is idempotent: it returns the existing transfer's id and
// starts no second goroutine, so a caller (e.g. the pipeline reconciler)
// that calls Enqueue more than once for the same file - after a restart, or
// a retried request - never races two orchestrations over the same
// destination file.
func (c *Client) Enqueue(ctx context.Context, username, filename string, size int64) (string, error) {
	// filename is the peer's own share path, fully attacker-controlled. Reject
	// any name that would escape the download directory before it can create a
	// transfer, write a file, or leave a ".part" behind for Remove to chase.
	if _, err := safeDownloadDest(c.cfg.DownloadDir, filename); err != nil {
		return "", err
	}
	id := downloadID(username, filename)
	tr := newTransfer(id, username, filename, size)
	tr.logger = c.logger
	// tr.cancel is set before insertIfAbsent makes tr reachable via the
	// registry, so a Cancel racing right after registration always finds a
	// usable cancel func rather than a nil one.
	trCtx, cancel := context.WithCancel(c.lifecycleContext())
	tr.cancel = cancel

	if existing := c.downloads.insertIfAbsent(tr); existing != nil {
		cancel() // discard the unused context for the deduplicated transfer
		return existing.id, nil
	}

	if !c.startTracked(func() { c.runDownload(trCtx, tr) }) {
		cancel()
		setTransferErrored(tr, "soulseek: client lifecycle is stopping", true)
		c.downloads.remove(tr)
	}
	return id, nil
}

// ListDownloads returns a point-in-time snapshot of every native download
// this client currently knows about (queued, in progress, or terminal but
// not yet Remove()d), mapped to core.RemoteTransfer. Implements
// pipeline.PeerNetwork.
func (c *Client) ListDownloads(ctx context.Context) ([]core.RemoteTransfer, error) {
	snapshot := c.downloads.snapshot()
	out := make([]core.RemoteTransfer, 0, len(snapshot))
	now := time.Now()
	for _, tr := range snapshot {
		tr.mu.Lock()
		state, failure, retryable, queuePosition, speed, speedAvg, speedAt, size := tr.state, tr.failure, tr.retryable, tr.queuePosition, tr.speed, tr.speedAvg, tr.speedAt, tr.size
		tr.mu.Unlock()
		// A transfer whose progress callback has gone quiet keeps its last
		// sampled speed forever otherwise (nothing else ever overwrites
		// tr.speed/tr.speedAvg) — report zero instead of a stale reading once
		// it has been silent longer than speedStaleAfter (issue #157).
		if speedAt.IsZero() || now.Sub(speedAt) > speedStaleAfter {
			speed, speedAvg = 0, 0
		}
		out = append(out, core.RemoteTransfer{
			ID:            tr.id,
			Username:      tr.username,
			Filename:      tr.filename,
			State:         state,
			Size:          size,
			BytesDone:     tr.bytesDone.Load(),
			Failure:       failure,
			Retryable:     retryable,
			QueuePosition: queuePosition,
			Speed:         speed,
			SpeedAverage:  speedAvg,
		})
	}
	return out, nil
}

// Cancel stops a still-active native download's orchestration goroutine and
// marks it TransferCancelled, leaving its registry entry (and any partial
// ".part" file already on disk) in place for a subsequent Remove. Implements
// both pipeline.PeerSearcher and pipeline.PeerNetwork - their Cancel
// signatures are identical, so one method satisfies both.
func (c *Client) Cancel(ctx context.Context, username, id string) error {
	tr := c.downloads.lookupByID(id)
	if tr == nil {
		return fmt.Errorf("cancel download %s: %w", id, core.ErrRemoteNotFound)
	}
	// The terminal state is set before tr.cancel is invoked, not after:
	// closing a context's Done channel happens-after every write the
	// cancelling goroutine made beforehand (Go's channel-close memory
	// model), so runDownload's own ctx.Done() branch - which checks whether
	// the state is already TransferCancelled before deciding whether to mark
	// the download errored instead, see finishInterrupted - is guaranteed to
	// observe this write rather than racing it.
	tr.mu.Lock()
	if tr.state == core.TransferCompleted {
		// A download that already finished is not cancellable: leave the
		// successful terminal state (and the file on disk) intact.
		tr.mu.Unlock()
		return nil
	}
	tr.state = core.TransferCancelled
	tr.mu.Unlock()
	if tr.cancel != nil {
		tr.cancel()
	}
	return nil
}

// Remove purges id's registry entry and deletes any partial (".part") file
// left on disk for it, cancelling its orchestration first if still active.
// Implements pipeline.PeerNetwork.
func (c *Client) Remove(ctx context.Context, username, id string) error {
	tr := c.downloads.lookupByID(id)
	if tr == nil {
		return fmt.Errorf("remove download %s: %w", id, core.ErrRemoteNotFound)
	}
	if tr.cancel != nil {
		tr.cancel()
	}
	c.downloads.remove(tr)

	dest, err := safeDownloadDest(c.cfg.DownloadDir, tr.filename)
	if err != nil {
		// Enqueue rejects escaping filenames, so a registered transfer should
		// never have one; if it somehow does, there is no safe ".part" path to
		// delete. The registry entry is already gone, so report clean rather
		// than aim os.Remove outside the download root.
		return nil
	}
	partPath := dest + ".part"
	if err := os.Remove(partPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove partial download %s: %w", partPath, err)
	}
	return nil
}

// DeleteDownloadFolder deletes name's directory under Config.DownloadDir -
// the per-release leaf directory downloaded files land in (see
// downloadDestPath) - and everything in it. Implements
// pipeline.PeerSearcher.
func (c *Client) DeleteDownloadFolder(ctx context.Context, name string) error {
	dir := filepath.Join(c.cfg.DownloadDir, name)
	// name originates from the peer's share paths (via pipeline.commonLeaf), so
	// it is untrusted: a crafted candidate whose files all sit in a `..\` remote
	// folder yields name == "..", and os.RemoveAll(filepath.Join(root, "..")))
	// would recursively delete the parent of the download dir. Refuse anything
	// that resolves outside the download dir, or to the download dir itself
	// (name ".", "" — never nuke the whole root).
	if dir == c.cfg.DownloadDir || !pathWithinRoot(c.cfg.DownloadDir, dir) {
		return fmt.Errorf("delete download folder %q: %w", name, errUnsafeDownloadPath)
	}
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("delete download folder %s: %w", name, core.ErrRemoteNotFound)
		}
		return fmt.Errorf("stat download folder %s: %w", dir, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete download folder %s: %w", dir, err)
	}
	return nil
}

// setTransferErrored marks tr TransferErrored under its lock, for the
// several runDownload exit paths (and Enqueue's own lifecycle-stopping
// fallback) that fail before ever reaching a file transfer.
func setTransferErrored(tr *transfer, failure string, retryable bool) {
	// Log before locking: username/filename are immutable and failure is the
	// argument, so no lock is needed, and the reason is otherwise only reachable
	// via a ListDownloads field the caller discards - invisible when a download
	// dies before ever streaming a byte.
	if tr.logger != nil {
		tr.logger.Info("download errored", "username", tr.username, "filename", tr.filename, "reason", failure, "retryable", retryable)
	}
	tr.mu.Lock()
	tr.state = core.TransferErrored
	tr.failure = failure
	tr.retryable = retryable
	tr.mu.Unlock()
}

// finishInterrupted handles runDownload waking up on ctx.Done(): if Cancel
// already marked tr TransferCancelled (see Client.Cancel's comment on why
// that write is guaranteed visible here), leave it alone; otherwise
// ctx.Done() fired because the client's own lifecycle is shutting down,
// which is reported as an errored, retryable transfer rather than silently
// abandoned, so a caller polling ListDownloads still sees a deliberate
// outcome instead of a transfer stuck QUEUED/IN_PROGRESS forever.
func finishInterrupted(tr *transfer, err error) {
	tr.mu.Lock()
	if tr.state == core.TransferCancelled {
		tr.mu.Unlock()
		return
	}
	tr.state = core.TransferErrored
	tr.failure = "download interrupted: " + err.Error()
	tr.retryable = true
	failure := tr.failure
	tr.mu.Unlock()
	if tr.logger != nil {
		tr.logger.Info("download errored", "username", tr.username, "filename", tr.filename, "reason", failure, "retryable", true)
	}
}

// downloadPeerMessage constrains trySendPeerMessage to the P messages the
// download orchestration sends to a peer, mirroring the serverMessage[M]
// pattern in client.go.
type downloadPeerMessage[M any] interface {
	*peer.QueueUpload | *peer.TransferResponse | *peer.PlaceInQueueRequest
	Serialize(M) ([]byte, error)
}

// trySendPeerMessage serializes msg and enqueues it on session's write queue
// without blocking - Serialize packs the size+code+body itself, the same
// pattern tree.go's sendChildMetadata uses for outbound P/D frames. It
// reports false on either a serialization error or TrySend's own
// backpressure/closed-session refusal; the caller decides whether that is
// worth retrying.
func trySendPeerMessage[M downloadPeerMessage[M]](session *peerSession, msg M) bool {
	frame, err := msg.Serialize(msg)
	if err != nil {
		return false
	}
	return session.TrySend(frame)
}

// sendTransferAccept tells the peer we accept its upload (a TransferResponse
// with Allowed set) so it opens the file connection back to us. Like the
// QueueUpload send, it re-establishes the shared P session and retries once if
// the first attempt is refused by backpressure or a just-closed session, and
// reports whether the accept ultimately reached a session's write queue -
// runDownload treats a failure as a retryable error rather than silently
// waiting for a file connection the peer was never asked to open.
func (c *Client) sendTransferAccept(ctx context.Context, tr *transfer, token soul.Token, session *peerSession) bool {
	if trySendPeerMessage(session, &peer.TransferResponse{Token: token, Allowed: true}) {
		return true
	}
	session, err := c.getOrConnectPeerSession(ctx, tr.username)
	if err != nil {
		return false
	}
	return trySendPeerMessage(session, &peer.TransferResponse{Token: token, Allowed: true})
}

// runDownload drives one transfer through the Soulseek download protocol
// dance from QUEUED to a terminal state: it asks the peer to queue our
// download (QueueUpload), waits for their TransferRequest - polling
// PlaceInQueueRequest for a queue-position update in the meantime, and
// re-establishing the shared P session if it drops while queued, since a
// peer holding us queued does not keep any particular session alive on our
// behalf, see getOrConnectPeerSession - accepts the request
// (TransferResponse), waits for the resulting F connection (delivered by
// handleInboundFileConn via tr.fileConnCh), and finally streams the file to
// disk with streamFile. It is started exactly once per transfer, by Enqueue,
// and owns every write to tr's state from that point until it returns.
func (c *Client) runDownload(ctx context.Context, tr *transfer) {
	session, err := c.getOrConnectPeerSession(ctx, tr.username)
	if err != nil {
		setTransferErrored(tr, "peer offline: "+err.Error(), true)
		return
	}
	if !trySendPeerMessage(session, &peer.QueueUpload{Filename: tr.filename}) {
		session, err = c.getOrConnectPeerSession(ctx, tr.username)
		if err != nil || !trySendPeerMessage(session, &peer.QueueUpload{Filename: tr.filename}) {
			setTransferErrored(tr, "queue upload request could not be delivered to the peer", true)
			return
		}
	}

	poll := time.NewTicker(c.cfg.placeInQueueInterval)
	defer poll.Stop()
	queueDeadline := time.NewTimer(c.cfg.downloadQueueTimeout)
	defer queueDeadline.Stop()

	var req peer.TransferRequest
queueWait:
	for {
		select {
		case req = <-tr.transferRequestCh:
			break queueWait
		case f := <-tr.failCh:
			setTransferErrored(tr, f.reason, f.retryable)
			return
		case <-poll.C:
			if s, err := c.getOrConnectPeerSession(ctx, tr.username); err == nil {
				trySendPeerMessage(s, &peer.PlaceInQueueRequest{Filename: tr.filename})
			}
		case <-ctx.Done():
			finishInterrupted(tr, ctx.Err())
			return
		case <-queueDeadline.C:
			setTransferErrored(tr, "timed out waiting for the peer to start the transfer", true)
			return
		}
	}

	// NEGOTIATE: the peer is ready to upload. Accept the transfer and wait for
	// the F connection it opens back to us.
	session, err = c.getOrConnectPeerSession(ctx, tr.username)
	if err != nil {
		setTransferErrored(tr, "peer offline: "+err.Error(), true)
		return
	}

	// Announce we are ready to receive the F connection before sending the
	// accept, so a peer that opens it immediately is delivered to us rather
	// than refused (and closed) as unowned by attachFileConn.
	tr.mu.Lock()
	tr.awaitingFileConn = true
	tr.mu.Unlock()

	if !c.sendTransferAccept(ctx, tr, req.Token, session) {
		closeLeftoverFileConn(tr.stopAwaitingFileConn())
		setTransferErrored(tr, "transfer acceptance could not be delivered to the peer", true)
		return
	}

	// The peer's TransferRequest is authoritative about how many bytes it will
	// send; trust it over the size the caller enqueued (normally identical) so
	// a mismatch cannot truncate the file (io.CopyN stopping early, then
	// renaming a short .part to Completed) or hang it until the idle timeout.
	streamSize := tr.size
	if req.FileSize > 0 {
		if req.FileSize > math.MaxInt64 {
			// req.FileSize is a peer-controlled uint64. A value ≥ 2^63 wraps
			// negative when cast to int64, and io.CopyN(dst, src, negative) is a
			// no-op returning (0, nil) — streamFile would then rename an empty
			// .part to the destination and report the download Completed, while
			// also discarding any resumable partial. Reject it as a protocol
			// violation (non-retryable) rather than trust the declared size.
			//
			// This return is past sendTransferAccept, so the peer has been told
			// to open the F connection: run the same handoff teardown every other
			// give-up path below uses, or a leftover F connection would sit unread
			// in fileConnCh and leak its socket and inbound lease.
			closeLeftoverFileConn(tr.stopAwaitingFileConn())
			setTransferErrored(tr, "peer declared an out-of-range file size", false)
			return
		}
		streamSize = int64(req.FileSize)
		tr.mu.Lock()
		tr.size = streamSize
		tr.mu.Unlock()
	}

	var handoff fileConnHandoff
	select {
	case handoff = <-tr.fileConnCh:
		// Clear the awaiting flag through the same serialised-under-mu path the
		// give-up branches use, closing any second (protocol-violating) F
		// connection a misbehaving peer raced in, so no late delivery can slip
		// into a channel this goroutine has stopped reading.
		closeLeftoverFileConn(tr.stopAwaitingFileConn())
	case f := <-tr.failCh:
		closeLeftoverFileConn(tr.stopAwaitingFileConn())
		setTransferErrored(tr, f.reason, f.retryable)
		return
	case <-ctx.Done():
		closeLeftoverFileConn(tr.stopAwaitingFileConn())
		finishInterrupted(tr, ctx.Err())
		return
	case <-time.After(c.cfg.downloadNegotiationTimeout):
		closeLeftoverFileConn(tr.stopAwaitingFileConn())
		setTransferErrored(tr, "timed out waiting for the peer's file connection", true)
		return
	}
	defer handoff.lease.Release()
	defer handoff.conn.Close()

	tr.mu.Lock()
	if tr.state == core.TransferCancelled {
		// Cancel won the race against the F-connection handoff (the select
		// above picks arbitrarily among ready cases): do not resurrect the
		// transfer to IN_PROGRESS. The deferred conn.Close and lease.Release
		// clean up the handoff.
		tr.mu.Unlock()
		return
	}
	tr.state = core.TransferInProgress
	tr.mu.Unlock()

	destPath, err := safeDownloadDest(c.cfg.DownloadDir, tr.filename)
	if err != nil {
		// Defense in depth: Enqueue already rejects escaping filenames, so a
		// registered transfer should never reach here with one. Fail closed
		// (non-retryable) rather than write outside the download root.
		setTransferErrored(tr, err.Error(), false)
		return
	}
	// streamFile blocks in conn reads and file writes that no context can
	// interrupt; closing the F connection is what unblocks them. Watch ctx for
	// the duration of the stream so Cancel/Remove (via tr.cancel) and
	// lifecycle shutdown abort an in-flight stream immediately - freeing the
	// socket and its inbound lease - instead of streaming on until completion
	// or the idle timeout.
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = handoff.conn.Close()
		case <-watchDone:
		}
	}()
	lastBytes, lastTime := int64(0), time.Now()
	firstSample := true
	written, err := streamFile(handoff.conn, destPath, streamSize, c.cfg.fileIdleTimeout, func(n int64) {
		tr.bytesDone.Store(n)
		now := time.Now()
		if d := now.Sub(lastTime); d >= time.Second {
			speed := int64(float64(n-lastBytes) / d.Seconds())
			tr.mu.Lock()
			tr.speed = speed
			if firstSample {
				tr.speedAvg = speed
			} else {
				tr.speedAvg = ewma(tr.speedAvg, speed, speedEWMAAlpha)
			}
			tr.speedAt = now
			tr.mu.Unlock()
			firstSample = false
			lastBytes, lastTime = n, now
		}
	})
	close(watchDone)
	if err != nil {
		if ctx.Err() != nil {
			// The watcher closed the connection under the stream: report
			// through the same path as the other ctx.Done branches, which
			// leaves a Cancel-set TransferCancelled state alone.
			finishInterrupted(tr, ctx.Err())
			return
		}
		// Peer/network failures are worth retrying; local filesystem failures
		// (diskError) are not — they persist until the operator fixes them.
		var de *diskError
		setTransferErrored(tr, err.Error(), !errors.As(err, &de))
		return
	}

	tr.bytesDone.Store(written)
	tr.mu.Lock()
	if tr.state != core.TransferCancelled {
		// A Cancel that arrived in the stream's final instants keeps its
		// terminal state: Cancel's contract is that the first terminal write
		// wins, even though the fully streamed file was renamed into place.
		tr.state = core.TransferCompleted
	}
	tr.mu.Unlock()
}
