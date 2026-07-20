package soulseek

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/file"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/server"
)

// TestDownloadEndToEndQueuePositionAndCompletion drives the full native
// download choreography over real loopback TCP (the startConnectedClient
// harness connectpeer_test.go/fileconn_test.go use), playing the remote
// "friend" uploader's part end to end on a second goroutine:
//
//  1. the client Enqueues a download for "friend";
//  2. the fake central server answers GetPeerAddress with the fake peer's
//     listener address, so the client's runDownload dials it directly and
//     sends QueueUpload on the resulting P session;
//  3. the fake peer answers a PlaceInQueueRequest first (exercising the
//     queue-position polling path runDownload takes while queued) before
//     sending TransferRequest;
//  4. the client's download hook wakes runDownload, which sends
//     TransferResponse(allowed) and starts waiting for an F connection;
//  5. the fake peer dials the client's own listener back for the F
//     connection, exactly like a real uploader, and streams the file;
//  6. ListDownloads is polled until the transfer reaches a terminal state,
//     and its final fields plus the file actually written to disk are
//     checked against what was streamed.
func TestDownloadEndToEndQueuePositionAndCompletion(t *testing.T) {
	const filename = `Artist - Album\01 Track.flac`
	payload := bytes.Repeat([]byte("soulseek-e2e-payload-"), 100)
	size := int64(len(payload))
	const transferToken = soul.Token(9001)
	const queuePlace = uint32(3)

	peerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake peer: %v", err)
	}
	defer peerLn.Close()
	peerAddr := peerLn.Addr().(*net.TCPAddr)

	// listenAddr is written once by the main test goroutine, strictly before
	// Enqueue (below) ever triggers the chain of events that leads the fake
	// peer to read it - see the comment at that assignment.
	var listenAddr string

	go func() {
		conn, err := peerLn.Accept()
		if err != nil {
			t.Logf("fake peer: accept: %v", err)
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

		reader, _, _, err := peer.Read(peer.CodeInit(0), conn, false)
		if err != nil {
			t.Logf("fake peer: read peer init: %v", err)
			return
		}
		pi := &peer.PeerInit{}
		if err := pi.Deserialize(reader); err != nil {
			t.Logf("fake peer: deserialize peer init: %v", err)
			return
		}
		if pi.ConnectionType != peer.ConnectionType {
			t.Errorf("fake peer: PeerInit.ConnectionType = %q, want %q", pi.ConnectionType, peer.ConnectionType)
			return
		}

		reader, _, code, err := peer.Read(peer.Code(0), conn, false)
		if err != nil {
			t.Logf("fake peer: read queue upload: %v", err)
			return
		}
		if peer.Code(code) != peer.CodeQueueUpload {
			t.Errorf("fake peer: code = %d, want CodeQueueUpload", code)
			return
		}
		qu := &peer.QueueUpload{}
		if err := qu.Deserialize(reader); err != nil {
			t.Errorf("fake peer: deserialize queue upload: %v", err)
			return
		}
		if qu.Filename != filename {
			t.Errorf("fake peer: QueueUpload.Filename = %q, want %q", qu.Filename, filename)
			return
		}

		// Answer with a queue position first, exercising the
		// PlaceInQueueRequest polling path runDownload takes before a
		// TransferRequest ever arrives.
		reader, _, code, err = peer.Read(peer.Code(0), conn, false)
		if err != nil {
			t.Logf("fake peer: read place in queue request: %v", err)
			return
		}
		if peer.Code(code) != peer.CodePlaceInQueueRequest {
			t.Errorf("fake peer: code = %d, want CodePlaceInQueueRequest", code)
			return
		}
		piqr := &peer.PlaceInQueueRequest{}
		if err := piqr.Deserialize(reader); err != nil {
			t.Errorf("fake peer: deserialize place in queue request: %v", err)
			return
		}
		piqResponse := &peer.PlaceInQueueResponse{Filename: piqr.Filename, Place: queuePlace}
		if _, err := peer.Write(conn, piqResponse, false); err != nil {
			t.Errorf("fake peer: write place in queue response: %v", err)
			return
		}

		transferRequest := &peer.TransferRequest{
			Direction: peer.UploadToPeer, Token: transferToken,
			Filename: qu.Filename, FileSize: uint64(size),
		}
		if _, err := peer.Write(conn, transferRequest, false); err != nil {
			t.Errorf("fake peer: write transfer request: %v", err)
			return
		}

		reader, _, code, err = peer.Read(peer.Code(0), conn, false)
		if err != nil {
			t.Logf("fake peer: read transfer response: %v", err)
			return
		}
		if peer.Code(code) != peer.CodeTransferResponse {
			t.Errorf("fake peer: code = %d, want CodeTransferResponse", code)
			return
		}
		tresp := &peer.TransferResponse{}
		if err := tresp.Deserialize(reader); err != nil {
			t.Errorf("fake peer: deserialize transfer response: %v", err)
			return
		}
		if !tresp.Allowed {
			t.Errorf("fake peer: TransferResponse.Allowed = false, want true")
			return
		}

		// Play the uploader's part on a fresh F connection dialed back to
		// the client's own listener, exactly like a real peer initiating
		// the file transfer once we have allowed it.
		fconn, err := net.Dial("tcp", listenAddr)
		if err != nil {
			t.Errorf("fake peer: dial client listener for F connection: %v", err)
			return
		}
		defer fconn.Close()
		_ = fconn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := peer.Write(fconn, &peer.PeerInit{Username: "friend", ConnectionType: file.ConnectionType}, false); err != nil {
			t.Errorf("fake peer: write F peer init: %v", err)
			return
		}
		if _, err := file.Write(fconn, &file.TransferInit{Token: transferToken}); err != nil {
			t.Errorf("fake peer: write transfer init: %v", err)
			return
		}
		off := &file.Offset{}
		if err := off.Deserialize(fconn); err != nil {
			t.Errorf("fake peer: read offset: %v", err)
			return
		}
		if off.Offset != 0 {
			t.Errorf("fake peer: offset = %d, want 0 (fresh download)", off.Offset)
			return
		}
		if _, err := fconn.Write(payload); err != nil {
			t.Errorf("fake peer: write payload: %v", err)
		}
	}()

	c, addr := startConnectedClient(t, func(conn net.Conn) {
		code, body, err := readRawFrame(conn)
		if err != nil || code != uint32(server.CodeGetPeerAddress) {
			t.Logf("read get peer address request: code=%d err=%v", code, err)
			return
		}
		username, err := parseGetPeerAddressRequest(body)
		if err != nil {
			t.Errorf("parse get peer address request: %v", err)
			return
		}
		writeGetPeerAddressResponse(t, conn, username, peerAddr.IP, peerAddr.Port, 0)
		_, _ = io.Copy(io.Discard, conn)
	})
	// Safe to write here, strictly before Enqueue below: the fake peer only
	// reads listenAddr after a chain of events (GetPeerAddress round trip,
	// direct P dial, QueueUpload, PlaceInQueueRequest, TransferRequest,
	// TransferResponse) that Enqueue alone kicks off, and the "go" statement
	// starting that chain's goroutine happens after this assignment in
	// program order on this same (test) goroutine.
	listenAddr = addr
	c.cfg.DownloadDir = t.TempDir()
	c.cfg.placeInQueueInterval = 30 * time.Millisecond

	id, err := c.Enqueue(context.Background(), "friend", filename, size)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if id == "" {
		t.Fatal("Enqueue returned an empty id")
	}

	deadline := time.Now().Add(5 * time.Second)
	var final core.RemoteTransfer
pollLoop:
	for time.Now().Before(deadline) {
		list, err := c.ListDownloads(context.Background())
		if err != nil {
			t.Fatalf("ListDownloads: %v", err)
		}
		for _, tr := range list {
			if tr.ID != id {
				continue
			}
			final = tr
			if tr.State == core.TransferCompleted || tr.State == core.TransferErrored {
				break pollLoop
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	if final.State != core.TransferCompleted {
		t.Fatalf("final transfer state = %q (failure=%q retryable=%v), want %q", final.State, final.Failure, final.Retryable, core.TransferCompleted)
	}
	if final.BytesDone != size {
		t.Errorf("BytesDone = %d, want %d", final.BytesDone, size)
	}
	if final.QueuePosition != queuePlace {
		t.Errorf("QueuePosition = %d, want %d", final.QueuePosition, queuePlace)
	}

	destPath := downloadDestPath(c.cfg.DownloadDir, filename)
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("downloaded file content does not match the streamed payload")
	}
	if _, err := os.Stat(destPath + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat .part after a completed download: err = %v, want os.ErrNotExist", err)
	}
}
