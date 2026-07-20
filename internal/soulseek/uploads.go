package soulseek

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
)

const (
	maxWaitingUploads        = 1024
	maxWaitingUploadsPerUser = 64
)

type uploadKey struct {
	username string
	filename string
}

type uploadJob struct {
	key    uploadKey
	active bool
}

type uploadResponseWaiter struct {
	username  string
	responses chan peer.TransferResponse
}

type uploadManager struct {
	mu      sync.Mutex
	c       *Client
	execute func(context.Context, *uploadJob)
	slots   int
	active  int
	waiting []*uploadJob
	byKey   map[uploadKey]*uploadJob
	byToken map[soul.Token]uploadResponseWaiter
	perUser map[string]int
	wake    chan struct{}
}

func newUploadManager(c *Client, slots int) *uploadManager {
	return &uploadManager{c: c, execute: c.runUpload, slots: slots, byKey: make(map[uploadKey]*uploadJob), byToken: make(map[soul.Token]uploadResponseWaiter), perUser: make(map[string]int), wake: make(chan struct{}, 1)}
}

func (m *uploadManager) notify() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *uploadManager) enqueue(username, filename string) error {
	normalized, ok := normalizeVirtualPath(filename)
	if !ok || m.c.shareSnapshot().files[normalized] == nil {
		return peer.ErrFileNotShared
	}
	key := uploadKey{username: username, filename: normalized}
	m.mu.Lock()
	if _, exists := m.byKey[key]; exists {
		m.mu.Unlock()
		return nil
	}
	if len(m.waiting) >= maxWaitingUploads || m.perUser[username] >= maxWaitingUploadsPerUser {
		m.mu.Unlock()
		return peer.ErrTooManyFiles
	}
	job := &uploadJob{key: key}
	m.byKey[key] = job
	m.waiting = append(m.waiting, job)
	m.perUser[username]++
	m.mu.Unlock()
	m.notify()
	return nil
}

func (m *uploadManager) availability() (bool, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active < m.slots && len(m.waiting) == 0, len(m.waiting)
}

func (m *uploadManager) position(key uploadKey) (uint32, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.byKey[key]
	if job == nil {
		return 0, false
	}
	if job.active {
		return 0, true
	}
	for i, waiting := range m.waiting {
		if waiting == job {
			return uint32(i + 1), true
		}
	}
	return 0, false
}

func (m *uploadManager) dispatch(ctx context.Context) {
	defer m.reset()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		}
		for {
			if ctx.Err() != nil {
				return
			}
			m.mu.Lock()
			if m.active >= m.slots || len(m.waiting) == 0 {
				m.mu.Unlock()
				break
			}
			job := m.waiting[0]
			m.waiting = m.waiting[1:]
			job.active = true
			m.active++
			m.mu.Unlock()
			if !m.c.startTracked(func() {
				defer m.complete(job)
				m.execute(ctx, job)
			}) {
				m.complete(job)
				return
			}
		}
	}
}

func (m *uploadManager) reset() {
	m.mu.Lock()
	for _, job := range m.byKey {
		job.active = false
	}
	m.active = 0
	m.waiting = nil
	m.byKey = make(map[uploadKey]*uploadJob)
	m.byToken = make(map[soul.Token]uploadResponseWaiter)
	m.perUser = make(map[string]int)
	m.mu.Unlock()
}

func (m *uploadManager) complete(job *uploadJob) {
	m.mu.Lock()
	if m.byKey[job.key] == job {
		delete(m.byKey, job.key)
		m.perUser[job.key.username]--
		if m.perUser[job.key.username] == 0 {
			delete(m.perUser, job.key.username)
		}
	}
	if job.active {
		job.active = false
		m.active--
	}
	m.mu.Unlock()
	m.notify()
}

func (m *uploadManager) registerToken(token soul.Token, username string, responses chan peer.TransferResponse) {
	m.mu.Lock()
	m.byToken[token] = uploadResponseWaiter{username: username, responses: responses}
	m.mu.Unlock()
}

func (m *uploadManager) unregisterToken(token soul.Token, responses chan peer.TransferResponse) {
	m.mu.Lock()
	if waiter, ok := m.byToken[token]; ok && waiter.responses == responses {
		delete(m.byToken, token)
	}
	m.mu.Unlock()
}

func (m *uploadManager) deliver(username string, response peer.TransferResponse) {
	m.mu.Lock()
	waiter, ok := m.byToken[response.Token]
	m.mu.Unlock()
	if ok && waiter.username == username {
		select {
		case waiter.responses <- response:
		default:
		}
	}
}

type uploadSessionHooks struct {
	c       *Client
	uploads *uploadManager
}

func (*uploadSessionHooks) established(*peerSession)   {}
func (*uploadSessionHooks) closed(*peerSession, error) {}

func (h *uploadSessionHooks) frame(session *peerSession, frame sessionFrame) error {
	if frame.connType != peer.ConnectionType {
		return errUnhandledPeerFrame
	}
	switch peer.Code(frame.code) {
	case peer.CodeQueueUpload:
		msg := &peer.QueueUpload{}
		if err := msg.Deserialize(bytes.NewReader(frame.wire)); err != nil {
			return fmt.Errorf("deserialize queue upload: %w", err)
		}
		if err := h.uploads.enqueue(session.key.username, msg.Filename); err != nil {
			if !sendUploadPeerMessage(session, &peer.UploadDenied{Filename: msg.Filename, Reason: err}) {
				return errors.New("upload denial backpressure")
			}
		}
		return nil
	case peer.CodePlaceInQueueRequest:
		msg := &peer.PlaceInQueueRequest{}
		if err := msg.Deserialize(bytes.NewReader(frame.wire)); err != nil {
			return fmt.Errorf("deserialize place in queue request: %w", err)
		}
		normalized, ok := normalizeVirtualPath(msg.Filename)
		if !ok {
			ok = false
		}
		place, found := h.uploads.position(uploadKey{username: session.key.username, filename: normalized})
		if !ok || !found {
			if !sendUploadPeerMessage(session, &peer.UploadDenied{Filename: msg.Filename, Reason: peer.ErrFileNotShared}) {
				return errors.New("upload denial backpressure")
			}
			return nil
		}
		if !sendUploadPeerMessage(session, &peer.PlaceInQueueResponse{Filename: msg.Filename, Place: place}) {
			return errors.New("queue position response backpressure")
		}
		return nil
	case peer.CodeTransferResponse:
		msg := &peer.TransferResponse{}
		if err := msg.Deserialize(bytes.NewReader(frame.wire)); err != nil {
			return fmt.Errorf("deserialize upload transfer response: %w", err)
		}
		h.uploads.deliver(session.key.username, *msg)
		return nil
	default:
		return errUnhandledPeerFrame
	}
}

type uploadPeerMessage[M any] interface {
	*peer.TransferRequest | *peer.PlaceInQueueResponse | *peer.UploadDenied | *peer.UploadFailed
	Serialize(M) ([]byte, error)
}

func sendUploadPeerMessage[M uploadPeerMessage[M]](session *peerSession, msg M) bool {
	wire, err := msg.Serialize(msg)
	return err == nil && session.TrySend(wire)
}
