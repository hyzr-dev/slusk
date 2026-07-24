package soulseek

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/file"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/server"
)

// --- streamFile: loopback TCP, no Client involved ---

func TestStreamFileWritesToDisk(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake uploader: %v", err)
	}
	defer ln.Close()

	payload := bytes.Repeat([]byte("abcdefgh"), 128) // 1024 bytes
	offsetSeen := make(chan uint64, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		off := &file.Offset{}
		if err := off.Deserialize(conn); err != nil {
			t.Logf("fake uploader: read offset: %v", err)
			return
		}
		offsetSeen <- off.Offset
		if _, err := conn.Write(payload); err != nil {
			t.Logf("fake uploader: write payload: %v", err)
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial fake uploader: %v", err)
	}
	defer conn.Close()

	destPath := filepath.Join(t.TempDir(), "leaf", "song.flac")

	var progressCalls []int64
	written, err := streamFile(conn, destPath, int64(len(payload)), time.Second, func(n int64) {
		progressCalls = append(progressCalls, n)
	})
	if err != nil {
		t.Fatalf("streamFile: %v", err)
	}
	if written != int64(len(payload)) {
		t.Errorf("written = %d, want %d", written, len(payload))
	}

	select {
	case off := <-offsetSeen:
		if off != 0 {
			t.Errorf("offset sent to peer = %d, want 0 (fresh download)", off)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake uploader never saw an Offset frame")
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read dest file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("dest file content does not match the streamed payload")
	}
	if _, err := os.Stat(destPath + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat .part after a successful stream: err = %v, want os.ErrNotExist", err)
	}
	if len(progressCalls) == 0 {
		t.Fatal("progress callback was never called")
	}
	if last := progressCalls[len(progressCalls)-1]; last != int64(len(payload)) {
		t.Errorf("last progress call = %d, want %d (cumulative, includes resume offset)", last, len(payload))
	}
}

func TestStreamFileResumeSendsOffset(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake uploader: %v", err)
	}
	defer ln.Close()

	destPath := filepath.Join(t.TempDir(), "song.flac")
	existing := []byte("EXISTING-PARTIAL-DOWNLOAD-DATA")
	if err := os.WriteFile(destPath+".part", existing, 0o644); err != nil {
		t.Fatalf("seed partial download: %v", err)
	}

	remainder := bytes.Repeat([]byte("Z"), 50)
	total := int64(len(existing) + len(remainder))

	offsetSeen := make(chan uint64, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		off := &file.Offset{}
		if err := off.Deserialize(conn); err != nil {
			t.Logf("fake uploader: read offset: %v", err)
			return
		}
		offsetSeen <- off.Offset
		if _, err := conn.Write(remainder); err != nil {
			t.Logf("fake uploader: write remainder: %v", err)
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial fake uploader: %v", err)
	}
	defer conn.Close()

	written, err := streamFile(conn, destPath, total, time.Second, nil)
	if err != nil {
		t.Fatalf("streamFile: %v", err)
	}
	if written != total {
		t.Errorf("written = %d, want %d", written, total)
	}

	select {
	case off := <-offsetSeen:
		if off != uint64(len(existing)) {
			t.Errorf("offset sent to peer = %d, want %d", off, len(existing))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake uploader never saw an Offset frame")
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read dest file: %v", err)
	}
	want := append(append([]byte{}, existing...), remainder...)
	if !bytes.Equal(got, want) {
		t.Error("dest file content does not match existing-prefix + streamed remainder")
	}
}

func TestStreamFileShortStreamKeepsPart(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake uploader: %v", err)
	}
	defer ln.Close()

	shortPayload := []byte("only-part-of-the-file")
	const wantSize = 100 // larger than len(shortPayload)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		off := &file.Offset{}
		if err := off.Deserialize(conn); err != nil {
			t.Logf("fake uploader: read offset: %v", err)
			conn.Close()
			return
		}
		if _, err := conn.Write(shortPayload); err != nil {
			t.Logf("fake uploader: write short payload: %v", err)
		}
		conn.Close() // hang up before sending the promised wantSize bytes
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial fake uploader: %v", err)
	}
	defer conn.Close()

	destPath := filepath.Join(t.TempDir(), "song.flac")

	written, err := streamFile(conn, destPath, wantSize, time.Second, nil)
	if err == nil {
		t.Fatal("streamFile: expected an error for a short stream, got nil")
	}
	if written != int64(len(shortPayload)) {
		t.Errorf("written = %d, want %d", written, len(shortPayload))
	}

	if _, statErr := os.Stat(destPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("dest file exists after a failed stream: err = %v, want os.ErrNotExist", statErr)
	}
	got, err := os.ReadFile(destPath + ".part")
	if err != nil {
		t.Fatalf("read .part after short stream: %v", err)
	}
	if !bytes.Equal(got, shortPayload) {
		t.Error(".part content does not match the bytes actually received before the peer hung up")
	}
}

type recordingReadConn struct {
	net.Conn
	read bytes.Buffer
}

func (c *recordingReadConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		_, _ = c.read.Write(p[:n])
	}
	return n, err
}

func TestZeroByteTransferHandshakeCreatesDestinationAndCompletesUpload(t *testing.T) {
	downloadConn, rawUploadConn := net.Pipe()
	uploadConn := &recordingReadConn{Conn: rawUploadConn}
	uploadDone := make(chan error, 1)
	go func() {
		uploadDone <- streamUploadConn(uploadConn, 123, bytes.NewReader(nil), 0, time.Second, time.Second)
	}()

	init := &file.TransferInit{}
	if err := init.Deserialize(downloadConn); err != nil {
		t.Fatalf("read transfer init: %v", err)
	}
	if init.Token != 123 {
		t.Fatalf("transfer token = %d, want 123", init.Token)
	}
	destPath := filepath.Join(t.TempDir(), "leaf", "empty.flac")
	written, err := streamFile(downloadConn, destPath, 0, time.Second, nil)
	if err != nil {
		t.Fatalf("stream zero-byte file: %v", err)
	}
	if written != 0 {
		t.Fatalf("written = %d, want 0", written)
	}
	if err := downloadConn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-uploadDone:
		if err != nil {
			t.Fatalf("zero-byte uploader completion: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("zero-byte uploader did not complete")
	}
	if got := uploadConn.read.Bytes(); len(got) < 8 || binary.LittleEndian.Uint64(got[:8]) != 0 {
		t.Fatalf("offset bytes = %x, want Offset(0)", got)
	}
	info, err := os.Stat(destPath)
	if err != nil {
		t.Fatalf("stat empty destination: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("empty destination size = %d, want 0", info.Size())
	}
	if _, err := os.Stat(destPath + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial remains after zero-byte completion: %v", err)
	}
}

func TestCompletedEmptyPartialSkipsHandshake(t *testing.T) {
	destPath := filepath.Join(t.TempDir(), "empty.flac")
	if err := os.WriteFile(destPath+".part", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	done := make(chan error, 1)
	go func() {
		_, err := streamFile(local, destPath, 0, time.Second, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("finish completed empty partial: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("completed empty partial attempted a socket handshake")
	}
	if info, err := os.Stat(destPath); err != nil || info.Size() != 0 {
		t.Fatalf("completed empty destination: info=%v err=%v", info, err)
	}
}

// --- handleInboundFileConn: direct inbound and unknown-token paths, via the
// same startConnectedClient/PeerInit harness connectpeer_test.go uses ---

func TestHandleInboundFileConnDirect(t *testing.T) {
	c, addr := startConnectedClient(t, func(conn net.Conn) {
		_, _ = io.Copy(io.Discard, conn)
	})

	tr := newTransfer("id1", "friend", "song.flac", 100)
	c.downloads.insert(tr)
	const token = soul.Token(555)
	c.downloads.registerToken(tr, token)
	// Model runDownload parked in its negotiation select: attachFileConn only
	// hands the F connection off to a transfer that is awaiting it.
	tr.mu.Lock()
	tr.awaitingFileConn = true
	tr.mu.Unlock()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial client listener: %v", err)
	}
	defer conn.Close()

	if _, err := peer.Write(conn, &peer.PeerInit{Username: "friend", ConnectionType: file.ConnectionType}, false); err != nil {
		t.Fatalf("write peer init: %v", err)
	}
	if _, err := file.Write(conn, &file.TransferInit{Token: token}); err != nil {
		t.Fatalf("write transfer init: %v", err)
	}

	select {
	case handoff := <-tr.fileConnCh:
		if handoff.conn == nil {
			t.Error("delivered handoff has a nil conn")
		}
		if handoff.lease == nil {
			t.Error("delivered handoff has a nil lease, want the accept-path inbound lease")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("transfer never received its file connection")
	}
}

func TestHandleInboundFileConnUnknownTokenClosed(t *testing.T) {
	c, addr := startConnectedClient(t, func(conn net.Conn) {
		_, _ = io.Copy(io.Discard, conn)
	})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial client listener: %v", err)
	}
	defer conn.Close()

	if _, err := peer.Write(conn, &peer.PeerInit{Username: "friend", ConnectionType: file.ConnectionType}, false); err != nil {
		t.Fatalf("write peer init: %v", err)
	}
	if _, err := file.Write(conn, &file.TransferInit{Token: soul.Token(999)}); err != nil {
		t.Fatalf("write transfer init: %v", err)
	}

	buf := make([]byte, 1)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(buf); err == nil {
		t.Error("expected the connection with an unknown token to be closed by the client")
	}

	deadline := time.Now().Add(2 * time.Second)
	for len(c.inboundSlots) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(c.inboundSlots); n != 0 {
		t.Errorf("inbound lease not released for the unknown-token connection: inboundSlots len = %d, want 0", n)
	}
}

// TestHandleInboundFileConnIndirectMirrorDial exercises the indirect path:
// the server relays a ConnectToPeer for "F", the client mirror-dials the peer
// back and writes PierceFirewall (peers.go handleConnectToPeer, mirroring
// TestHandleConnectToPeerMirrorSuccess in connectpeer_test.go), and the fake
// peer then plays the uploader's part by sending TransferInit on that same
// connection. The transfer is registered before Run starts so there is no
// race between the server's relay (which can fire immediately after login)
// and the test's registration.
func TestHandleInboundFileConnIndirectMirrorDial(t *testing.T) {
	peerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake peer: %v", err)
	}
	defer peerLn.Close()
	peerAddr := peerLn.Addr().(*net.TCPAddr)

	const transferToken = soul.Token(4242)
	fileConnAccepted := make(chan net.Conn, 1)
	go func() {
		conn, err := peerLn.Accept()
		if err != nil {
			return
		}
		reader, _, _, err := peer.Read(peer.CodeInit(0), conn, false)
		if err != nil {
			t.Logf("fake peer: read pierce firewall: %v", err)
			conn.Close()
			return
		}
		pf := &peer.PierceFirewall{}
		if err := pf.Deserialize(reader); err != nil {
			t.Logf("fake peer: deserialize pierce firewall: %v", err)
			conn.Close()
			return
		}
		if _, err := file.Write(conn, &file.TransferInit{Token: transferToken}); err != nil {
			t.Logf("fake peer: write transfer init: %v", err)
			conn.Close()
			return
		}
		fileConnAccepted <- conn
	}()

	srv := newFakeServer(t)
	srv.serve(t, func(conn net.Conn) {
		defer conn.Close()
		if err := drainLoginRequest(conn); err != nil {
			return
		}
		if _, err := conn.Write(loginSuccessFrame(t)); err != nil {
			return
		}
		if _, _, err := readRawFrame(conn); err != nil { // SetListenPort
			return
		}
		if err := drainInitialTreeAdvertisements(conn); err != nil {
			return
		}

		payload := new(bytes.Buffer)
		mustWrite(t, writeUint32(payload, uint32(server.CodeConnectToPeer)))
		mustWrite(t, writeString(payload, "friend"))
		mustWrite(t, writeString(payload, string(file.ConnectionType)))
		payload.Write(wireIPBytes(peerAddr.IP))
		mustWrite(t, writeUint32(payload, uint32(peerAddr.Port)))
		mustWrite(t, writeUint32(payload, uint32(transferToken)))
		mustWrite(t, writeBool(payload, false)) // privileged
		mustWrite(t, writeUint32(payload, 0))   // obfuscated port
		if _, err := conn.Write(packFrame(payload.Bytes())); err != nil {
			t.Logf("write connect to peer: %v", err)
			return
		}
		_, _ = io.Copy(io.Discard, conn)
	})

	c := New(Config{Address: srv.addr(), Username: "me", Password: "p", ListenAddr: "127.0.0.1:0"}, testLogger())
	c.cfg.allowLoopbackPeerDial = true

	// Register the transfer before Run starts dialing anything, so the
	// server's ConnectToPeer relay (which can arrive immediately after
	// login) can never race ahead of the registration it depends on.
	tr := newTransfer("id1", "friend", "song.flac", 100)
	c.downloads.insert(tr)
	c.downloads.registerToken(tr, transferToken)
	// Model runDownload parked in its negotiation select: attachFileConn only
	// hands the F connection off to a transfer that is awaiting it.
	tr.mu.Lock()
	tr.awaitingFileConn = true
	tr.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	waitForState(t, c, StateConnected, 2*time.Second)

	select {
	case conn := <-fileConnAccepted:
		defer conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("fake peer never completed the pierce-firewall + transfer-init handshake")
	}

	select {
	case handoff := <-tr.fileConnCh:
		if handoff.conn == nil {
			t.Error("delivered handoff has a nil conn")
		}
		if handoff.lease != nil {
			t.Error("mirror-dial file connection should carry no inbound lease")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("transfer never received its file connection over the indirect path")
	}
}

// TestStreamFileRejectsNegativeSize locks the defense-in-depth guard against a
// negative (overflowed) transfer size: streamFile must error immediately and
// never create a "completed" destination file. Passing a nil conn also proves
// the guard runs before any socket use.
func TestStreamFileRejectsNegativeSize(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.flac")
	written, err := streamFile(nil, dest, -1, time.Second, nil)
	if err == nil {
		t.Fatal("streamFile(size=-1) = nil error, want rejection")
	}
	if written != 0 {
		t.Errorf("written = %d, want 0", written)
	}
	if _, statErr := os.Stat(dest); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a destination file was created for a negative size: %v", statErr)
	}
}

// --- streamFile error classification (issue #103): local filesystem failures
// are tagged diskError so runDownload can mark them non-retryable, while
// peer/network failures stay untagged (retryable). ---

func TestStreamFileLocalPathFailureIsDiskError(t *testing.T) {
	// A regular file where the destination's parent directory should be makes
	// every local path operation fail before the connection is ever touched.
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	destPath := filepath.Join(blocker, "song.flac")

	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	_, err := streamFile(local, destPath, 8, time.Second, nil)
	if err == nil {
		t.Fatal("streamFile: expected an error for a blocked destination path, got nil")
	}
	var de *diskError
	if !errors.As(err, &de) {
		t.Errorf("streamFile error = %v, want a diskError (local filesystem failure)", err)
	}
}

func TestStreamFileOpenFailureIsDiskError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits; the read-only dir cannot fail the open")
	}
	// The destination directory exists but is read-only, so the .part open
	// fails after the Offset handshake with the peer has already happened.
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o555); err != nil {
		t.Fatalf("mkdir read-only dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	destPath := filepath.Join(dir, "song.flac")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake uploader: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		off := &file.Offset{}
		_ = off.Deserialize(conn) // absorb the handshake; the failure is local
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial fake uploader: %v", err)
	}
	defer conn.Close()

	_, err = streamFile(conn, destPath, 8, time.Second, nil)
	if err == nil {
		t.Fatal("streamFile: expected an error for a read-only destination directory, got nil")
	}
	var de *diskError
	if !errors.As(err, &de) {
		t.Errorf("streamFile error = %v, want a diskError (local filesystem failure)", err)
	}
}

func TestStreamFileNetworkFailureIsNotDiskError(t *testing.T) {
	// The peer sends only half the payload and closes: the failure is the
	// network/peer's, so it must NOT be tagged diskError.
	payload := bytes.Repeat([]byte("abcdefgh"), 16) // 128 bytes
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake uploader: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		off := &file.Offset{}
		if err := off.Deserialize(conn); err != nil {
			return
		}
		_, _ = conn.Write(payload[:len(payload)/2])
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial fake uploader: %v", err)
	}
	defer conn.Close()

	destPath := filepath.Join(t.TempDir(), "song.flac")
	_, err = streamFile(conn, destPath, int64(len(payload)), time.Second, nil)
	if err == nil {
		t.Fatal("streamFile: expected an error for a short stream, got nil")
	}
	var de *diskError
	if errors.As(err, &de) {
		t.Errorf("streamFile error = %v, must not be a diskError (peer/network failure)", err)
	}
}
