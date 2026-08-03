package soulseek

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/hyzr-dev/slusk/internal/soulseek/soul"
	"github.com/hyzr-dev/slusk/internal/soulseek/soul/peer"
)

const (
	maxWaitingUploads        = 1024
	maxWaitingUploadsPerUser = 64
	// maxReportedUploads caps how many waiting uploads UploadReport includes,
	// so a large queue can never make the dashboard's report expensive to
	// build or serve. Active uploads are never truncated (see report).
	maxReportedUploads = 100
)

type uploadKey struct {
	username string
	filename string
}

// UploadRecord is one finished upload handed to an UploadSink (issue #325).
// It is produced once the transfer's outcome is decided, never while bytes are
// moving.
//
// Filename is the normalized virtual share path, exactly as UploadEntry
// reports it — already visible to any peer that browses us, unlike the local
// path, which must never appear here or in Detail.
//
// Detail is a short fixed reason string, deliberately not err.Error(): upload
// failures routinely wrap *os.PathError, and this value is served over the API
// (the same reason registerShares refuses to echo a rescan error). The raw
// error is logged instead.
//
// BytesSent is what this attempt put on the wire, i.e. the delta above the
// resume offset the peer asked for — not uploadJob.sent, which is absolute.
// AvgBytesPerSecond divides that by the streaming phase's own duration, so it
// is not derivable from FinishedAt-StartedAt, which also covers the time the
// job spent queued and negotiating.
type UploadRecord struct {
	Username          string
	Filename          string
	Size              uint64
	BytesSent         uint64
	AvgBytesPerSecond uint64
	StartedAt         time.Time
	FinishedAt        time.Time
	Status            core.UploadStatus
	Detail            string
}

// UploadSink receives finished uploads so they survive a restart. Unlike
// MessageSink nothing depends on the write succeeding — the peer has already
// been served either way — so an error is logged and discarded rather than
// retried, and the sink must never be given work that could stall the
// protocol.
//
// Implementations are called from the upload's own goroutine after the
// transfer is over, so a slow write delays releasing that upload slot (bounded
// by uploadSinkTimeout) but never touches the streaming hot path.
type UploadSink interface {
	RecordUpload(ctx context.Context, r UploadRecord) error
}

// uploadSinkTimeout bounds a single sink write. It is shorter than
// messageSinkTimeout because the write holds an upload slot for its duration
// while nothing depends on its outcome, whereas a message write gates an ack.
const uploadSinkTimeout = 5 * time.Second

// recordUpload hands r to the configured UploadSink, if any. A nil sink is a
// no-op, matching MessageSink and ShareMetaCache.
//
// The write gets a fresh bounded context derived with WithoutCancel: by the
// time an upload's outcome is known its ctx is frequently already cancelled
// (shutdown, or the peer connection going away is what ended the transfer),
// and reusing it would make every interesting row fail to persist. Same
// problem, same fix as runMessageWorker's persistCtx.
func (c *Client) recordUpload(ctx context.Context, r UploadRecord) {
	if c.cfg.UploadSink == nil {
		return
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), uploadSinkTimeout)
	defer cancel()
	if err := c.cfg.UploadSink.RecordUpload(persistCtx, r); err != nil && c.logger != nil {
		c.logger.Error("upload history sink failed", "username", r.Username, "filename", r.Filename, "err", err)
	}
}

// uploadJob is only ever handled through a *uploadJob (see byKey/waiting),
// never copied by value, so its embedded atomics are safe. That is load
// bearing: size/sent are updated from the per-Write streaming hot path
// (progressWriter.Write via streamUpload), which must never acquire m.mu, so
// these fields must stay plain atomics rather than being "simplified" into
// mutex-guarded ints - that would put a lock acquisition on every upload
// Write.
type uploadJob struct {
	key    uploadKey
	active bool
	// seq is the job's enqueue order, assigned under m.mu in enqueue. It
	// gives report a stable ordering for active uploads (which are not kept
	// in a slice like waiting is).
	seq uint64
	// size is the file's full size; 0 until runUpload resolves the share
	// entry and stores it, just before opening the file.
	size atomic.Uint64
	// sent is the absolute number of bytes delivered to the peer so far,
	// including any resume offset the peer requested. See streamUploadConn.
	sent atomic.Uint64
}

type uploadResponseWaiter struct {
	username  string
	responses chan peer.TransferResponse
}

type uploadManager struct {
	mu      sync.Mutex
	c       *Client
	execute func(context.Context, *uploadJob)
	slots   int
	active  int
	waiting []*uploadJob
	byKey   map[uploadKey]*uploadJob
	byToken map[soul.Token]uploadResponseWaiter
	perUser map[string]int
	wake    chan struct{}
	// seq is a monotonic counter assigned to each enqueued job's seq field,
	// giving report a stable ordering for active uploads.
	seq uint64
	// totalWritten is manager-lifetime body bytes accepted by upload sockets.
	// It is deliberately independent of per-job resume-aware progress so jobs
	// that complete between throughput ticks cannot disappear from the delta.
	totalWritten atomic.Uint64
}

func newUploadManager(c *Client, slots int) *uploadManager {
	return &uploadManager{c: c, execute: c.runUpload, slots: slots, byKey: make(map[uploadKey]*uploadJob), byToken: make(map[soul.Token]uploadResponseWaiter), perUser: make(map[string]int), wake: make(chan struct{}, 1)}
}

func (m *uploadManager) notify() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *uploadManager) enqueue(username, filename string) error {
	normalized, ok := normalizeVirtualPath(filename)
	if !ok || m.c.shareSnapshot().files[normalized] == nil {
		return peer.ErrFileNotShared
	}
	key := uploadKey{username: username, filename: normalized}
	m.mu.Lock()
	if _, exists := m.byKey[key]; exists {
		m.mu.Unlock()
		return nil
	}
	if len(m.waiting) >= maxWaitingUploads || m.perUser[username] >= maxWaitingUploadsPerUser {
		m.mu.Unlock()
		return peer.ErrTooManyFiles
	}
	m.seq++
	job := &uploadJob{key: key, seq: m.seq}
	m.byKey[key] = job
	m.waiting = append(m.waiting, job)
	m.perUser[username]++
	m.mu.Unlock()
	m.notify()
	return nil
}

func (m *uploadManager) availability() (bool, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active < m.slots && len(m.waiting) == 0, len(m.waiting)
}

// throughputSnapshot returns the manager-lifetime socket-write total and the
// full active upload count. Waiting jobs are deliberately excluded.
func (m *uploadManager) throughputSnapshot() (uint64, int) {
	total := m.totalWritten.Load()
	m.mu.Lock()
	active := m.active
	m.mu.Unlock()
	return total, active
}

func (m *uploadManager) position(key uploadKey) (uint32, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.byKey[key]
	if job == nil {
		return 0, false
	}
	if job.active {
		return 0, true
	}
	for i, waiting := range m.waiting {
		if waiting == job {
			return uint32(i + 1), true
		}
	}
	return 0, false
}

// UploadEntry is one upload in an UploadReport: either currently streaming
// (Active, Position 0) or waiting in the queue (Position is its 1-based
// place, matching position()).
type UploadEntry struct {
	Username     string
	Filename     string
	Active       bool
	Position     uint32 // 1-based queue place; 0 when Active
	Size         uint64
	BytesWritten uint64
}

// UploadReport is a point-in-time snapshot of the upload manager's state,
// for the dashboard's upload-activity view. See uploadManager.report and
// Client.UploadReport.
type UploadReport struct {
	Slots     int
	Active    int
	Queued    int // true waiting count, regardless of truncation
	Truncated int // waiting uploads omitted from Uploads
	Uploads   []UploadEntry
}

// report builds an UploadReport under one m.mu critical section: no I/O, no
// blocking behind a transfer, and every field is copied by value so no
// *uploadJob ever escapes the lock. BytesWritten/Size may be a few writes
// stale since they are read via atomic.Load while the transfer keeps
// writing - fine for a UI. limit bounds how many *waiting* uploads are
// included; active uploads are never truncated (they are already bounded by
// m.slots).
//
// After reset() (dispatcher shutdown), the report goes empty while
// in-flight bytes may still move for a moment on an already-returned
// *uploadJob - the same pre-existing semantics shared with
// availability()/position().
func (m *uploadManager) report(limit int) UploadReport {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Sized from m.active rather than len(m.byKey)-len(m.waiting): the two are
	// equal while byKey holds exactly the waiting plus the active jobs, but the
	// subtraction would panic on a negative capacity if a later refactor ever
	// broke that invariant - on a dashboard poll, far from the change itself.
	active := make([]*uploadJob, 0, m.active)
	for _, job := range m.byKey {
		if job.active {
			active = append(active, job)
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].seq < active[j].seq })

	entries := make([]UploadEntry, 0, len(active)+max(0, min(limit, len(m.waiting))))
	for _, job := range active {
		entries = append(entries, UploadEntry{
			Username:     job.key.username,
			Filename:     job.key.filename,
			Active:       true,
			Position:     0,
			Size:         job.size.Load(),
			BytesWritten: job.sent.Load(),
		})
	}
	emitted := 0
	for i, job := range m.waiting {
		if emitted >= limit {
			break
		}
		entries = append(entries, UploadEntry{
			Username:     job.key.username,
			Filename:     job.key.filename,
			Active:       false,
			Position:     uint32(i + 1),
			Size:         job.size.Load(),
			BytesWritten: job.sent.Load(),
		})
		emitted++
	}

	return UploadReport{
		Slots:     m.slots,
		Active:    m.active,
		Queued:    len(m.waiting),
		Truncated: len(m.waiting) - emitted,
		Uploads:   entries,
	}
}

func (m *uploadManager) dispatch(ctx context.Context) {
	defer m.reset()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		}
		for {
			if ctx.Err() != nil {
				return
			}
			m.mu.Lock()
			if m.active >= m.slots || len(m.waiting) == 0 {
				m.mu.Unlock()
				break
			}
			job := m.waiting[0]
			m.waiting = m.waiting[1:]
			job.active = true
			m.active++
			m.mu.Unlock()
			if !m.c.startTracked(func() {
				defer m.complete(job)
				m.execute(ctx, job)
			}) {
				m.complete(job)
				return
			}
		}
	}
}

func (m *uploadManager) reset() {
	m.mu.Lock()
	for _, job := range m.byKey {
		job.active = false
	}
	m.active = 0
	m.waiting = nil
	m.byKey = make(map[uploadKey]*uploadJob)
	m.byToken = make(map[soul.Token]uploadResponseWaiter)
	m.perUser = make(map[string]int)
	m.mu.Unlock()
}

func (m *uploadManager) complete(job *uploadJob) {
	m.mu.Lock()
	if m.byKey[job.key] == job {
		delete(m.byKey, job.key)
		m.perUser[job.key.username]--
		if m.perUser[job.key.username] == 0 {
			delete(m.perUser, job.key.username)
		}
	}
	if job.active {
		job.active = false
		m.active--
	}
	m.mu.Unlock()
	m.notify()
}

func (m *uploadManager) registerToken(token soul.Token, username string, responses chan peer.TransferResponse) {
	m.mu.Lock()
	m.byToken[token] = uploadResponseWaiter{username: username, responses: responses}
	m.mu.Unlock()
}

func (m *uploadManager) unregisterToken(token soul.Token, responses chan peer.TransferResponse) {
	m.mu.Lock()
	if waiter, ok := m.byToken[token]; ok && waiter.responses == responses {
		delete(m.byToken, token)
	}
	m.mu.Unlock()
}

func (m *uploadManager) deliver(username string, response peer.TransferResponse) {
	m.mu.Lock()
	waiter, ok := m.byToken[response.Token]
	m.mu.Unlock()
	if ok && waiter.username == username {
		select {
		case waiter.responses <- response:
		default:
		}
	}
}

type uploadSessionHooks struct {
	c       *Client
	uploads *uploadManager
}

func (*uploadSessionHooks) established(*peerSession)   {}
func (*uploadSessionHooks) closed(*peerSession, error) {}

func (h *uploadSessionHooks) frame(session *peerSession, frame sessionFrame) error {
	if frame.connType != peer.ConnectionType {
		return errUnhandledPeerFrame
	}
	switch peer.Code(frame.code) {
	case peer.CodeTransferRequest:
		msg := &peer.TransferRequest{}
		if err := msg.Deserialize(bytes.NewReader(frame.wire)); err != nil {
			return fmt.Errorf("deserialize upload transfer request: %w", err)
		}
		if msg.Direction != peer.DownloadFromPeer {
			return errUnhandledPeerFrame
		}
		reason := error(peer.ErrQueued)
		if err := h.uploads.enqueue(session.key.username, msg.Filename); err != nil {
			reason = err
		}
		// Soulseek.NET/slskd uses the legacy direction-0 request and waits for
		// this immediate acknowledgement. Queue it even when a slot is free;
		// the dispatcher then sends the modern direction-1 TransferRequest
		// when the slot is committed, reusing one queue implementation.
		if !sendUploadPeerMessage(session, &peer.TransferResponse{Token: msg.Token, Allowed: false, Reason: reason}) {
			return errors.New("legacy upload request response backpressure")
		}
		return nil

	case peer.CodeQueueUpload:
		msg := &peer.QueueUpload{}
		if err := msg.Deserialize(bytes.NewReader(frame.wire)); err != nil {
			return fmt.Errorf("deserialize queue upload: %w", err)
		}
		if err := h.uploads.enqueue(session.key.username, msg.Filename); err != nil {
			if !sendUploadPeerMessage(session, &peer.UploadDenied{Filename: msg.Filename, Reason: err}) {
				return errors.New("upload denial backpressure")
			}
		}
		return nil
	case peer.CodePlaceInQueueRequest:
		msg := &peer.PlaceInQueueRequest{}
		if err := msg.Deserialize(bytes.NewReader(frame.wire)); err != nil {
			return fmt.Errorf("deserialize place in queue request: %w", err)
		}
		normalized, ok := normalizeVirtualPath(msg.Filename)
		if !ok {
			ok = false
		}
		place, found := h.uploads.position(uploadKey{username: session.key.username, filename: normalized})
		if !ok || !found {
			if !sendUploadPeerMessage(session, &peer.UploadDenied{Filename: msg.Filename, Reason: peer.ErrFileNotShared}) {
				return errors.New("upload denial backpressure")
			}
			return nil
		}
		if !sendUploadPeerMessage(session, &peer.PlaceInQueueResponse{Filename: msg.Filename, Place: place}) {
			return errors.New("queue position response backpressure")
		}
		return nil
	case peer.CodeTransferResponse:
		msg := &peer.TransferResponse{}
		if err := msg.Deserialize(bytes.NewReader(frame.wire)); err != nil {
			return fmt.Errorf("deserialize upload transfer response: %w", err)
		}
		h.uploads.deliver(session.key.username, *msg)
		return nil
	default:
		return errUnhandledPeerFrame
	}
}

type uploadPeerMessage[M any] interface {
	*peer.TransferRequest | *peer.TransferResponse | *peer.PlaceInQueueResponse | *peer.UploadDenied | *peer.UploadFailed
	Serialize(M) ([]byte, error)
}

func sendUploadPeerMessage[M uploadPeerMessage[M]](session *peerSession, msg M) bool {
	wire, err := msg.Serialize(msg)
	return err == nil && session.TrySend(wire)
}

// UploadReport returns the current upload manager's state: slot usage,
// active/queued counts, and up to maxReportedUploads waiting entries plus
// every active one. Like ShareReport, it takes uploads' internal mutex only
// for bookkeeping and never blocks behind an in-progress transfer.
//
// The nil check below is defensive only, for a hand-built Client: New always
// assigns c.uploads, and a non-positive UploadSlots is clamped to a default
// rather than disabling the manager. Callers that need to distinguish "native
// Soulseek is off" get that from the client being nil, not from this report.
func (c *Client) UploadReport() UploadReport {
	if c.uploads == nil {
		return UploadReport{}
	}
	return c.uploads.report(maxReportedUploads)
}
