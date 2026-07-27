package soulseek

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"strings"
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

		// runDownload keeps polling PlaceInQueueRequest on its ticker until it
		// observes our TransferRequest, so one or more extra polls can arrive
		// on this connection before the TransferResponse does. Drain and ignore
		// any of them rather than depend on exact scheduler timing.
		var tresp *peer.TransferResponse
		for tresp == nil {
			reader, _, code, err = peer.Read(peer.Code(0), conn, false)
			if err != nil {
				t.Logf("fake peer: read transfer response: %v", err)
				return
			}
			switch peer.Code(code) {
			case peer.CodePlaceInQueueRequest:
				if err := (&peer.PlaceInQueueRequest{}).Deserialize(reader); err != nil {
					t.Errorf("fake peer: deserialize extra place in queue request: %v", err)
					return
				}
			case peer.CodeTransferResponse:
				tresp = &peer.TransferResponse{}
				if err := tresp.Deserialize(reader); err != nil {
					t.Errorf("fake peer: deserialize transfer response: %v", err)
					return
				}
			default:
				t.Errorf("fake peer: code = %d, want CodeTransferResponse or CodePlaceInQueueRequest", code)
				return
			}
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
	// Cleared the moment the peer started sending (issue #256): a finished
	// transfer is not waiting in anyone's queue, and reporting the place it
	// held right before pickup rendered the file as simultaneously downloading
	// and queued. This assertion used to require queuePlace here, which
	// encoded the bug. That the place is delivered *while* the transfer waits
	// is covered deterministically by TestDownloadHooksClaimAndDispatch —
	// asserting it here would depend on the poll loop happening to sample the
	// queued window.
	if final.QueuePosition != 0 {
		t.Errorf("QueuePosition = %d, want 0 (cleared when the transfer started)", final.QueuePosition)
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

// TestDownloadRejectsOutOfRangeFileSizeWithoutLeakingFileConn is the regression
// guard for a leak the FileSize > MaxInt64 guard introduced: that give-up path
// returned without the fileConnCh teardown every sibling give-up path runs, so a
// peer that declared an out-of-range size and then opened the promised F
// connection left its socket and inbound lease stranded in the buffered channel
// forever. Here the fake peer declares FileSize = math.MaxUint64 yet still dials
// the F connection back like a real uploader. The transfer must ERROR, and the
// client must actively close that F connection and release its inbound lease -
// which, with the bug, it never does (the socket sits unread in fileConnCh).
func TestDownloadRejectsOutOfRangeFileSizeWithoutLeakingFileConn(t *testing.T) {
	const filename = `Artist - Album\01 Track.flac`
	const transferToken = soul.Token(9002)
	const size = int64(4096)

	peerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake peer: %v", err)
	}
	defer peerLn.Close()
	peerAddr := peerLn.Addr().(*net.TCPAddr)

	var listenAddr string
	// Closed by the fake peer once the client closes the rejected F connection
	// from its side (io.Copy returns EOF). With the leak the client never closes
	// it, so this never fires and the test fails deterministically.
	fconnClosedByClient := make(chan struct{})

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
		if err := (&peer.PeerInit{}).Deserialize(reader); err != nil {
			t.Logf("fake peer: deserialize peer init: %v", err)
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

		// Declare an out-of-range size: >= 2^63 overflows int64, which the client
		// must reject. It still sends the accept before hitting that guard, so we
		// proceed to open the F connection exactly like a real upload.
		transferRequest := &peer.TransferRequest{
			Direction: peer.UploadToPeer, Token: transferToken,
			Filename: qu.Filename, FileSize: math.MaxUint64,
		}
		if _, err := peer.Write(conn, transferRequest, false); err != nil {
			t.Errorf("fake peer: write transfer request: %v", err)
			return
		}

		var tresp *peer.TransferResponse
		for tresp == nil {
			reader, _, code, err = peer.Read(peer.Code(0), conn, false)
			if err != nil {
				t.Logf("fake peer: read transfer response: %v", err)
				return
			}
			switch peer.Code(code) {
			case peer.CodePlaceInQueueRequest:
				if err := (&peer.PlaceInQueueRequest{}).Deserialize(reader); err != nil {
					t.Errorf("fake peer: deserialize place in queue request: %v", err)
					return
				}
			case peer.CodeTransferResponse:
				tresp = &peer.TransferResponse{}
				if err := tresp.Deserialize(reader); err != nil {
					t.Errorf("fake peer: deserialize transfer response: %v", err)
					return
				}
			default:
				t.Errorf("fake peer: code = %d, want CodeTransferResponse or CodePlaceInQueueRequest", code)
				return
			}
		}

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
		// Block until the client closes the F connection from its side. The fix
		// (refuse the handoff, close the socket, release the lease) makes this
		// return EOF; the leak leaves the socket stranded and this blocks until
		// the 5s deadline above.
		_, _ = io.Copy(io.Discard, fconn)
		close(fconnClosedByClient)
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
	listenAddr = addr
	c.cfg.DownloadDir = t.TempDir()
	c.cfg.placeInQueueInterval = 30 * time.Millisecond

	id, err := c.Enqueue(context.Background(), "friend", filename, size)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var final core.RemoteTransfer
	for time.Now().Before(deadline) {
		list, err := c.ListDownloads(context.Background())
		if err != nil {
			t.Fatalf("ListDownloads: %v", err)
		}
		found := false
		for _, tr := range list {
			if tr.ID == id {
				final = tr
				found = true
			}
		}
		if found && final.State == core.TransferErrored {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if final.State != core.TransferErrored {
		t.Fatalf("final transfer state = %q (failure=%q), want %q", final.State, final.Failure, core.TransferErrored)
	}
	if !strings.Contains(final.Failure, "out-of-range file size") {
		t.Errorf("failure = %q, want it to name the out-of-range file size", final.Failure)
	}

	select {
	case <-fconnClosedByClient:
	case <-time.After(3 * time.Second):
		t.Fatal("client never closed the rejected F connection — socket and inbound lease leaked")
	}

	// The inbound lease behind that F connection must be released; inboundSlots
	// holds one token per live lease, so it must drain back to empty.
	leaseDeadline := time.Now().Add(time.Second)
	for len(c.inboundSlots) != 0 && time.Now().Before(leaseDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(c.inboundSlots); got != 0 {
		t.Errorf("inboundSlots still holds %d lease(s) after the errored transfer — F connection leaked", got)
	}
}

// TestDownloadCancelAbortsActiveStream is the regression guard for issue #99:
// Cancel used to mark the transfer TransferCancelled but let the in-flight
// streamFile run on until completion or idle timeout, holding the F connection
// and its inbound lease. Here the fake peer streams half the payload and then
// deliberately stalls, holding its end open; Cancel must close the client's
// side of the F connection promptly (long before the 60s idle timeout set
// below), the transfer must stay TransferCancelled - not flip to
// Errored/Completed - and the ".part" file must remain for Remove.
func TestDownloadCancelAbortsActiveStream(t *testing.T) {
	const filename = `Artist - Album\02 Track.flac`
	payload := bytes.Repeat([]byte("soulseek-cancel-payload-"), 100)
	size := int64(len(payload))
	half := size / 2
	const transferToken = soul.Token(9002)

	peerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake peer: %v", err)
	}
	defer peerLn.Close()
	peerAddr := peerLn.Addr().(*net.TCPAddr)

	var listenAddr string
	// Closed by the fake peer once its stalled F connection is torn down by
	// the client - the observable proof that Cancel aborted the stream.
	fconnClosed := make(chan struct{})

	go func() {
		conn, err := peerLn.Accept()
		if err != nil {
			t.Logf("fake peer: accept: %v", err)
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

		reader, _, _, err := peer.Read(peer.CodeInit(0), conn, false)
		if err != nil {
			t.Logf("fake peer: read peer init: %v", err)
			return
		}
		if err := (&peer.PeerInit{}).Deserialize(reader); err != nil {
			t.Logf("fake peer: deserialize peer init: %v", err)
			return
		}

		reader, _, code, err := peer.Read(peer.Code(0), conn, false)
		if err != nil || peer.Code(code) != peer.CodeQueueUpload {
			t.Logf("fake peer: read queue upload: code=%d err=%v", code, err)
			return
		}
		qu := &peer.QueueUpload{}
		if err := qu.Deserialize(reader); err != nil {
			t.Errorf("fake peer: deserialize queue upload: %v", err)
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

		var tresp *peer.TransferResponse
		for tresp == nil {
			reader, _, code, err = peer.Read(peer.Code(0), conn, false)
			if err != nil {
				t.Logf("fake peer: read transfer response: %v", err)
				return
			}
			switch peer.Code(code) {
			case peer.CodePlaceInQueueRequest:
				if err := (&peer.PlaceInQueueRequest{}).Deserialize(reader); err != nil {
					t.Errorf("fake peer: deserialize place in queue request: %v", err)
					return
				}
			case peer.CodeTransferResponse:
				tresp = &peer.TransferResponse{}
				if err := tresp.Deserialize(reader); err != nil {
					t.Errorf("fake peer: deserialize transfer response: %v", err)
					return
				}
			default:
				t.Errorf("fake peer: code = %d, want CodeTransferResponse or CodePlaceInQueueRequest", code)
				return
			}
		}

		fconn, err := net.Dial("tcp", listenAddr)
		if err != nil {
			t.Errorf("fake peer: dial client listener for F connection: %v", err)
			return
		}
		defer fconn.Close()
		_ = fconn.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := peer.Write(fconn, &peer.PeerInit{Username: "friend", ConnectionType: file.ConnectionType}, false); err != nil {
			t.Errorf("fake peer: write F peer init: %v", err)
			return
		}
		if _, err := file.Write(fconn, &file.TransferInit{Token: transferToken}); err != nil {
			t.Errorf("fake peer: write transfer init: %v", err)
			return
		}
		if err := (&file.Offset{}).Deserialize(fconn); err != nil {
			t.Errorf("fake peer: read offset: %v", err)
			return
		}
		if _, err := fconn.Write(payload[:half]); err != nil {
			t.Errorf("fake peer: write first half of payload: %v", err)
			return
		}
		// Stall: send nothing more, and wait for the client to close its end.
		// A Read here only returns once the client's side goes away (or the
		// 10s deadline above fires, which the assertions below would catch).
		buf := make([]byte, 1)
		_, _ = fconn.Read(buf)
		close(fconnClosed)
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
	listenAddr = addr
	c.cfg.DownloadDir = t.TempDir()
	// Long enough that only an aborted stream - never the idle timeout - can
	// explain the F connection closing within this test's deadlines.
	c.cfg.fileIdleTimeout = 60 * time.Second

	id, err := c.Enqueue(context.Background(), "friend", filename, size)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Wait until the stream is demonstrably in flight (half the payload
	// landed) before cancelling mid-stream.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the stream to reach the half-payload mark")
		}
		list, err := c.ListDownloads(context.Background())
		if err != nil {
			t.Fatalf("ListDownloads: %v", err)
		}
		var done int64
		for _, tr := range list {
			if tr.ID == id {
				done = tr.BytesDone
			}
		}
		if done >= half {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := c.Cancel(context.Background(), "friend", id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	select {
	case <-fconnClosed:
		// Cancel tore down the F connection while the peer was stalling.
	case <-time.After(3 * time.Second):
		t.Fatal("the F connection was not closed within 3s of Cancel; the stream kept running")
	}

	// The state must remain TransferCancelled - give the orchestration
	// goroutine time to run its post-stream branches, then confirm nothing
	// resurrected or re-labelled the transfer.
	time.Sleep(100 * time.Millisecond)
	list, err := c.ListDownloads(context.Background())
	if err != nil {
		t.Fatalf("ListDownloads: %v", err)
	}
	var final core.RemoteTransfer
	for _, tr := range list {
		if tr.ID == id {
			final = tr
		}
	}
	if final.State != core.TransferCancelled {
		t.Fatalf("state after Cancel = %q (failure=%q), want %q", final.State, final.Failure, core.TransferCancelled)
	}

	destPath := downloadDestPath(c.cfg.DownloadDir, filename)
	if _, err := os.Stat(destPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat dest after cancel: err = %v, want os.ErrNotExist", err)
	}
	if _, err := os.Stat(destPath + ".part"); err != nil {
		t.Errorf("stat .part after cancel: %v, want it left in place for Remove", err)
	}
}
