package soulseek

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/hyzr-dev/slusk/internal/soulseek/soul"
	"github.com/hyzr-dev/slusk/internal/soulseek/soul/file"
	"github.com/hyzr-dev/slusk/internal/soulseek/soul/peer"
)

// errUploadTooSlow is returned by streamUploadConn when a peer sustains a
// throughput below Config.uploadMinThroughput for two consecutive sample
// windows, so a trickle-reading peer cannot occupy an upload slot
// indefinitely (see #108).
var errUploadTooSlow = errors.New("soulseek: upload aborted, peer below minimum throughput")

// Upload detail strings recorded alongside an UploadRecord's status. They are
// fixed values on purpose: an upload error's text routinely carries a local
// filesystem path (*os.PathError from opening or seeking the shared file) and
// UploadRecord.Detail is served over the API. The raw error keeps going to the
// log, which is not.
const (
	uploadDetailFileUnavailable   = "file unavailable"
	uploadDetailPeerUnreachable   = "peer session unavailable"
	uploadDetailRequestNotSent    = "transfer request not sent"
	uploadDetailPeerDeclined      = "peer declined"
	uploadDetailNegotiationTimout = "negotiation timeout"
	uploadDetailClientStopping    = "client stopping"
	uploadDetailTooSlow           = "below minimum throughput"
	uploadDetailTransferFailed    = "transfer failed"
)

func (c *Client) runUpload(ctx context.Context, job *uploadJob) {
	startedAt := time.Now()
	// rejected records an outcome where not a single body byte was streamed,
	// so BytesSent and AvgBytesPerSecond are a truthful zero rather than an
	// unmeasured one (see core.UploadRejected).
	rejected := func(detail string) {
		c.recordUpload(ctx, UploadRecord{
			Username:   job.key.username,
			Filename:   job.key.filename,
			Size:       job.size.Load(),
			StartedAt:  startedAt,
			FinishedAt: time.Now(),
			Status:     core.UploadRejected,
			Detail:     detail,
		})
	}

	indexed := c.shareSnapshot().files[job.key.filename]
	if indexed == nil {
		c.denyQueuedUpload(ctx, job, peer.ErrFileNotShared)
		rejected(uploadDetailFileUnavailable)
		return
	}
	// Store size before the file is even opened, so job.size is always
	// non-zero before job.sent can be (a UploadReport reader never sees
	// "sent bytes, but size still 0").
	job.size.Store(indexed.wire.Size)
	shared, err := openIndexedFile(indexed)
	if err != nil {
		c.denyQueuedUpload(ctx, job, peer.ErrFileReadError)
		rejected(uploadDetailFileUnavailable)
		return
	}
	defer shared.Close()

	session, err := c.getOrConnectPeerSession(ctx, job.key.username)
	if err != nil {
		rejected(uploadDetailPeerUnreachable)
		return
	}
	reservation := c.tokens.Reserve()
	defer reservation.Release()
	responses := make(chan peer.TransferResponse, 1)
	c.uploads.registerToken(reservation.token, job.key.username, responses)
	defer c.uploads.unregisterToken(reservation.token, responses)

	request := &peer.TransferRequest{Direction: peer.UploadToPeer, Token: reservation.token, Filename: job.key.filename, FileSize: indexed.wire.Size}
	if !sendUploadPeerMessage(session, request) {
		rejected(uploadDetailRequestNotSent)
		return
	}

	timer := time.NewTimer(c.cfg.uploadNegotiationTimeout)
	defer timer.Stop()
	select {
	case response := <-responses:
		if !response.Allowed {
			rejected(uploadDetailPeerDeclined)
			return
		}
	case <-timer.C:
		rejected(uploadDetailNegotiationTimout)
		return
	case <-ctx.Done():
		rejected(uploadDetailClientStopping)
		return
	}

	// startOffset is where the peer asked the transfer to resume, filled in by
	// streamUploadConn. job.sent is absolute and starts there, so only the
	// delta is this attempt's contribution — recording job.sent directly would
	// silently overstate a resumed upload's volume and speed.
	var startOffset atomic.Uint64
	streamStarted := time.Now()
	err = c.streamUpload(ctx, job.key.username, reservation.token, shared, indexed.wire.Size, &job.sent, &startOffset)
	streamDuration := time.Since(streamStarted)

	status, detail := core.UploadCompleted, ""
	if err != nil {
		status = core.UploadAborted
		detail = uploadDetailTransferFailed
		_ = sendUploadPeerMessage(session, &peer.UploadFailed{Filename: job.key.filename})
		if errors.Is(err, errUploadTooSlow) {
			detail = uploadDetailTooSlow
			if c.logger != nil {
				c.logger.Info("soulseek upload aborted: below minimum throughput", "username", job.key.username, "filename", job.key.filename)
			}
		} else if c.logger != nil {
			c.logger.Debug("soulseek upload failed", "username", job.key.username, "filename", job.key.filename, "err", err)
		}
	}
	c.recordUpload(ctx, UploadRecord{
		Username:          job.key.username,
		Filename:          job.key.filename,
		Size:              job.size.Load(),
		BytesSent:         uploadBytesSent(job.sent.Load(), startOffset.Load()),
		AvgBytesPerSecond: uploadAvgBytesPerSecond(uploadBytesSent(job.sent.Load(), startOffset.Load()), streamDuration),
		StartedAt:         startedAt,
		FinishedAt:        time.Now(),
		Status:            status,
		Detail:            detail,
	})
}

// uploadBytesSent is this attempt's contribution: the absolute progress
// counter minus the resume offset it was seeded with. It saturates at 0 rather
// than underflowing, since both values are read without a lock and a
// still-settling write could in principle order them the wrong way round — an
// unsigned wrap here would turn a tiny discrepancy into an exabyte.
func uploadBytesSent(sent, startOffset uint64) uint64 {
	if sent < startOffset {
		return 0
	}
	return sent - startOffset
}

// uploadAvgBytesPerSecond is the streaming phase's own average rate. A
// non-positive duration yields 0 rather than dividing: a transfer that failed
// before it began has no rate to report, and 0 is how core.UploadRejected
// already spells "no measurement".
func uploadAvgBytesPerSecond(bytesSent uint64, d time.Duration) uint64 {
	if d <= 0 || bytesSent == 0 {
		return 0
	}
	return uint64(float64(bytesSent) / d.Seconds())
}

func (c *Client) denyQueuedUpload(ctx context.Context, job *uploadJob, reason error) {
	session, err := c.getOrConnectPeerSession(ctx, job.key.username)
	if err == nil {
		_ = sendUploadPeerMessage(session, &peer.UploadDenied{Filename: job.key.filename, Reason: reason})
	}
}

func openIndexedFile(indexed *indexedFile) (*os.File, error) {
	resolvedBefore, err := filepath.EvalSymlinks(indexed.local)
	if err != nil {
		return nil, fmt.Errorf("resolve shared file before opening: %w", err)
	}
	resolvedBefore, err = filepath.Abs(resolvedBefore)
	if err != nil || !pathWithinRoot(indexed.root, resolvedBefore) {
		return nil, fmt.Errorf("shared file escaped resolved root")
	}
	before, err := os.Stat(resolvedBefore)
	if err != nil || !before.Mode().IsRegular() || before.Size() != int64(indexed.wire.Size) ||
		before.ModTime().UnixMicro() != indexed.modTime.UnixMicro() {
		return nil, fmt.Errorf("shared file changed since scan")
	}
	// os.SameFile catches a replacement that kept the size and mtime, but it
	// needs the os.FileInfo the walk produced - which an entry restored from a
	// persisted index does not have, since that path never touched the
	// filesystem (issue #497). Size and mtime are the same pair share_file_meta
	// data has always trusted to mean "unchanged", and every other check here,
	// including the SameFile checks across the open below, still applies.
	//
	// Compared on UnixMicro rather than time.Equal because a restored mtime has
	// been through Postgres and lost sub-microsecond precision; comparing at
	// full resolution would refuse every upload of a restored entry.
	if indexed.info != nil && !os.SameFile(indexed.info, before) {
		return nil, fmt.Errorf("shared file changed since scan")
	}
	f, err := os.Open(indexed.local)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() != before.Size() {
		f.Close()
		return nil, fmt.Errorf("shared file changed while opening")
	}
	resolvedAfter, err := filepath.EvalSymlinks(indexed.local)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("resolve shared file after opening: %w", err)
	}
	resolvedAfter, err = filepath.Abs(resolvedAfter)
	if err != nil || !pathWithinRoot(indexed.root, resolvedAfter) {
		f.Close()
		return nil, fmt.Errorf("shared file escaped resolved root while opening")
	}
	after, err := os.Stat(resolvedAfter)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		f.Close()
		return nil, fmt.Errorf("shared file path changed while opening")
	}
	return f, nil
}

// streamUpload opens the file connection and streams the upload. startOffset
// receives the resume offset the peer requested, which the caller needs to
// turn the absolute sent counter into this attempt's own byte delta.
func (c *Client) streamUpload(ctx context.Context, username string, token soul.Token, shared *os.File, size uint64, sent, startOffset *atomic.Uint64) error {
	conn, err := c.ConnectPeer(ctx, username, file.ConnectionType)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	return streamUploadConn(conn, token, shared, size, c.cfg.fileInitTimeout, c.cfg.fileIdleTimeout, c.cfg.uploadMinThroughput, c.cfg.uploadThroughputSampleInterval, sent, startOffset, &c.uploads.totalWritten)
}

// uploadThroughputStrikeLimit is how many consecutive sub-floor sample
// windows a streaming upload tolerates before it is aborted as too slow
// (see #108). The first window is always skipped as grace so a peer's
// initial read latency never counts against it.
const uploadThroughputStrikeLimit = 2

func streamUploadConn(conn net.Conn, token soul.Token, shared io.ReadSeeker, size uint64, initTimeout, idleTimeout time.Duration, minThroughput int, sampleInterval time.Duration, sent, startOffset, totalWritten *atomic.Uint64) error {
	if sent == nil {
		sent = new(atomic.Uint64)
	}
	if startOffset == nil {
		startOffset = new(atomic.Uint64)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(idleTimeout)); err != nil {
		return err
	}
	if _, err := file.Write(conn, &file.TransferInit{Token: token}); err != nil {
		return fmt.Errorf("write transfer init: %w", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(initTimeout)); err != nil {
		return err
	}
	offset := &file.Offset{}
	if err := offset.Deserialize(conn); err != nil {
		return fmt.Errorf("read upload offset: %w", err)
	}
	if offset.Offset > size {
		return fmt.Errorf("invalid upload offset %d greater than size %d", offset.Offset, size)
	}
	// The peer's requested offset is where the transfer actually starts, so
	// record it before any body bytes are sent. Store, not Add: exactly one
	// attempt streams a given uploadJob (a retry gets a fresh job from a new
	// enqueue), so there is no prior value to add to. Without this, a
	// resumed upload would permanently under-report: streamUploadBody below
	// is only given size-offset.Offset to send and counts only what it
	// writes, so e.g. a 100MB file resumed at 90MB would finish reporting
	// 10%; and when offset.Offset == size the body is skipped entirely,
	// which would leave sent at 0 forever.
	sent.Store(offset.Offset)
	// Same value, separate counter: sent keeps growing as the body streams,
	// so the caller cannot recover the offset from it afterwards, and it needs
	// exactly that to report the delta this attempt actually sent (#325).
	startOffset.Store(offset.Offset)
	if offset.Offset < size {
		if _, err := shared.Seek(int64(offset.Offset), io.SeekStart); err != nil {
			return err
		}
		if err := streamUploadBody(conn, shared, size-offset.Offset, idleTimeout, minThroughput, sampleInterval, sent, totalWritten); err != nil {
			return err
		}
	}
	// The downloader owns successful F-connection completion. Keep the socket
	// open after the exact byte count and wait for it to close, bounded by the
	// same idle timeout used for transfer progress.
	if err := conn.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
		if err == io.ErrClosedPipe {
			return nil
		}
		return err
	}
	var unexpected [1]byte
	n, err := conn.Read(unexpected[:])
	if (err == io.EOF || err == io.ErrClosedPipe) && n == 0 {
		return nil
	}
	if err != nil {
		return fmt.Errorf("wait for downloader to close completed upload: %w", err)
	}
	return fmt.Errorf("unexpected data after completed upload")
}

// streamUploadBody copies the remaining n bytes from shared to conn. When
// minThroughput and sampleInterval are both positive it also runs a
// throughput sampler (see uploadThroughputSampler) for the duration of the
// copy only - never during the post-transfer wait for the downloader to
// close the connection - aborting the connection and returning
// errUploadTooSlow if the peer sustains a throughput below minThroughput
// for two consecutive sample windows (#108).
func streamUploadBody(conn net.Conn, shared io.Reader, n uint64, idleTimeout time.Duration, minThroughput int, sampleInterval time.Duration, sent, totalWritten *atomic.Uint64) error {
	if sent == nil {
		sent = new(atomic.Uint64)
	}
	writer := &progressWriter{conn: conn, idleTimeout: idleTimeout, written: sent, totalWritten: totalWritten}

	var abortedSlow atomic.Bool
	if minThroughput > 0 && sampleInterval > 0 {
		done := make(chan struct{})
		defer close(done)
		go uploadThroughputSampler(writer, conn, minThroughput, sampleInterval, &abortedSlow, done)
	}

	written, err := io.CopyN(writer, shared, int64(n))
	if err != nil {
		if abortedSlow.Load() {
			return fmt.Errorf("stream upload (sent %d of %d): %w", written, n, errUploadTooSlow)
		}
		return fmt.Errorf("stream upload (sent %d of %d): %w", written, n, err)
	}
	return nil
}

// uploadThroughputSampler polls writer's cumulative byte count once per
// sampleInterval and, once it has seen two consecutive intervals whose
// delta falls below minThroughput * sampleInterval, sets abortedSlow and
// closes conn to unblock the upload's blocked Write (#108). The very first
// interval is always skipped as grace, since it may include time spent
// before the peer starts reading in earnest. It returns as soon as done is
// closed, so it never outlives the transfer it is watching.
//
// writer.written is now the job's shared *atomic.Uint64 progress counter
// (also read by UploadReport), and on a resumed transfer it may already
// start above 0 (streamUploadConn stores the peer's requested offset before
// the body copy begins) - the sampler only cares about the delta between
// consecutive polls, so that starting value does not affect its throughput
// math.
func uploadThroughputSampler(writer *progressWriter, conn net.Conn, minThroughput int, sampleInterval time.Duration, abortedSlow *atomic.Bool, done <-chan struct{}) {
	ticker := time.NewTicker(sampleInterval)
	defer ticker.Stop()

	threshold := uint64(float64(minThroughput) * sampleInterval.Seconds())
	var last uint64
	strikes := 0
	first := true
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			// select picks randomly among ready cases, so a buffered tick
			// can still win here even after streamUploadBody's copy has
			// finished and defer close(done) has run - right as the caller
			// enters the post-transfer wait-for-downloader-close Read. On a
			// real net.TCPConn that stray conn.Close() turns a successful
			// upload into a "wait for downloader to close" error, wrongly
			// reporting a fully-delivered transfer as failed (net.Pipe
			// masks this: it returns io.ErrClosedPipe there, which is
			// treated as success, so pipe-based tests can't catch it).
			// Re-check done here, non-blocking, so once done is closed this
			// branch can never call conn.Close() again.
			select {
			case <-done:
				return
			default:
			}
			cur := writer.written.Load()
			if !first && cur-last < threshold {
				strikes++
			} else {
				strikes = 0
			}
			first = false
			last = cur
			if strikes >= uploadThroughputStrikeLimit {
				abortedSlow.Store(true)
				_ = conn.Close()
				return
			}
		}
	}
}

type progressWriter struct {
	conn         net.Conn
	idleTimeout  time.Duration
	written      *atomic.Uint64
	totalWritten *atomic.Uint64
}

func (w *progressWriter) Write(p []byte) (int, error) {
	if err := w.conn.SetWriteDeadline(time.Now().Add(w.idleTimeout)); err != nil {
		return 0, err
	}
	n, err := w.conn.Write(p)
	if n > 0 {
		w.written.Add(uint64(n))
		if w.totalWritten != nil {
			w.totalWritten.Add(uint64(n))
		}
	}
	return n, err
}
