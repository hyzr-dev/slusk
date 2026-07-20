package soulseek

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/file"
)

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
			err := streamUploadConn(client, 99, bytes.NewReader(payload), uint64(len(payload)), time.Second, time.Second)
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
	err := streamUploadConn(client, 1, bytes.NewReader([]byte("abc")), 3, time.Second, 30*time.Millisecond)
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
	if err := streamUploadConn(client, 1, bytes.NewReader([]byte("0123456789")), 10, time.Second, time.Second); err == nil {
		t.Fatal("oversized offset accepted")
	}
}
