package soulseek

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/distributed"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/server"
)

const (
	defaultParentCandidateTimeout = 10 * time.Second
	maxConcurrentParentCandidates = 4
)

var (
	errRejectedDistributedPeer = errors.New("rejected distributed peer")
	errInvalidDistributedFrame = errors.New("invalid distributed frame")
)

// composedSessionHooks lets tree and future P-search handling share the sole
// session reader without replacing each other's callbacks.
type composedSessionHooks []sessionHooks

func (h composedSessionHooks) established(s *peerSession) {
	for _, hook := range h {
		hook.established(s)
	}
}

func (h composedSessionHooks) frame(s *peerSession, frame sessionFrame) error {
	for _, hook := range h {
		if err := hook.frame(s, frame); err != nil {
			return err
		}
	}
	return nil
}

func (h composedSessionHooks) closed(s *peerSession, err error) {
	for _, hook := range h {
		hook.closed(s, err)
	}
}

type distributedSearchCallback func(distributed.Search, []byte)

type parentCandidateState struct {
	session    *peerSession
	epoch      uint64
	level      *int32
	root       string
	search     *distributed.Search
	searchWire []byte
	signal     chan struct{}
}

func (c *parentCandidateState) notify() {
	select {
	case c.signal <- struct{}{}:
	default:
	}
}

type distributedTree struct {
	mu sync.Mutex
	c  *Client

	active     bool
	generation uint64
	epoch      uint64

	candidateCancel  context.CancelFunc
	candidateUsers   map[string]struct{}
	currentCandidate string
	candidates       map[*peerSession]*parentCandidateState

	parent       *peerSession
	parentLevel  int32
	serverParent bool
	children     map[*peerSession]struct{}

	branchLevel int32
	branchRoot  string

	parentMinSpeed int
	parentRatio    int
	uploadSpeed    int
	uploadKnown    bool
	capacity       int
	acceptSent     *bool

	onSearch distributedSearchCallback
}

func newDistributedTree(c *Client) *distributedTree {
	return &distributedTree{
		c:              c,
		candidateUsers: make(map[string]struct{}),
		candidates:     make(map[*peerSession]*parentCandidateState),
		children:       make(map[*peerSession]struct{}),
		onSearch:       func(distributed.Search, []byte) {},
	}
}

func (t *distributedTree) activate(generation uint64) error {
	t.mu.Lock()
	if t.candidateCancel != nil {
		t.candidateCancel()
	}
	t.active = true
	t.generation = generation
	t.epoch++
	t.candidateCancel = nil
	t.candidateUsers = make(map[string]struct{})
	t.currentCandidate = ""
	t.candidates = make(map[*peerSession]*parentCandidateState)
	t.parent = nil
	t.parentLevel = 0
	t.serverParent = false
	t.children = make(map[*peerSession]struct{})
	t.branchLevel = 0
	t.branchRoot = t.c.cfg.Username
	t.parentMinSpeed = 0
	t.parentRatio = 0
	t.uploadSpeed = 0
	t.uploadKnown = false
	t.capacity = 0
	initialAccept := false
	t.acceptSent = &initialAccept
	t.mu.Unlock()

	return t.advertiseNoParent(generation)
}

func (t *distributedTree) deactivate(generation uint64) {
	t.mu.Lock()
	if !t.active || t.generation != generation {
		t.mu.Unlock()
		return
	}
	if t.candidateCancel != nil {
		t.candidateCancel()
	}
	t.active = false
	t.epoch++
	t.candidateCancel = nil
	t.candidateUsers = make(map[string]struct{})
	t.currentCandidate = ""
	t.candidates = make(map[*peerSession]*parentCandidateState)
	t.parent = nil
	t.serverParent = false
	t.children = make(map[*peerSession]struct{})
	t.acceptSent = nil
	t.mu.Unlock()
}

func (t *distributedTree) advertiseNoParent(generation uint64) error {
	messages := []func() error{
		func() error { return sendToServerGeneration(t.c, generation, &server.HaveNoParent{Have: true}) },
		func() error {
			return sendToServerGeneration(t.c, generation, &server.BranchRoot{Root: t.c.cfg.Username})
		},
		func() error { return sendToServerGeneration(t.c, generation, &server.BranchLevel{Level: 0}) },
		func() error { return sendToServerGeneration(t.c, generation, &server.AcceptChildren{Accept: false}) },
	}
	for _, send := range messages {
		if err := send(); err != nil {
			return err
		}
	}
	return nil
}

func (t *distributedTree) reset(generation uint64) {
	t.mu.Lock()
	if !t.active || t.generation != generation {
		t.mu.Unlock()
		return
	}
	if t.candidateCancel != nil {
		t.candidateCancel()
	}
	t.epoch++
	t.candidateCancel = nil
	t.candidateUsers = make(map[string]struct{})
	t.currentCandidate = ""
	t.candidates = make(map[*peerSession]*parentCandidateState)
	t.parent = nil
	t.parentLevel = 0
	t.serverParent = false
	t.children = make(map[*peerSession]struct{})
	t.branchLevel = 0
	t.branchRoot = t.c.cfg.Username
	accept := false
	t.acceptSent = &accept
	t.mu.Unlock()

	for _, session := range t.c.sessions.Snapshot() {
		if session.key.connType == distributed.ConnectionType && session.generation == generation {
			session.Close(errors.New("distributed tree reset"))
		}
	}
	if t.c.isServerGenerationActive(generation) {
		if err := t.advertiseNoParent(generation); err != nil && t.c.logger != nil {
			t.c.logger.Debug("advertise distributed reset", "err", err)
		}
	}
}

func (t *distributedTree) offerParents(ctx context.Context, generation uint64, parents []server.Parent) {
	t.mu.Lock()
	if !t.active || t.generation != generation || t.parent != nil || t.serverParent {
		t.mu.Unlock()
		return
	}
	if t.candidateCancel != nil {
		t.candidateCancel()
	}
	var closing []*peerSession
	for session := range t.candidates {
		closing = append(closing, session)
	}
	t.epoch++
	epoch := t.epoch
	workerCtx, cancel := context.WithCancel(ctx)
	t.candidateCancel = cancel
	t.candidateUsers = make(map[string]struct{}, len(parents))
	for _, parent := range parents {
		if parent.Username != "" && parent.Username != t.c.cfg.Username {
			t.candidateUsers[parent.Username] = struct{}{}
		}
	}
	t.currentCandidate = ""
	t.candidates = make(map[*peerSession]*parentCandidateState)
	t.mu.Unlock()

	for _, session := range closing {
		session.Close(errors.New("possible parent list replaced"))
	}
	if !t.c.startTracked(func() { t.runCandidates(workerCtx, generation, epoch, parents) }) {
		cancel()
	}
}

func (t *distributedTree) runCandidates(ctx context.Context, generation, epoch uint64, parents []server.Parent) {
	semaphore := make(chan struct{}, maxConcurrentParentCandidates)
	var wg sync.WaitGroup
	seen := make(map[string]struct{}, len(parents))

	for _, parent := range parents {
		if parent.Username == "" || parent.Username == t.c.cfg.Username || parent.IP == nil || parent.IP.IsUnspecified() || parent.Port <= 0 || parent.Port > 65535 {
			continue
		}
		if _, duplicate := seen[parent.Username]; duplicate {
			continue
		}
		seen[parent.Username] = struct{}{}

		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(parent server.Parent) {
			defer wg.Done()
			defer func() { <-semaphore }()
			t.runCandidate(ctx, generation, epoch, parent)
		}(parent)
	}
	wg.Wait()

	t.mu.Lock()
	if t.candidateWorkerCurrentLocked(generation, epoch) {
		t.currentCandidate = ""
		t.candidateCancel = nil
	}
	t.mu.Unlock()
}

func (t *distributedTree) runCandidate(ctx context.Context, generation, epoch uint64, parent server.Parent) {
	attemptCtx, cancel := context.WithTimeout(ctx, t.c.cfg.parentCandidateTimeout)
	defer cancel()
	target := sessionTarget{
		username: parent.Username,
		connType: distributed.ConnectionType,
		address:  net.JoinHostPort(parent.IP.String(), strconv.Itoa(parent.Port)),
	}
	session, err := t.c.getOrEstablishSession(attemptCtx, target, sessionInitiatorLocal, sessionRoleParent, generation)
	if err != nil {
		return
	}

	t.mu.Lock()
	if !t.candidateWorkerCurrentLocked(generation, epoch) || session.role != sessionRoleParent || session.generation != generation {
		t.mu.Unlock()
		t.retireCandidate(session, generation, epoch, errors.New("stale parent candidate"))
		return
	}
	candidate := t.candidates[session]
	if candidate == nil {
		candidate = &parentCandidateState{session: session, epoch: epoch, signal: make(chan struct{}, 1)}
		t.candidates[session] = candidate
	}
	t.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			t.retireCandidate(session, generation, epoch, ctx.Err())
			return
		case <-attemptCtx.Done():
			t.retireCandidate(session, generation, epoch, errors.New("parent candidate timed out"))
			return
		case <-session.done:
			return
		case <-candidate.signal:
			t.mu.Lock()
			adopted := t.parent == session
			current := t.candidateWorkerCurrentLocked(generation, epoch)
			t.mu.Unlock()
			if adopted || !current {
				return
			}
		}
	}
}

func (t *distributedTree) candidateWorkerCurrentLocked(generation, epoch uint64) bool {
	return t.active && t.generation == generation && t.epoch == epoch && t.parent == nil && !t.serverParent
}

// retireCandidate and tryPromote arbitrate under the same tree lock. Once a
// candidate is removed for timeout/staleness it cannot be promoted, and once
// promoted it cannot be closed by its attempt deadline.
func (t *distributedTree) retireCandidate(session *peerSession, generation, epoch uint64, reason error) {
	t.mu.Lock()
	if t.parent == session {
		t.mu.Unlock()
		return
	}
	candidate := t.candidates[session]
	if candidate == nil || candidate.epoch != epoch || session.generation != generation {
		t.mu.Unlock()
		return
	}
	delete(t.candidates, session)
	t.mu.Unlock()
	session.Close(reason)
}

func (t *distributedTree) established(session *peerSession) {
	if session.key.connType != distributed.ConnectionType {
		return
	}
	t.mu.Lock()
	active := t.active
	t.mu.Unlock()
	// Foundation-only tests and future pre-server P/D setup may create
	// generation-zero sessions. Admission is enforced once a tree generation
	// is active; an inactive generation is torn down by the client lifecycle.
	if !active {
		return
	}

	switch session.role {
	case sessionRoleParent:
		t.mu.Lock()
		_, expected := t.candidateUsers[session.key.username]
		valid := t.active && session.generation == t.generation && t.parent == nil && !t.serverParent && expected
		if valid && t.candidates[session] == nil {
			t.candidates[session] = &parentCandidateState{session: session, epoch: t.epoch, signal: make(chan struct{}, 1)}
		}
		t.mu.Unlock()
		if !valid {
			session.Close(errRejectedDistributedPeer)
		}

	case sessionRoleChild:
		t.mu.Lock()
		_, candidate := t.candidateUsers[session.key.username]
		_, duplicate := t.childByUsernameLocked(session.key.username)
		valid := t.active && session.generation == t.generation && session.key.username != "" &&
			session.key.username != t.c.cfg.Username && !candidate && !duplicate &&
			(t.parent != nil || t.serverParent) && t.capacity > len(t.children)
		queued := false
		if valid {
			t.children[session] = struct{}{}
			// Queue the initial pair while holding the state lock. Metadata
			// transitions use the same lock, so an update cannot overtake this
			// admission snapshot. TrySend only enqueues; it performs no I/O.
			queued = sendChildMetadata(session, t.branchLevel, t.branchRoot)
			if !queued {
				delete(t.children, session)
			}
		}
		acceptUpdate := t.acceptUpdateLocked(false)
		t.mu.Unlock()
		if !valid {
			session.Close(errRejectedDistributedPeer)
			return
		}
		if !queued {
			session.Close(errors.New("distributed child write queue overflow"))
			return
		}
		t.sendAcceptUpdate(session.generation, acceptUpdate)
	}
}

func (t *distributedTree) childByUsernameLocked(username string) (*peerSession, bool) {
	for child := range t.children {
		if child.key.username == username {
			return child, true
		}
	}
	return nil, false
}

func (t *distributedTree) frame(session *peerSession, frame sessionFrame) error {
	if frame.connType != distributed.ConnectionType {
		return nil
	}

	t.mu.Lock()
	_, child := t.children[session]
	candidate := t.candidates[session]
	parent := t.parent == session
	active := t.active && session.generation == t.generation
	t.mu.Unlock()
	if !active || child || (!parent && candidate == nil) {
		return errRejectedDistributedPeer
	}

	switch distributed.Code(frame.code) {
	case distributed.CodeBranchLevel:
		var msg distributed.BranchLevel
		if err := msg.Deserialize(bytes.NewReader(frame.wire)); err != nil {
			return fmt.Errorf("%w: branch level: %v", errInvalidDistributedFrame, err)
		}
		if msg.Level < 0 || msg.Level == math.MaxInt32 {
			return fmt.Errorf("%w: invalid parent branch level %d", errInvalidDistributedFrame, msg.Level)
		}
		return t.handleBranchLevel(session, msg.Level)

	case distributed.CodeBranchRoot:
		var msg distributed.BranchRoot
		if err := msg.Deserialize(bytes.NewReader(frame.wire)); err != nil {
			return fmt.Errorf("%w: branch root: %v", errInvalidDistributedFrame, err)
		}
		if msg.Root == "" {
			return fmt.Errorf("%w: empty branch root", errInvalidDistributedFrame)
		}
		return t.handleBranchRoot(session, msg.Root)

	case distributed.CodeSearch:
		search, err := validateSearchFrame(frame.wire)
		if err != nil {
			return err
		}
		return t.handleSearch(session, search, frame.wire)

	case distributed.CodeEmbeddedMessage:
		if !parent {
			return fmt.Errorf("%w: embedded search from parent candidate", errInvalidDistributedFrame)
		}
		var embedded distributed.EmbeddedMessage
		if err := embedded.Deserialize(bytes.NewReader(frame.wire)); err != nil {
			return fmt.Errorf("%w: embedded message: %v", errInvalidDistributedFrame, err)
		}
		if embedded.Code != distributed.CodeSearch {
			return fmt.Errorf("%w: unsupported embedded code %d", errInvalidDistributedFrame, embedded.Code)
		}
		wire := rawDistributedSearchFrame(embedded.Message)
		search, err := validateSearchFrame(wire)
		if err != nil {
			return err
		}
		return t.handleSearch(session, search, wire)
	default:
		return fmt.Errorf("%w: unsupported distributed code %d", errInvalidDistributedFrame, frame.code)
	}
}

func (t *distributedTree) handleBranchLevel(session *peerSession, parentLevel int32) error {
	t.mu.Lock()
	if t.parent == session {
		previousRoot := t.branchRoot
		root := previousRoot
		if parentLevel == 0 {
			root = session.key.username
		}
		if parentLevel > 0 && root == "" {
			t.mu.Unlock()
			return fmt.Errorf("%w: branch level without root", errInvalidDistributedFrame)
		}
		t.parentLevel = parentLevel
		t.branchLevel = parentLevel + 1
		t.branchRoot = root
		generation, level := t.generation, t.branchLevel
		children := t.childSnapshotLocked()
		var failed []*peerSession
		if root != previousRoot {
			failed = queueChildMetadata(children, level, root)
		} else {
			failed = queueChildLevel(children, level)
		}
		t.mu.Unlock()
		closeOverflowedChildren(failed)
		t.reportLevel(generation, level)
		if root != previousRoot {
			t.reportRoot(generation, root)
		}
		return nil
	}
	candidate := t.candidates[session]
	if candidate == nil {
		t.mu.Unlock()
		return errRejectedDistributedPeer
	}
	levelCopy := parentLevel
	candidate.level = &levelCopy
	if parentLevel == 0 {
		candidate.root = session.key.username
	}
	candidate.notify()
	t.mu.Unlock()
	t.tryPromote(session)
	return nil
}

func (t *distributedTree) handleBranchRoot(session *peerSession, root string) error {
	t.mu.Lock()
	if t.parent == session {
		if t.parentLevel == 0 {
			root = session.key.username
		}
		t.branchRoot = root
		generation := t.generation
		failed := queueChildRoot(t.childSnapshotLocked(), root)
		t.mu.Unlock()
		closeOverflowedChildren(failed)
		t.reportRoot(generation, root)
		return nil
	}
	candidate := t.candidates[session]
	if candidate == nil {
		t.mu.Unlock()
		return errRejectedDistributedPeer
	}
	candidate.root = root
	if candidate.level != nil && *candidate.level == 0 {
		candidate.root = session.key.username
	}
	candidate.notify()
	t.mu.Unlock()
	t.tryPromote(session)
	return nil
}

func (t *distributedTree) handleSearch(session *peerSession, search distributed.Search, wire []byte) error {
	t.mu.Lock()
	if t.parent == session {
		children := t.childSnapshotLocked()
		callback := t.onSearch
		t.mu.Unlock()
		t.fanout(children, wire)
		callback(search, append([]byte(nil), wire...))
		return nil
	}
	candidate := t.candidates[session]
	if candidate == nil {
		t.mu.Unlock()
		return errRejectedDistributedPeer
	}
	if candidate.search == nil {
		searchCopy := search
		candidate.search = &searchCopy
		candidate.searchWire = append([]byte(nil), wire...)
	}
	candidate.notify()
	t.mu.Unlock()
	t.tryPromote(session)
	return nil
}

func (t *distributedTree) tryPromote(session *peerSession) bool {
	t.mu.Lock()
	candidate := t.candidates[session]
	if candidate == nil || candidate.epoch != t.epoch || candidate.level == nil || candidate.search == nil || *candidate.level < 0 || *candidate.level == math.MaxInt32 {
		t.mu.Unlock()
		return false
	}
	root := candidate.root
	if *candidate.level == 0 {
		root = session.key.username
	}
	if root == "" {
		t.mu.Unlock()
		return false
	}
	if !t.active || t.parent != nil || t.serverParent || session.generation != t.generation {
		t.mu.Unlock()
		return false
	}

	t.parent = session
	t.parentLevel = *candidate.level
	t.branchLevel = *candidate.level + 1
	t.branchRoot = root
	t.currentCandidate = ""
	cancel := t.candidateCancel
	t.candidateCancel = nil
	var closing []*peerSession
	for other := range t.candidates {
		if other != session {
			closing = append(closing, other)
		}
	}
	t.candidates = make(map[*peerSession]*parentCandidateState)
	t.candidateUsers = make(map[string]struct{})
	children := t.childSnapshotLocked()
	failed := queueChildMetadata(children, t.branchLevel, root)
	generation, level := t.generation, t.branchLevel
	search := *candidate.search
	wire := append([]byte(nil), candidate.searchWire...)
	callback := t.onSearch
	acceptUpdate := t.acceptUpdateLocked(false)
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	for _, other := range closing {
		other.Close(errors.New("another distributed parent was adopted"))
	}
	if err := sendToServerGeneration(t.c, generation, &server.HaveNoParent{Have: false}); err != nil {
		t.logServerSend("report distributed parent", err)
	}
	t.reportMetadata(generation, level, root)
	t.sendAcceptUpdate(generation, acceptUpdate)
	closeOverflowedChildren(failed)
	t.fanout(children, wire)
	callback(search, wire)
	return true
}

func (t *distributedTree) closed(session *peerSession, _ error) {
	if session.key.connType != distributed.ConnectionType {
		return
	}

	t.mu.Lock()
	if _, ok := t.children[session]; ok {
		delete(t.children, session)
		generation := t.generation
		acceptUpdate := t.acceptUpdateLocked(false)
		t.mu.Unlock()
		t.sendAcceptUpdate(generation, acceptUpdate)
		return
	}
	if candidate := t.candidates[session]; candidate != nil {
		delete(t.candidates, session)
		candidate.notify()
		t.mu.Unlock()
		return
	}
	if t.parent != session {
		t.mu.Unlock()
		return
	}

	t.parent = nil
	t.parentLevel = 0
	t.branchLevel = 0
	t.branchRoot = t.c.cfg.Username
	t.serverParent = false
	t.epoch++
	t.currentCandidate = ""
	t.candidateUsers = make(map[string]struct{})
	t.candidates = make(map[*peerSession]*parentCandidateState)
	accept := false
	t.acceptSent = &accept
	active, generation := t.active && session.generation == t.generation, t.generation
	t.mu.Unlock()
	if active && t.c.isServerGenerationActive(generation) {
		if err := t.advertiseNoParent(generation); err != nil {
			t.logServerSend("advertise parent loss", err)
		}
	}
}

func (t *distributedTree) handleServerEmbedded(generation uint64, embedded server.EmbeddedMessage) error {
	t.mu.Lock()
	if !t.active || t.generation != generation {
		t.mu.Unlock()
		return errNoServerConnection
	}
	// A peer parent takes precedence. The server should not send embedded
	// searches in this state, but delayed/stray frames must not replace it.
	if t.parent != nil {
		t.mu.Unlock()
		return nil
	}
	t.mu.Unlock()

	// Unknown and malformed embedded types are optional protocol input. Ignore
	// them without making the central server connection fail.
	if embedded.Code != distributed.CodeSearch {
		return nil
	}
	wire := rawDistributedSearchFrame(embedded.Message)
	search, err := validateSearchFrame(wire)
	if err != nil {
		return nil
	}

	t.mu.Lock()
	if !t.active || t.generation != generation {
		t.mu.Unlock()
		return errNoServerConnection
	}
	if t.parent != nil {
		t.mu.Unlock()
		return nil
	}
	alreadyRoot := t.serverParent
	if t.candidateCancel != nil {
		t.candidateCancel()
	}
	closing := make([]*peerSession, 0, len(t.candidates))
	for candidate := range t.candidates {
		closing = append(closing, candidate)
	}
	t.parentLevel = 0
	t.serverParent = true
	t.branchLevel = 0
	t.branchRoot = t.c.cfg.Username
	t.epoch++
	t.candidateCancel = nil
	t.currentCandidate = ""
	t.candidateUsers = make(map[string]struct{})
	t.candidates = make(map[*peerSession]*parentCandidateState)
	children := t.childSnapshotLocked()
	var failed []*peerSession
	if !alreadyRoot {
		failed = queueChildMetadata(children, 0, t.c.cfg.Username)
	}
	callback := t.onSearch
	acceptUpdate := t.acceptUpdateLocked(false)
	t.mu.Unlock()

	for _, session := range closing {
		session.Close(errors.New("server became distributed parent"))
	}
	closeOverflowedChildren(failed)
	if !alreadyRoot {
		t.reportMetadata(generation, 0, t.c.cfg.Username)
	}
	t.sendAcceptUpdate(generation, acceptUpdate)
	t.fanout(children, wire)
	callback(search, wire)
	return nil
}

func (t *distributedTree) updateParentMinSpeed(generation uint64, speed int) {
	t.updateCapacityInputs(generation, func() { t.parentMinSpeed = speed })
}

func (t *distributedTree) updateParentRatio(generation uint64, ratio int) {
	t.updateCapacityInputs(generation, func() { t.parentRatio = ratio })
}

func (t *distributedTree) updateUploadSpeed(generation uint64, speed int) {
	t.updateCapacityInputs(generation, func() {
		t.uploadSpeed = speed
		t.uploadKnown = true
	})
}

func (t *distributedTree) updateCapacityInputs(generation uint64, update func()) {
	t.mu.Lock()
	if !t.active || t.generation != generation {
		t.mu.Unlock()
		return
	}
	update()
	t.capacity = t.computeCapacityLocked()
	acceptUpdate := t.acceptUpdateLocked(false)
	t.mu.Unlock()
	t.sendAcceptUpdate(generation, acceptUpdate)
}

func (t *distributedTree) computeCapacityLocked() int {
	if !t.uploadKnown || t.uploadSpeed < t.parentMinSpeed || t.parentRatio <= 0 {
		return 0
	}
	capacity := t.uploadSpeed / t.parentRatio / 100
	if capacity > 10 {
		return 10
	}
	if capacity < 0 {
		return 0
	}
	return capacity
}

func (t *distributedTree) acceptUpdateLocked(force bool) *bool {
	accept := t.active && (t.parent != nil || t.serverParent) && t.capacity > len(t.children)
	if !force && t.acceptSent != nil && *t.acceptSent == accept {
		return nil
	}
	value := accept
	t.acceptSent = &value
	return &value
}

func (t *distributedTree) childSnapshotLocked() []*peerSession {
	children := make([]*peerSession, 0, len(t.children))
	for child := range t.children {
		children = append(children, child)
	}
	return children
}

func (t *distributedTree) sendAcceptUpdate(generation uint64, accept *bool) {
	if accept == nil {
		return
	}
	if err := sendToServerGeneration(t.c, generation, &server.AcceptChildren{Accept: *accept}); err != nil {
		t.logServerSend("update distributed child acceptance", err)
	}
}

func (t *distributedTree) reportMetadata(generation uint64, level int32, root string) {
	t.reportRoot(generation, root)
	t.reportLevel(generation, level)
}

func (t *distributedTree) reportRoot(generation uint64, root string) {
	if err := sendToServerGeneration(t.c, generation, &server.BranchRoot{Root: root}); err != nil {
		t.logServerSend("report distributed branch root", err)
	}
}

func (t *distributedTree) reportLevel(generation uint64, level int32) {
	if err := sendToServerGeneration(t.c, generation, &server.BranchLevel{Level: int(level)}); err != nil {
		t.logServerSend("report distributed branch level", err)
	}
}

func sendChildMetadata(child *peerSession, level int32, root string) bool {
	levelMessage := &distributed.BranchLevel{Level: level}
	levelFrame, err := levelMessage.Serialize(levelMessage)
	if err != nil {
		return false
	}
	rootMessage := &distributed.BranchRoot{Root: root}
	rootFrame, err := rootMessage.Serialize(rootMessage)
	if err != nil {
		return false
	}
	// A single queue command keeps the level/root pair indivisible relative to
	// every other child write. The writer emits the concatenated framed messages
	// in order with one serialized write operation.
	batch := make([]byte, 0, len(levelFrame)+len(rootFrame))
	batch = append(batch, levelFrame...)
	batch = append(batch, rootFrame...)
	return child.TrySend(batch)
}

func queueChildMetadata(children []*peerSession, level int32, root string) (failed []*peerSession) {
	for _, child := range children {
		if !sendChildMetadata(child, level, root) {
			failed = append(failed, child)
		}
	}
	return failed
}

func queueChildLevel(children []*peerSession, level int32) (failed []*peerSession) {
	message := &distributed.BranchLevel{Level: level}
	frame, err := message.Serialize(message)
	if err != nil {
		return append(failed, children...)
	}
	for _, child := range children {
		if !child.TrySend(frame) {
			failed = append(failed, child)
		}
	}
	return failed
}

func queueChildRoot(children []*peerSession, root string) (failed []*peerSession) {
	message := &distributed.BranchRoot{Root: root}
	frame, err := message.Serialize(message)
	if err != nil {
		return append(failed, children...)
	}
	for _, child := range children {
		if !child.TrySend(frame) {
			failed = append(failed, child)
		}
	}
	return failed
}

func closeOverflowedChildren(children []*peerSession) {
	for _, child := range children {
		child.Close(errors.New("distributed child write queue overflow"))
	}
}

func (t *distributedTree) fanout(children []*peerSession, wire []byte) {
	for _, child := range children {
		if !child.TrySend(wire) {
			child.Close(errors.New("distributed child write queue overflow"))
		}
	}
}

func (t *distributedTree) logServerSend(operation string, err error) {
	if t.c.logger != nil {
		t.c.logger.Debug(operation, "err", err)
	}
}

func rawDistributedSearchFrame(body []byte) []byte {
	wire := make([]byte, 5+len(body))
	binary.LittleEndian.PutUint32(wire[:4], uint32(1+len(body)))
	wire[4] = byte(distributed.CodeSearch)
	copy(wire[5:], body)
	return wire
}

func validateSearchFrame(wire []byte) (distributed.Search, error) {
	var search distributed.Search
	if err := search.Deserialize(bytes.NewReader(wire)); err != nil {
		return distributed.Search{}, fmt.Errorf("%w: search: %v", errInvalidDistributedFrame, err)
	}
	return search, nil
}
