package soulseek

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/file"
)

// readSeeker hides bytes.Reader's WriteTo method so io.CopyN always drives
// streamUploadConn's progressWriter through its ordinary Write path, keeping
// these tests independent of io.Copy's WriterTo fast-path optimizations.
type readSeeker struct{ r *bytes.Reader }

func (rs *readSeeker) Read(p []byte) (int, error)                   { return rs.r.Read(p) }
func (rs *readSeeker) Seek(offset int64, whence int) (int64, error) { return rs.r.Seek(offset, whence) }

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
			err := streamUploadConn(client, 99, bytes.NewReader(payload), uint64(len(payload)), time.Second, time.Second, 0, 0)
			if err != nil {
				t.Fatalf("streamUploadConn: %v", err)
			}
			if body := <-got; !bytes.Equal(body, payload[offset:]) {
				t.Fatalf("body = %q, want %q", body, payload[offset:])
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
	err := streamUploadConn(client, 1, bytes.NewReader([]byte("abc")), 3, time.Second, 30*time.Millisecond, 0, 0)
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
	if err := streamUploadConn(client, 1, bytes.NewReader([]byte("0123456789")), 10, time.Second, time.Second, 0, 0); err == nil {
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
	err := streamUploadConn(client, 1, rs, uint64(len(payload)), time.Second, time.Second, 100000, 20*time.Millisecond)
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
	err := streamUploadConn(client, 1, rs, uint64(len(payload)), time.Second, time.Second, 1000, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("streamUploadConn: %v", err)
	}
	if body := <-got; !bytes.Equal(body, payload) {
		t.Fatalf("body = %q, want %q", body, payload)
	}
}
