package soulseek

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samuelenocsson/slusk/internal/soulseek/soul"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul/file"
)

// readSeeker hides bytes.Reader's WriteTo method so io.CopyN always drives
// streamUploadConn's progressWriter through its ordinary Write path, keeping
// these tests independent of io.Copy's WriterTo fast-path optimizations.
type readSeeker struct{ r *bytes.Reader }

func (rs *readSeeker) Read(p []byte) (int, error)                   { return rs.r.Read(p) }
func (rs *readSeeker) Seek(offset int64, whence int) (int64, error) { return rs.r.Seek(offset, whence) }

type fixedWriteConn struct {
	n   int
	err error
}

func (*fixedWriteConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *fixedWriteConn) Write(p []byte) (int, error)    { return min(c.n, len(p)), c.err }
func (*fixedWriteConn) Close() error                     { return nil }
func (*fixedWriteConn) LocalAddr() net.Addr              { return nil }
func (*fixedWriteConn) RemoteAddr() net.Addr             { return nil }
func (*fixedWriteConn) SetDeadline(time.Time) error      { return nil }
func (*fixedWriteConn) SetReadDeadline(time.Time) error  { return nil }
func (*fixedWriteConn) SetWriteDeadline(time.Time) error { return nil }

func TestProgressWriterAggregatesActualSocketWriteCounts(t *testing.T) {
	total := &atomic.Uint64{}
	firstJob := &atomic.Uint64{}
	secondJob := &atomic.Uint64{}
	first := &progressWriter{conn: &fixedWriteConn{n: 4}, idleTimeout: time.Second, written: firstJob, totalWritten: total}
	second := &progressWriter{conn: &fixedWriteConn{n: 6}, idleTimeout: time.Second, written: secondJob, totalWritten: total}

	if n, err := first.Write(make([]byte, 4)); err != nil || n != 4 {
		t.Fatalf("first Write = %d, %v", n, err)
	}
	if n, err := second.Write(make([]byte, 6)); err != nil || n != 6 {
		t.Fatalf("second Write = %d, %v", n, err)
	}
	if firstJob.Load() != 4 || secondJob.Load() != 6 || total.Load() != 10 {
		t.Fatalf("write counters = first:%d second:%d total:%d, want 4/6/10", firstJob.Load(), secondJob.Load(), total.Load())
	}
}

func TestProgressWriterShortWriteCountsOnlyReturnedBytes(t *testing.T) {
	job := &atomic.Uint64{}
	total := &atomic.Uint64{}
	writer := &progressWriter{
		conn:         &fixedWriteConn{n: 3, err: io.ErrUnexpectedEOF},
		idleTimeout:  time.Second,
		written:      job,
		totalWritten: total,
	}

	n, err := writer.Write(make([]byte, 10))
	if n != 3 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Write = %d, %v, want 3, unexpected EOF", n, err)
	}
	if job.Load() != 3 || total.Load() != 3 {
		t.Fatalf("short-write counters = job:%d total:%d, want 3/3", job.Load(), total.Load())
	}
}

func TestStreamUploadConnResumeOffsets(t *testing.T) {
	payload := []byte("0123456789")
	for _, offset := range []uint64{0, 4, uint64(len(payload))} {
		t.Run(string(rune('0'+offset)), func(t *testing.T) {
			client, remote := net.Pipe()
			defer client.Close()
			defer remote.Close()
			got := make(chan []byte, 1)
			go func() {
				init := &file.TransferInit{}
				if err := init.Deserialize(remote); err != nil || init.Token != soul.Token(99) {
					got <- nil
					return
				}
				if _, err := file.Write(remote, &file.Offset{Offset: offset}); err != nil {
					got <- nil
					return
				}
				body := make([]byte, len(payload)-int(offset))
				if _, err := io.ReadFull(remote, body); err != nil {
					got <- nil
					return
				}
				got <- body
				_ = remote.Close() // the downloader owns successful completion
			}()
			// Pre-seeded with a sentinel so this pins Store rather than Add.
			// Starting from zero, an Add(offset) implementation would reach
			// the same totals and the assertion below could not tell them
			// apart - and Add would be wrong, since a job is streamed by
			// exactly one attempt and must not accumulate across calls.
			sent := &atomic.Uint64{}
			sent.Store(12345)
			totalWritten := &atomic.Uint64{}
			startOffset := &atomic.Uint64{}
			err := streamUploadConn(client, 99, bytes.NewReader(payload), uint64(len(payload)), time.Second, time.Second, 0, 0, sent, startOffset, totalWritten)
			if err != nil {
				t.Fatalf("streamUploadConn: %v", err)
			}
			if body := <-got; !bytes.Equal(body, payload[offset:]) {
				t.Fatalf("body = %q, want %q", body, payload[offset:])
			}
			// Regardless of resume offset - including offset == len(payload),
			// which skips the body copy entirely - sent must end up at the
			// full payload length: streamUploadConn stores the peer's
			// requested offset before the body copy even starts, so a
			// resumed or already-complete transfer is never stuck reporting
			// less than 100%.
			if got := sent.Load(); got != uint64(len(payload)) {
				t.Fatalf("sent.Load() = %d, want %d (offset %d)", got, len(payload), offset)
			}
			if got, want := totalWritten.Load(), uint64(len(payload))-offset; got != want {
				t.Fatalf("totalWritten.Load() = %d, want actual body writes %d (offset %d)", got, want, offset)
			}
			// startOffset keeps the resume point after sent has moved past it,
			// which is the only way the caller can report this attempt's own
			// byte delta rather than the absolute counter (#325).
			if got := startOffset.Load(); got != offset {
				t.Fatalf("startOffset.Load() = %d, want the peer's requested offset %d", got, offset)
			}
		})
	}
}

func TestStreamUploadConnWaitsBoundedlyForDownloaderClose(t *testing.T) {
	client, remote := net.Pipe()
	defer client.Close()
	defer remote.Close()
	peerReady := make(chan struct{})
	go func() {
		var init file.TransferInit
		_ = init.Deserialize(remote)
		_, _ = file.Write(remote, &file.Offset{})
		body := make([]byte, 3)
		_, _ = io.ReadFull(remote, body)
		close(peerReady)
		// Deliberately retain the downloader side instead of closing it.
	}()
	start := time.Now()
	err := streamUploadConn(client, 1, bytes.NewReader([]byte("abc")), 3, time.Second, 30*time.Millisecond, 0, 0, nil, nil, nil)
	<-peerReady
	if err == nil || time.Since(start) < 20*time.Millisecond {
		t.Fatalf("completion wait error=%v elapsed=%v", err, time.Since(start))
	}
}

func TestStreamUploadConnRejectsOversizedOffset(t *testing.T) {
	client, remote := net.Pipe()
	defer client.Close()
	defer remote.Close()
	go func() {
		var init file.TransferInit
		_ = init.Deserialize(remote)
		_, _ = file.Write(remote, &file.Offset{Offset: 11})
	}()
	if err := streamUploadConn(client, 1, bytes.NewReader([]byte("0123456789")), 10, time.Second, time.Second, 0, 0, nil, nil, nil); err == nil {
		t.Fatal("oversized offset accepted")
	}
}

// TestStreamUploadConnAbortsSlowPeer verifies the minimum-throughput floor
// (issue #108): a peer that drains the upload far slower than minThroughput
// causes streamUploadConn to abort the connection and return
// errUploadTooSlow, instead of leaving the upload slot occupied forever.
func TestStreamUploadConnAbortsSlowPeer(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 2000)
	client, remote := net.Pipe()
	defer client.Close()
	defer remote.Close()
	go func() {
		var init file.TransferInit
		if err := init.Deserialize(remote); err != nil {
			return
		}
		if _, err := file.Write(remote, &file.Offset{}); err != nil {
			return
		}
		// Trickle-read the body a few bytes at a time, far below the
		// configured floor, until the connection is closed out from under
		// us by the sampler.
		buf := make([]byte, 10)
		for {
			if _, err := remote.Read(buf); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	rs := &readSeeker{r: bytes.NewReader(payload)}
	// threshold = minThroughput * sampleInterval.Seconds() = 100000 * 0.02 = 2000 bytes/window,
	// far above the 10 bytes/20ms (~500 bytes/s) the fake peer above trickles at.
	err := streamUploadConn(client, 1, rs, uint64(len(payload)), time.Second, time.Second, 100000, 20*time.Millisecond, nil, nil, nil)
	if !errors.Is(err, errUploadTooSlow) {
		t.Fatalf("streamUploadConn err = %v, want errUploadTooSlow", err)
	}
}

// TestStreamUploadConnAllowsSteadySlowPeer verifies that a peer reading at
// or above the throughput floor is never aborted by the sampler.
func TestStreamUploadConnAllowsSteadySlowPeer(t *testing.T) {
	payload := bytes.Repeat([]byte("y"), 300)
	client, remote := net.Pipe()
	defer client.Close()
	defer remote.Close()
	got := make(chan []byte, 1)
	go func() {
		var init file.TransferInit
		if err := init.Deserialize(remote); err != nil {
			got <- nil
			return
		}
		if _, err := file.Write(remote, &file.Offset{}); err != nil {
			got <- nil
			return
		}
		body := make([]byte, len(payload))
		if _, err := io.ReadFull(remote, body); err != nil {
			got <- nil
			return
		}
		got <- body
		_ = remote.Close() // the downloader owns successful completion
	}()
	rs := &readSeeker{r: bytes.NewReader(payload)}
	// threshold = minThroughput * sampleInterval.Seconds() = 1000 * 0.02 = 20 bytes/window;
	// the unthrottled reader below drains the whole 300-byte payload well within that.
	err := streamUploadConn(client, 1, rs, uint64(len(payload)), time.Second, time.Second, 1000, 20*time.Millisecond, nil, nil, nil)
	if err != nil {
		t.Fatalf("streamUploadConn: %v", err)
	}
	if body := <-got; !bytes.Equal(body, payload) {
		t.Fatalf("body = %q, want %q", body, payload)
	}
}
