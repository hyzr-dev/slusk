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

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/file"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
)

// errUploadTooSlow is returned by streamUploadConn when a peer sustains a
// throughput below Config.uploadMinThroughput for two consecutive sample
// windows, so a trickle-reading peer cannot occupy an upload slot
// indefinitely (see #108).
var errUploadTooSlow = errors.New("soulseek: upload aborted, peer below minimum throughput")

func (c *Client) runUpload(ctx context.Context, job *uploadJob) {
	indexed := c.shareSnapshot().files[job.key.filename]
	if indexed == nil {
		c.denyQueuedUpload(ctx, job, peer.ErrFileNotShared)
		return
	}
	// Store size before the file is even opened, so job.size is always
	// non-zero before job.sent can be (a UploadReport reader never sees
	// "sent bytes, but size still 0").
	job.size.Store(indexed.wire.Size)
	shared, err := openIndexedFile(indexed)
	if err != nil {
		c.denyQueuedUpload(ctx, job, peer.ErrFileReadError)
		return
	}
	defer shared.Close()

	session, err := c.getOrConnectPeerSession(ctx, job.key.username)
	if err != nil {
		return
	}
	reservation := c.tokens.Reserve()
	defer reservation.Release()
	responses := make(chan peer.TransferResponse, 1)
	c.uploads.registerToken(reservation.token, job.key.username, responses)
	defer c.uploads.unregisterToken(reservation.token, responses)

	request := &peer.TransferRequest{Direction: peer.UploadToPeer, Token: reservation.token, Filename: job.key.filename, FileSize: indexed.wire.Size}
	if !sendUploadPeerMessage(session, request) {
		return
	}

	timer := time.NewTimer(c.cfg.uploadNegotiationTimeout)
	defer timer.Stop()
	select {
	case response := <-responses:
		if !response.Allowed {
			return
		}
	case <-timer.C:
		return
	case <-ctx.Done():
		return
	}

	if err := c.streamUpload(ctx, job.key.username, reservation.token, shared, indexed.wire.Size, &job.sent); err != nil {
		_ = sendUploadPeerMessage(session, &peer.UploadFailed{Filename: job.key.filename})
		if c.logger != nil {
			if errors.Is(err, errUploadTooSlow) {
				c.logger.Info("soulseek upload aborted: below minimum throughput", "username", job.key.username, "filename", job.key.filename)
			} else {
				c.logger.Debug("soulseek upload failed", "username", job.key.username, "filename", job.key.filename, "err", err)
			}
		}
	}
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
	if err != nil || !before.Mode().IsRegular() || !os.SameFile(indexed.info, before) || before.Size() != int64(indexed.wire.Size) || !before.ModTime().Equal(indexed.info.ModTime()) {
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

func (c *Client) streamUpload(ctx context.Context, username string, token soul.Token, shared *os.File, size uint64, sent *atomic.Uint64) error {
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

	return streamUploadConn(conn, token, shared, size, c.cfg.fileInitTimeout, c.cfg.fileIdleTimeout, c.cfg.uploadMinThroughput, c.cfg.uploadThroughputSampleInterval, sent)
}

// uploadThroughputStrikeLimit is how many consecutive sub-floor sample
// windows a streaming upload tolerates before it is aborted as too slow
// (see #108). The first window is always skipped as grace so a peer's
// initial read latency never counts against it.
const uploadThroughputStrikeLimit = 2

func streamUploadConn(conn net.Conn, token soul.Token, shared io.ReadSeeker, size uint64, initTimeout, idleTimeout time.Duration, minThroughput int, sampleInterval time.Duration, sent *atomic.Uint64) error {
	if sent == nil {
		sent = new(atomic.Uint64)
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
	if offset.Offset < size {
		if _, err := shared.Seek(int64(offset.Offset), io.SeekStart); err != nil {
			return err
		}
		if err := streamUploadBody(conn, shared, size-offset.Offset, idleTimeout, minThroughput, sampleInterval, sent); err != nil {
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
func streamUploadBody(conn net.Conn, shared io.Reader, n uint64, idleTimeout time.Duration, minThroughput int, sampleInterval time.Duration, sent *atomic.Uint64) error {
	if sent == nil {
		sent = new(atomic.Uint64)
	}
	writer := &progressWriter{conn: conn, idleTimeout: idleTimeout, written: sent}

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
	conn        net.Conn
	idleTimeout time.Duration
	written     *atomic.Uint64
}

func (w *progressWriter) Write(p []byte) (int, error) {
	if err := w.conn.SetWriteDeadline(time.Now().Add(w.idleTimeout)); err != nil {
		return 0, err
	}
	n, err := w.conn.Write(p)
	w.written.Add(uint64(n))
	return n, err
}
