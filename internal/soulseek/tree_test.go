package soulseek

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/distributed"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/server"
)

type treeServerConn struct {
	mu     sync.Mutex
	frames [][]byte
	closed chan struct{}
	once   sync.Once
}

func newTreeServerConn() *treeServerConn { return &treeServerConn{closed: make(chan struct{})} }
func (c *treeServerConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, net.ErrClosed
}
func (c *treeServerConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.frames = append(c.frames, append([]byte(nil), p...))
	c.mu.Unlock()
	return len(p), nil
}
func (c *treeServerConn) Close() error                   { c.once.Do(func() { close(c.closed) }); return nil }
func (*treeServerConn) LocalAddr() net.Addr              { return dummyAddr("server-local") }
func (*treeServerConn) RemoteAddr() net.Addr             { return dummyAddr("server-remote") }
func (*treeServerConn) SetDeadline(time.Time) error      { return nil }
func (*treeServerConn) SetReadDeadline(time.Time) error  { return nil }
func (*treeServerConn) SetWriteDeadline(time.Time) error { return nil }
func (c *treeServerConn) snapshot() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.frames))
	for i := range c.frames {
		out[i] = append([]byte(nil), c.frames[i]...)
	}
	return out
}

func serverFrameCode(frame []byte) uint32 {
	if len(frame) < 8 {
		return 0
	}
	return binary.LittleEndian.Uint32(frame[4:8])
}

func startTreeClient(t *testing.T) (*Client, *treeServerConn, uint64) {
	t.Helper()
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	startSessionLifecycle(t, c)
	conn := newTreeServerConn()
	const generation = 1
	c.mu.Lock()
	c.serverConn = conn
	c.serverGeneration = generation
	c.serverCancel = func() {}
	c.mu.Unlock()
	if err := c.tree.activate(generation); err != nil {
		t.Fatalf("activate tree: %v", err)
	}
	t.Cleanup(func() {
		c.tree.deactivate(generation)
		_ = conn.Close()
	})
	return c, conn, generation
}

func searchBody(t *testing.T, username, query string, token soul.Token, unknown ...byte) []byte {
	t.Helper()
	msg := &distributed.Search{Username: username, Query: query, Token: token}
	frame, err := msg.Serialize(msg)
	if err != nil {
		t.Fatalf("serialize search: %v", err)
	}
	return append(append([]byte(nil), frame[5:]...), unknown...)
}

func readDistributedWire(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	reader, _, _, err := distributed.Read(conn)
	if err != nil {
		t.Fatalf("read distributed frame: %v", err)
	}
	wire, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read buffered distributed frame: %v", err)
	}
	return wire
}

func decodeMetadataBatch(t *testing.T, batch []byte) (int32, string) {
	t.Helper()
	reader := bytes.NewReader(batch)
	levelReader, _, levelCode, err := distributed.Read(reader)
	if err != nil || levelCode != distributed.CodeBranchLevel {
		t.Fatalf("read metadata level: code=%d err=%v", levelCode, err)
	}
	var level distributed.BranchLevel
	if err := level.Deserialize(levelReader); err != nil {
		t.Fatalf("decode metadata level: %v", err)
	}
	rootReader, _, rootCode, err := distributed.Read(reader)
	if err != nil || rootCode != distributed.CodeBranchRoot {
		t.Fatalf("read metadata root: code=%d err=%v", rootCode, err)
	}
	var root distributed.BranchRoot
	if err := root.Deserialize(rootReader); err != nil {
		t.Fatalf("decode metadata root: %v", err)
	}
	if reader.Len() != 0 {
		t.Fatalf("metadata batch has %d trailing bytes", reader.Len())
	}
	return level.Level, root.Root
}

func waitTree(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(message)
}

func TestDistributedTreeInitialAdvertisements(t *testing.T) {
	c, conn, _ := startTreeClient(t)
	frames := conn.snapshot()
	want := []uint32{uint32(server.CodeHaveNoParent), uint32(server.CodeBranchRoot), uint32(server.CodeBranchLevel), uint32(server.CodeAcceptChildren)}
	if len(frames) != len(want) {
		t.Fatalf("initial frame count = %d, want %d", len(frames), len(want))
	}
	for i, code := range want {
		if got := serverFrameCode(frames[i]); got != code {
			t.Fatalf("initial frame %d code = %d, want %d", i, got, code)
		}
	}
	if binary.LittleEndian.Uint32(frames[2][8:12]) != 0 || frames[3][8] != 0 {
		t.Fatal("initial level/AcceptChildren did not advertise 0/false")
	}
	c.tree.mu.Lock()
	root, level := c.tree.branchRoot, c.tree.branchLevel
	c.tree.mu.Unlock()
	if root != "me" || level != 0 {
		t.Fatalf("initial metadata = %q/%d, want me/0", root, level)
	}
}

func TestServerEmbeddedRawWireClientHandlingAndPeerPrecedence(t *testing.T) {
	c, _, generation := startTreeClient(t)
	body := searchBody(t, "wire-user", "wire-query", 77, 0xde, 0xad)
	payload := make([]byte, 5+len(body))
	binary.LittleEndian.PutUint32(payload[:4], uint32(server.CodeEmbeddedMessage))
	payload[4] = byte(distributed.CodeSearch)
	copy(payload[5:], body)
	rawFrame := packFrame(payload)

	parentSide, parentRemote := net.Pipe()
	defer parentRemote.Close()
	parent := c.newSession(parentSide, sessionKey{username: "peer-parent", connType: distributed.ConnectionType}, sessionInitiatorLocal, sessionRoleParent, generation, nil)
	defer parent.Close(errors.New("test complete"))
	callback := make(chan distributed.Search, 1)
	c.tree.mu.Lock()
	c.tree.parent = parent
	c.tree.onSearch = func(search distributed.Search, _ []byte) { callback <- search }
	c.tree.mu.Unlock()

	if err := c.handleMessage(context.Background(), server.CodeEmbeddedMessage, bytes.NewReader(rawFrame)); err != nil {
		t.Fatalf("handle raw embedded with peer parent: %v", err)
	}
	c.tree.mu.Lock()
	keptParent, serverParent := c.tree.parent, c.tree.serverParent
	c.tree.mu.Unlock()
	if keptParent != parent || serverParent {
		t.Fatalf("server embedded replaced peer parent: parent=%p serverParent=%v", keptParent, serverParent)
	}
	select {
	case <-callback:
		t.Fatal("ignored server search reached callback")
	default:
	}

	c.tree.mu.Lock()
	c.tree.parent = nil
	c.tree.mu.Unlock()
	if err := c.handleMessage(context.Background(), server.CodeEmbeddedMessage, bytes.NewReader(rawFrame)); err != nil {
		t.Fatalf("handle raw embedded as server root: %v", err)
	}
	select {
	case got := <-callback:
		if got.Username != "wire-user" || got.Query != "wire-query" || got.Token != 77 {
			t.Fatalf("decoded raw search = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("raw server wire search did not reach tree callback")
	}

	for _, malformed := range [][]byte{
		packFrame([]byte{byte(server.CodeEmbeddedMessage), 0, 0, 0}),
		packFrame([]byte{byte(server.CodeEmbeddedMessage), 0, 0, 0, byte(distributed.CodeSearch), 0}),
		packFrame([]byte{byte(server.CodeEmbeddedMessage), 0, 0, 0, byte(distributed.CodeBranchRoot)}),
	} {
		if err := c.handleMessage(context.Background(), server.CodeEmbeddedMessage, bytes.NewReader(malformed)); err != nil {
			t.Fatalf("malformed/unsupported embedded frame dropped server: %v", err)
		}
	}
}

func TestServerEmbeddedRootCapacityAndRawForwarding(t *testing.T) {
	c, conn, generation := startTreeClient(t)
	c.tree.updateParentMinSpeed(generation, 100)
	c.tree.updateParentRatio(generation, 2)
	c.tree.updateUploadSpeed(generation, 200) // min(200 / 2 / 100, 10) = 1
	if err := c.tree.handleServerEmbedded(generation, server.EmbeddedMessage{Code: distributed.CodeSearch, Message: searchBody(t, "first", "root", 1)}); err != nil {
		t.Fatal(err)
	}

	childSide, childRemote := net.Pipe()
	defer childRemote.Close()
	child := c.newSession(childSide, sessionKey{username: "child", connType: distributed.ConnectionType}, sessionInitiatorRemote, sessionRoleChild, generation, nil)
	c.registerSession(child)
	var level distributed.BranchLevel
	if err := level.Deserialize(bytes.NewReader(readDistributedWire(t, childRemote))); err != nil {
		t.Fatal(err)
	}
	var root distributed.BranchRoot
	if err := root.Deserialize(bytes.NewReader(readDistributedWire(t, childRemote))); err != nil {
		t.Fatal(err)
	}
	if level.Level != 0 || root.Root != "me" {
		t.Fatalf("child metadata = %d/%q, want 0/me", level.Level, root.Root)
	}

	body := searchBody(t, "alice", "raw", 9, 0xde, 0xad)
	wantWire := rawDistributedSearchFrame(body)
	callback := make(chan []byte, 1)
	c.tree.mu.Lock()
	c.tree.onSearch = func(_ distributed.Search, wire []byte) { callback <- wire }
	c.tree.mu.Unlock()
	if err := c.tree.handleServerEmbedded(generation, server.EmbeddedMessage{Code: distributed.CodeSearch, Message: body}); err != nil {
		t.Fatal(err)
	}
	if got := readDistributedWire(t, childRemote); !bytes.Equal(got, wantWire) {
		t.Fatalf("forwarded server embedded = %x, want raw code-3 %x", got, wantWire)
	}
	if got := <-callback; !bytes.Equal(got, wantWire) {
		t.Fatalf("callback wire = %x, want %x", got, wantWire)
	}

	overSide, overRemote := net.Pipe()
	defer overRemote.Close()
	over := c.newSession(overSide, sessionKey{username: "extra", connType: distributed.ConnectionType}, sessionInitiatorRemote, sessionRoleChild, generation, nil)
	c.registerSession(over)
	select {
	case <-over.done:
	case <-time.After(time.Second):
		t.Fatal("over-capacity child was not rejected")
	}
	child.Close(errors.New("remove child"))

	trueCount, falseAfterTrue := 0, false
	for _, frame := range conn.snapshot() {
		if serverFrameCode(frame) != uint32(server.CodeAcceptChildren) || len(frame) <= 8 {
			continue
		}
		if frame[8] == 1 {
			trueCount++
		} else if trueCount > 0 {
			falseAfterTrue = true
		}
	}
	if trueCount < 2 || !falseAfterTrue {
		t.Fatalf("AcceptChildren transitions true=%d false-after-true=%v", trueCount, falseAfterTrue)
	}
}

func TestPossibleParentsSilentFirstDoesNotStarveValidLater(t *testing.T) {
	c, conn, generation := startTreeClient(t)
	c.cfg.parentCandidateTimeout = 500 * time.Millisecond

	firstLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer firstLn.Close()
	firstSeen := make(chan time.Time, 1)
	go func() {
		pc, err := firstLn.Accept()
		if err != nil {
			return
		}
		defer pc.Close()
		_, _, _, _ = peer.Read(peer.CodeInit(0), pc, false)
		firstSeen <- time.Now()
		time.Sleep(time.Second)
	}()

	secondLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer secondLn.Close()
	secondSeen := make(chan time.Time, 1)
	go func() {
		pc, err := secondLn.Accept()
		if err != nil {
			return
		}
		defer pc.Close()
		reader, _, _, err := peer.Read(peer.CodeInit(0), pc, false)
		if err != nil {
			return
		}
		var init peer.PeerInit
		if init.Deserialize(reader) != nil || init.ConnectionType != distributed.ConnectionType {
			return
		}
		secondSeen <- time.Now()
		_, _ = distributed.Write(pc, &distributed.BranchLevel{Level: 0})
		_, _ = distributed.Write(pc, &distributed.BranchRoot{Root: "ignored-for-level-zero"})
		_, _ = distributed.Write(pc, &distributed.Search{Username: "alice", Token: 42, Query: "needle"})
		_, _ = io.Copy(io.Discard, pc)
	}()

	first := firstLn.Addr().(*net.TCPAddr)
	second := secondLn.Addr().(*net.TCPAddr)
	c.tree.offerParents(c.lifecycleContext(), generation, []server.Parent{
		{Username: "silent", IP: first.IP, Port: first.Port},
		{Username: "parent", IP: second.IP, Port: second.Port},
	})
	var firstAt time.Time
	select {
	case firstAt = <-firstSeen:
	case <-time.After(time.Second):
		t.Fatal("first candidate was not tried")
	}
	select {
	case secondAt := <-secondSeen:
		if delay := secondAt.Sub(firstAt); delay >= c.cfg.parentCandidateTimeout/2 {
			t.Fatalf("valid later candidate waited %v behind silent first candidate", delay)
		}
	case <-time.After(c.cfg.parentCandidateTimeout / 2):
		t.Fatal("valid later candidate was starved by silent first candidate")
	}
	waitTree(t, func() bool {
		c.tree.mu.Lock()
		defer c.tree.mu.Unlock()
		return c.tree.parent != nil && c.tree.parent.key.username == "parent"
	}, "metadata plus first valid search did not adopt parent")
	c.tree.mu.Lock()
	level, root := c.tree.branchLevel, c.tree.branchRoot
	c.tree.mu.Unlock()
	if level != 1 || root != "parent" {
		t.Fatalf("adopted metadata = %d/%q, want 1/parent", level, root)
	}
	for _, frame := range conn.snapshot() {
		if code := serverFrameCode(frame); code == uint32(server.CodeGetPeerAddress) || code == uint32(server.CodeConnectToPeer) {
			t.Fatalf("candidate used forbidden address/indirect request code %d", code)
		}
	}
}

func TestChildAdmissionMetadataBatchCannotBeOvertakenByUpdate(t *testing.T) {
	for i := 0; i < 50; i++ {
		c, _, generation := startTreeClient(t)
		childSide, childRemote := net.Pipe()
		child := c.newSession(childSide, sessionKey{username: "ordered-child", connType: distributed.ConnectionType}, sessionInitiatorRemote, sessionRoleChild, generation, nil)
		c.tree.mu.Lock()
		c.tree.serverParent = true
		c.tree.capacity = 1
		c.tree.mu.Unlock()

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			c.tree.established(child)
		}()
		go func() {
			defer wg.Done()
			<-start
			c.tree.mu.Lock()
			c.tree.branchLevel = 8
			c.tree.branchRoot = "new-root"
			failed := queueChildMetadata(c.tree.childSnapshotLocked(), 8, "new-root")
			c.tree.mu.Unlock()
			closeOverflowedChildren(failed)
		}()
		close(start)
		wg.Wait()

		var batches [][]byte
		for {
			select {
			case batch := <-child.writes:
				batches = append(batches, batch)
			default:
				goto drained
			}
		}
	drained:
		if len(batches) < 1 || len(batches) > 2 {
			t.Fatalf("metadata queue commands = %d, want one or two atomic batches", len(batches))
		}
		lastLevel, lastRoot := decodeMetadataBatch(t, batches[len(batches)-1])
		if lastLevel != 8 || lastRoot != "new-root" {
			t.Fatalf("last metadata = %d/%q, want 8/new-root", lastLevel, lastRoot)
		}
		if len(batches) == 2 {
			firstLevel, firstRoot := decodeMetadataBatch(t, batches[0])
			if firstLevel != 0 || firstRoot != "me" {
				t.Fatalf("ordered admission metadata = %d/%q then %d/%q", firstLevel, firstRoot, lastLevel, lastRoot)
			}
		}
		child.Close(errors.New("test complete"))
		_ = childRemote.Close()
	}
}

func TestParentUpdatesSearchForwardingLossAndChildRejection(t *testing.T) {
	c, conn, generation := startTreeClient(t)
	c.tree.updateParentMinSpeed(generation, 0)
	c.tree.updateParentRatio(generation, 1)
	c.tree.updateUploadSpeed(generation, 100)

	parentSide, parentRemote := net.Pipe()
	defer parentRemote.Close()
	parent := c.newSession(parentSide, sessionKey{username: "parent", connType: distributed.ConnectionType}, sessionInitiatorLocal, sessionRoleParent, generation, nil)
	c.tree.mu.Lock()
	c.tree.candidateUsers["parent"] = struct{}{}
	c.tree.currentCandidate = "parent"
	c.tree.candidateCancel = func() {}
	c.tree.mu.Unlock()
	c.registerSession(parent)
	if _, err := parentRemote.Write([]byte{1, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	select {
	case <-parent.done:
		t.Fatal("valid distributed ping closed candidate session")
	default:
	}
	_, _ = distributed.Write(parentRemote, &distributed.BranchLevel{Level: 2})
	_, _ = distributed.Write(parentRemote, &distributed.BranchRoot{Root: "root"})
	_, _ = distributed.Write(parentRemote, &distributed.Search{Username: "alice", Token: 1, Query: "adopt"})
	waitTree(t, func() bool {
		c.tree.mu.Lock()
		defer c.tree.mu.Unlock()
		return c.tree.parent == parent
	}, "parent was not adopted")

	childSide, childRemote := net.Pipe()
	defer childRemote.Close()
	child := c.newSession(childSide, sessionKey{username: "child", connType: distributed.ConnectionType}, sessionInitiatorRemote, sessionRoleChild, generation, nil)
	c.registerSession(child)
	_ = readDistributedWire(t, childRemote)
	_ = readDistributedWire(t, childRemote)

	// Soulseek NS sends this exact code-0 D ping frame. It is a no-op for an
	// adopted parent and must not desynchronize or close the session.
	if _, err := parentRemote.Write([]byte{1, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	select {
	case <-parent.done:
		t.Fatal("valid distributed ping closed parent session")
	default:
	}

	_, _ = distributed.Write(parentRemote, &distributed.BranchLevel{Level: 3})
	var level distributed.BranchLevel
	if err := level.Deserialize(bytes.NewReader(readDistributedWire(t, childRemote))); err != nil || level.Level != 4 {
		t.Fatalf("level update = %d, err %v", level.Level, err)
	}
	_, _ = distributed.Write(parentRemote, &distributed.BranchRoot{Root: "new-root"})
	var root distributed.BranchRoot
	if err := root.Deserialize(bytes.NewReader(readDistributedWire(t, childRemote))); err != nil || root.Root != "new-root" {
		t.Fatalf("root update = %q, err %v", root.Root, err)
	}

	plain := rawDistributedSearchFrame(searchBody(t, "plain-user", "plain", 2, 0xaa, 0xbb))
	if _, err := parentRemote.Write(plain); err != nil {
		t.Fatal(err)
	}
	if got := readDistributedWire(t, childRemote); !bytes.Equal(got, plain) {
		t.Fatalf("plain forwarding = %x, want %x", got, plain)
	}
	body := searchBody(t, "legacy-user", "legacy", 3, 0xca, 0xfe)
	// Older SoulseekQt used a uint32 outer code for this deprecated peer
	// wrapper. Write that historical frame by hand so compatibility is tested
	// against real wire bytes rather than this package's serializer.
	legacy := make([]byte, 9+len(body))
	binary.LittleEndian.PutUint32(legacy[:4], uint32(5+len(body)))
	binary.LittleEndian.PutUint32(legacy[4:8], uint32(distributed.CodeEmbeddedMessage))
	legacy[8] = byte(distributed.CodeSearch)
	copy(legacy[9:], body)
	if _, err := parentRemote.Write(legacy); err != nil {
		t.Fatal(err)
	}
	if got, want := readDistributedWire(t, childRemote), rawDistributedSearchFrame(body); !bytes.Equal(got, want) {
		t.Fatalf("deprecated forwarding = %x, want raw %x", got, want)
	}

	// The same raw ping is not accepted from a child; child-origin traffic is
	// still rejected before no-op dispatch.
	if _, err := childRemote.Write([]byte{1, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.done:
	case <-time.After(time.Second):
		t.Fatal("child-origin ping did not close child")
	}
	before := len(conn.snapshot())
	parent.Close(errors.New("parent lost"))
	waitTree(t, func() bool {
		c.tree.mu.Lock()
		defer c.tree.mu.Unlock()
		return c.tree.parent == nil && c.tree.branchRoot == "me" && c.tree.branchLevel == 0
	}, "parent loss did not reset no-parent state")
	after := conn.snapshot()
	foundNoParent := false
	for _, frame := range after[before:] {
		if serverFrameCode(frame) == uint32(server.CodeHaveNoParent) {
			foundNoParent = true
			break
		}
	}
	if len(after) < before+4 || !foundNoParent {
		t.Fatal("parent loss did not re-advertise no-parent state")
	}
}

func TestCandidateDeadlineVersusFinalSearchIsAtomic(t *testing.T) {
	c, _, generation := startTreeClient(t)
	for i := 0; i < 50; i++ {
		local, remote := net.Pipe()
		session := c.newSession(local, sessionKey{username: "deadline-" + string(rune('a'+i)), connType: distributed.ConnectionType}, sessionInitiatorLocal, sessionRoleParent, generation, nil)
		c.sessions.Register(session)

		level := int32(0)
		c.tree.mu.Lock()
		epoch := c.tree.epoch
		c.tree.candidateUsers[session.key.username] = struct{}{}
		c.tree.candidates[session] = &parentCandidateState{
			session: session,
			epoch:   epoch,
			level:   &level,
			root:    session.key.username,
			signal:  make(chan struct{}, 1),
		}
		c.tree.mu.Unlock()

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			c.tree.retireCandidate(session, generation, epoch, errors.New("candidate deadline"))
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = c.tree.handleSearch(session, distributed.Search{Username: "last", Token: 1, Query: "at-deadline"}, rawDistributedSearchFrame(searchBody(t, "last", "at-deadline", 1)))
		}()
		close(start)
		wg.Wait()

		c.tree.mu.Lock()
		adopted := c.tree.parent == session
		_, candidateStillPresent := c.tree.candidates[session]
		c.tree.mu.Unlock()
		if adopted {
			select {
			case <-session.done:
				t.Fatal("deadline closed candidate after it won promotion")
			default:
			}
			session.Close(errors.New("next iteration"))
		} else {
			select {
			case <-session.done:
			default:
				t.Fatal("deadline loser remained open without promotion")
			}
		}
		if candidateStillPresent {
			t.Fatal("deadline/promotion race left stale candidate state")
		}
		_ = remote.Close()
	}
}

func TestCandidateStaleAfterReset(t *testing.T) {
	c, _, generation := startTreeClient(t)
	c.cfg.parentCandidateTimeout = time.Second
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		pc, err := ln.Accept()
		if err != nil {
			return
		}
		if _, _, _, err := peer.Read(peer.CodeInit(0), pc, false); err != nil {
			_ = pc.Close()
			return
		}
		accepted <- pc
	}()
	addr := ln.Addr().(*net.TCPAddr)
	c.tree.offerParents(c.lifecycleContext(), generation, []server.Parent{{Username: "late", IP: addr.IP, Port: addr.Port}})
	var remote net.Conn
	select {
	case remote = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("candidate did not connect")
	}
	defer remote.Close()
	c.tree.reset(generation)
	_, _ = distributed.Write(remote, &distributed.BranchLevel{Level: 0})
	_, _ = distributed.Write(remote, &distributed.Search{Username: "late", Token: 1, Query: "stale"})
	time.Sleep(30 * time.Millisecond)
	c.tree.mu.Lock()
	parent, root, level := c.tree.parent, c.tree.branchRoot, c.tree.branchLevel
	c.tree.mu.Unlock()
	if parent != nil || root != "me" || level != 0 {
		t.Fatalf("stale candidate changed reset state: %v %q/%d", parent, root, level)
	}
	if c.sessions.Get(sessionKey{username: "late", connType: distributed.ConnectionType}) != nil {
		t.Fatal("stale candidate session survived reset")
	}
}

func TestDistributedValidationSlowChildAndReset(t *testing.T) {
	c, conn, generation := startTreeClient(t)
	badSide, badRemote := net.Pipe()
	bad := c.newSession(badSide, sessionKey{username: "bad", connType: distributed.ConnectionType}, sessionInitiatorLocal, sessionRoleParent, generation, nil)
	c.tree.mu.Lock()
	c.tree.candidateUsers["bad"] = struct{}{}
	c.tree.currentCandidate = "bad"
	c.tree.mu.Unlock()
	c.registerSession(bad)
	_, _ = distributed.Write(badRemote, &distributed.BranchLevel{Level: -1})
	select {
	case <-bad.done:
	case <-time.After(time.Second):
		t.Fatal("negative branch level did not close candidate")
	}
	_ = badRemote.Close()

	slowA, slowB := net.Pipe()
	defer slowB.Close()
	slow := c.newSession(slowA, sessionKey{username: "slow", connType: distributed.ConnectionType}, sessionInitiatorRemote, sessionRoleChild, generation, nil)
	slow.writes = make(chan []byte, 1)
	c.sessions.Register(slow)
	c.tree.mu.Lock()
	c.tree.serverParent = true
	c.tree.children[slow] = struct{}{}
	c.tree.capacity = 2
	c.tree.mu.Unlock()
	if !slow.TrySend([]byte("full")) {
		t.Fatal("failed to fill slow child queue")
	}
	c.tree.fanout([]*peerSession{slow}, []byte("next"))
	select {
	case <-slow.done:
	case <-time.After(time.Second):
		t.Fatal("slow child was not isolated")
	}

	before := len(conn.snapshot())
	c.tree.reset(generation)
	if got := len(conn.snapshot()) - before; got < 4 {
		t.Fatalf("reset advertisements = %d, want at least 4", got)
	}
	for _, session := range c.sessions.Snapshot() {
		if session.key.connType == distributed.ConnectionType && session.generation == generation {
			t.Fatalf("D session %q survived reset", session.key.username)
		}
	}
	if err := c.tree.handleServerEmbedded(generation, server.EmbeddedMessage{Code: distributed.CodeSearch, Message: []byte{0}}); err != nil {
		t.Fatalf("malformed embedded search should be ignored: %v", err)
	}
	if err := c.tree.handleServerEmbedded(generation, server.EmbeddedMessage{Code: distributed.CodeBranchRoot}); err != nil {
		t.Fatalf("unsupported embedded code should be ignored: %v", err)
	}
}
