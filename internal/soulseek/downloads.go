package soulseek

import (
	"context"
	"net"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
)

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
// produces and pipeline.AlbumFolder expects.
func downloadDestPath(completeDir, filename string) string {
	base := path.Base(strings.ReplaceAll(filename, `\`, "/"))
	if leaf := destLeaf(filename); leaf != "" {
		return filepath.Join(completeDir, leaf, base)
	}
	return filepath.Join(completeDir, base)
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
	size     int64
	// token is the token from the peer's TransferRequest, echoed back to us
	// unchanged in the F connection's TransferInit. It is the zero value
	// until the download hook (Group E) observes a TransferRequest and calls
	// downloadRegistry.registerToken; guarded by mu like the other fields a
	// concurrent ListDownloads snapshot reads, even though it is declared
	// above mu here to keep the wire-correlation fields grouped together.
	token soul.Token

	mu            sync.Mutex // guards state, failure, retryable, queuePosition, speed, token
	state         core.TransferState
	failure       string
	retryable     bool
	queuePosition uint32
	speed         int64

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

// attachFileConn delivers an established F connection to whichever
// orchestration goroutine is waiting on tr.fileConnCh, without blocking. It
// reports whether the delivery landed: false means nothing was waiting -
// fileConnCh's single buffer slot was already full, or nobody has read from
// it since a prior delivery - in which case the caller (handleInboundFileConn)
// must close conn and release lease itself, since ownership was never handed
// off.
func (tr *transfer) attachFileConn(conn net.Conn, lease *inboundLease) bool {
	select {
	case tr.fileConnCh <- fileConnHandoff{conn: conn, lease: lease}:
		return true
	default:
		return false
	}
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
	tr.token = token
	tr.mu.Unlock()

	r.mu.Lock()
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
