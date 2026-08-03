package observ

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samuelenocsson/slusk/internal/core"
)

func newMessagesTestHandler(reg *prometheus.Registry, conversations ConversationsFunc, thread ThreadFunc, send SendMessageFunc, markRead MarkReadFunc, presence ...ConversationPresenceFunc) http.Handler {
	deps := testServerDeps(reg)
	deps.Conversations = conversations
	deps.Thread = thread
	deps.Send = send
	deps.MarkRead = markRead
	if len(presence) > 0 {
		deps.ConversationPresence = presence[0]
	}
	return NewServer(deps)
}

// TestConversationsEndpointServesShape asserts the GET /api/messages DTO
// shape, including lastMessageAt formatting.
func TestConversationsEndpointServesShape(t *testing.T) {
	reg := prometheus.NewRegistry()
	lastAt := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	conversations := func(ctx context.Context) ([]core.Conversation, error) {
		return []core.Conversation{
			{Username: "alice", LastMessage: "hi", LastMessageAt: lastAt, LastDirection: core.MessageIncoming, Unread: 2, Total: 5},
		}, nil
	}
	h := newMessagesTestHandler(reg, conversations, noopThread, noopSendMessage, noopMarkRead)

	req := httptest.NewRequest(http.MethodGet, "/api/messages", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got []conversationDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	c := got[0]
	if c.Username != "alice" || c.LastMessage != "hi" || c.Unread != 2 || c.Total != 5 {
		t.Errorf("conversation = %+v, want the alice entry", c)
	}
	if c.LastMessageAt != lastAt.Format(timeFormat) {
		t.Errorf("LastMessageAt = %q, want %q", c.LastMessageAt, lastAt.Format(timeFormat))
	}
	if c.LastDirection != string(core.MessageIncoming) {
		t.Errorf("LastDirection = %q, want %q", c.LastDirection, core.MessageIncoming)
	}
}

func TestConversationsEndpointEnrichesOnlyKnownPresence(t *testing.T) {
	reg := prometheus.NewRegistry()
	lastAt := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	conversations := func(context.Context) ([]core.Conversation, error) {
		return []core.Conversation{
			{Username: "alice", LastMessage: "a", LastMessageAt: lastAt, LastDirection: core.MessageIncoming, Total: 1},
			{Username: "bob", LastMessage: "b", LastMessageAt: lastAt, LastDirection: core.MessageOutgoing, Total: 1},
			{Username: "carol", LastMessage: "c", LastMessageAt: lastAt, LastDirection: core.MessageIncoming, Total: 1},
		}, nil
	}
	var gotUsernames []string
	presence := func(usernames []string) map[string]bool {
		gotUsernames = append([]string(nil), usernames...)
		return map[string]bool{"alice": true, "bob": false, "extra": true}
	}
	h := newMessagesTestHandler(reg, conversations, noopThread, noopSendMessage, noopMarkRead, presence)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/messages", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	wantUsers := []string{"alice", "bob", "carol"}
	if len(gotUsernames) != len(wantUsers) {
		t.Fatalf("presence usernames = %v, want %v", gotUsernames, wantUsers)
	}
	for i := range wantUsers {
		if gotUsernames[i] != wantUsers[i] {
			t.Fatalf("presence usernames = %v, want %v", gotUsernames, wantUsers)
		}
	}
	want := `[{"username":"alice","lastMessage":"a","lastMessageAt":"2026-07-25T12:00:00Z","lastDirection":"IN","unread":0,"total":1,"online":true},{"username":"bob","lastMessage":"b","lastMessageAt":"2026-07-25T12:00:00Z","lastDirection":"OUT","unread":0,"total":1,"online":false},{"username":"carol","lastMessage":"c","lastMessageAt":"2026-07-25T12:00:00Z","lastDirection":"IN","unread":0,"total":1}]`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("body = %s\nwant = %s", got, want)
	}
}

func TestConversationsEndpointNilPresenceOmitsField(t *testing.T) {
	reg := prometheus.NewRegistry()
	conversations := func(context.Context) ([]core.Conversation, error) {
		return []core.Conversation{{Username: "alice"}}, nil
	}
	h := newMessagesTestHandler(reg, conversations, noopThread, noopSendMessage, noopMarkRead)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/messages", nil))
	if strings.Contains(rec.Body.String(), `"online"`) {
		t.Fatalf("nil provider response exposed presence: %s", rec.Body.String())
	}
}

// TestConversationsEndpointEmptyEmitsEmptyArrayNotNull asserts an empty
// result serves "[]", never "null".
func TestConversationsEndpointEmptyEmitsEmptyArrayNotNull(t *testing.T) {
	reg := prometheus.NewRegistry()
	conversations := func(ctx context.Context) ([]core.Conversation, error) { return nil, nil }
	h := newMessagesTestHandler(reg, conversations, noopThread, noopSendMessage, noopMarkRead)

	req := httptest.NewRequest(http.MethodGet, "/api/messages", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("body = %q, want []", rec.Body.String())
	}
}

// TestConversationsEndpointStoreErrorReturns500 asserts a failing
// ConversationsFunc maps to 500.
func TestConversationsEndpointStoreErrorReturns500(t *testing.T) {
	reg := prometheus.NewRegistry()
	conversations := func(ctx context.Context) ([]core.Conversation, error) { return nil, errors.New("db down") }
	h := newMessagesTestHandler(reg, conversations, noopThread, noopSendMessage, noopMarkRead)

	req := httptest.NewRequest(http.MethodGet, "/api/messages", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status code = %d, want 500", rec.Code)
	}
}

// TestConversationsEndpointRejectsNonGET asserts GET-only.
func TestConversationsEndpointRejectsNonGET(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newMessagesTestHandler(reg, noopConversations, noopThread, noopSendMessage, noopMarkRead)

	req := httptest.NewRequest(http.MethodPost, "/api/messages", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status code = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Errorf("Allow header = %q, want %q", allow, "GET")
	}
}

// TestThreadEndpointServesShape asserts GET /api/messages/{username}'s DTO
// shape and default limit.
func TestThreadEndpointServesShape(t *testing.T) {
	reg := prometheus.NewRegistry()
	sentAt := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	var gotLimit int
	var gotBefore int64
	thread := func(ctx context.Context, username string, limit int, beforeID int64) ([]core.PrivateMessage, error) {
		gotLimit = limit
		gotBefore = beforeID
		return []core.PrivateMessage{
			{ID: 1, Username: username, Direction: core.MessageIncoming, Body: "hi", SentAt: sentAt, Admin: true},
		}, nil
	}
	h := newMessagesTestHandler(reg, noopConversations, thread, noopSendMessage, noopMarkRead)

	req := httptest.NewRequest(http.MethodGet, "/api/messages/alice", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	// default limit is fetched +1 internally to detect hasMore
	if gotLimit != defaultThreadLimit+1 {
		t.Errorf("limit passed to ThreadFunc = %d, want %d", gotLimit, defaultThreadLimit+1)
	}
	if gotBefore != 0 {
		t.Errorf("beforeID = %d, want 0", gotBefore)
	}

	var got threadResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Username != "alice" {
		t.Errorf("Username = %q, want alice", got.Username)
	}
	if got.HasMore {
		t.Error("HasMore = true, want false")
	}
	if len(got.Messages) != 1 {
		t.Fatalf("Messages = %+v, want 1 entry", got.Messages)
	}
	m := got.Messages[0]
	if m.ID != 1 || m.Username != "alice" || m.Direction != string(core.MessageIncoming) || m.Body != "hi" || !m.Admin {
		t.Errorf("message = %+v, want the persisted row", m)
	}
	if m.Read {
		t.Error("Read = true, want false (ReadAt nil)")
	}
	if m.SentAt != sentAt.Format(timeFormat) {
		t.Errorf("SentAt = %q, want %q", m.SentAt, sentAt.Format(timeFormat))
	}
}

// TestThreadEndpointLimitClamping asserts ?limit= is clamped between default
// and max.
func TestThreadEndpointLimitClamping(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantLimit int
	}{
		{name: "explicit within range", query: "?limit=10", wantLimit: 11},
		{name: "over max clamps", query: "?limit=999999", wantLimit: maxThreadLimit + 1},
		{name: "invalid falls back to default", query: "?limit=notanumber", wantLimit: defaultThreadLimit + 1},
		{name: "zero falls back to default", query: "?limit=0", wantLimit: defaultThreadLimit + 1},
		{name: "negative falls back to default", query: "?limit=-5", wantLimit: defaultThreadLimit + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			var gotLimit int
			thread := func(ctx context.Context, username string, limit int, beforeID int64) ([]core.PrivateMessage, error) {
				gotLimit = limit
				return nil, nil
			}
			h := newMessagesTestHandler(reg, noopConversations, thread, noopSendMessage, noopMarkRead)

			req := httptest.NewRequest(http.MethodGet, "/api/messages/alice"+tt.query, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
			}
			if gotLimit != tt.wantLimit {
				t.Errorf("limit = %d, want %d", gotLimit, tt.wantLimit)
			}
		})
	}
}

// TestThreadEndpointBeforeIDPagination asserts ?before= id reaches ThreadFunc
// and hasMore is computed from the extra row.
func TestThreadEndpointBeforeIDPagination(t *testing.T) {
	reg := prometheus.NewRegistry()
	var gotBefore int64
	thread := func(ctx context.Context, username string, limit int, beforeID int64) ([]core.PrivateMessage, error) {
		gotBefore = beforeID
		out := make([]core.PrivateMessage, limit)
		for i := range out {
			out[i] = core.PrivateMessage{ID: int64(limit - i), Username: username}
		}
		return out, nil
	}
	h := newMessagesTestHandler(reg, noopConversations, thread, noopSendMessage, noopMarkRead)

	req := httptest.NewRequest(http.MethodGet, "/api/messages/alice?limit=2&before=100", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotBefore != 100 {
		t.Errorf("beforeID = %d, want 100", gotBefore)
	}
	var got threadResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.HasMore {
		t.Error("HasMore = false, want true (extra row fetched)")
	}
	if len(got.Messages) != 2 {
		t.Errorf("len(Messages) = %d, want 2 (trimmed back to requested limit)", len(got.Messages))
	}
}

// TestThreadEndpointUnknownUserReturnsEmpty asserts an unknown username is
// 200 with an empty thread, not a 404 (the store can't distinguish "no
// messages yet" from "never heard of them").
func TestThreadEndpointUnknownUserReturnsEmpty(t *testing.T) {
	reg := prometheus.NewRegistry()
	thread := func(ctx context.Context, username string, limit int, beforeID int64) ([]core.PrivateMessage, error) {
		return nil, nil
	}
	h := newMessagesTestHandler(reg, noopConversations, thread, noopSendMessage, noopMarkRead)

	req := httptest.NewRequest(http.MethodGet, "/api/messages/nobody", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got threadResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Messages) != 0 {
		t.Errorf("Messages = %+v, want empty", got.Messages)
	}
}

// TestThreadEndpointStoreErrorReturns500 asserts a failing ThreadFunc maps to
// 500.
func TestThreadEndpointStoreErrorReturns500(t *testing.T) {
	reg := prometheus.NewRegistry()
	thread := func(ctx context.Context, username string, limit int, beforeID int64) ([]core.PrivateMessage, error) {
		return nil, errors.New("db down")
	}
	h := newMessagesTestHandler(reg, noopConversations, thread, noopSendMessage, noopMarkRead)

	req := httptest.NewRequest(http.MethodGet, "/api/messages/alice", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status code = %d, want 500", rec.Code)
	}
}

// TestSendMessageEndpointHappyPath asserts a valid POST trims the body,
// forwards it to SendMessageFunc, and answers 201 with the persisted DTO.
func TestSendMessageEndpointHappyPath(t *testing.T) {
	reg := prometheus.NewRegistry()
	sentAt := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	var gotUsername, gotBody string
	send := func(ctx context.Context, username, body string) (core.PrivateMessage, error) {
		gotUsername, gotBody = username, body
		return core.PrivateMessage{ID: 7, Username: username, Direction: core.MessageOutgoing, Body: body, SentAt: sentAt}, nil
	}
	h := newMessagesTestHandler(reg, noopConversations, noopThread, send, noopMarkRead)

	req := httptest.NewRequest(http.MethodPost, "/api/messages/alice", strings.NewReader(`{"body":"  hello there  "}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotUsername != "alice" {
		t.Errorf("username passed to SendMessageFunc = %q, want alice", gotUsername)
	}
	if gotBody != "hello there" {
		t.Errorf("body passed to SendMessageFunc = %q, want trimmed %q", gotBody, "hello there")
	}
	var got messageDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != 7 || got.Username != "alice" || got.Body != "hello there" || got.Direction != string(core.MessageOutgoing) {
		t.Errorf("response = %+v, want the persisted message", got)
	}
}

// TestSendMessageEndpointEmptyBodyReturns422 covers both a whitespace-only
// body and an entirely absent one.
func TestSendMessageEndpointEmptyBodyReturns422(t *testing.T) {
	reg := prometheus.NewRegistry()
	called := false
	send := func(ctx context.Context, username, body string) (core.PrivateMessage, error) {
		called = true
		return core.PrivateMessage{}, nil
	}
	h := newMessagesTestHandler(reg, noopConversations, noopThread, send, noopMarkRead)

	for _, body := range []string{`{"body":""}`, `{"body":"   "}`} {
		called = false
		req := httptest.NewRequest(http.MethodPost, "/api/messages/alice", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("body %q: status code = %d, want 422", body, rec.Code)
		}
		if called {
			t.Errorf("body %q: SendMessageFunc was called, want short-circuited before send", body)
		}
	}
}

// TestSendMessageEndpointOversizeBodyReturns422 asserts a body over the cap
// is rejected before reaching SendMessageFunc.
func TestSendMessageEndpointOversizeBodyReturns422(t *testing.T) {
	reg := prometheus.NewRegistry()
	called := false
	send := func(ctx context.Context, username, body string) (core.PrivateMessage, error) {
		called = true
		return core.PrivateMessage{}, nil
	}
	h := newMessagesTestHandler(reg, noopConversations, noopThread, send, noopMarkRead)

	oversized, _ := json.Marshal(sendMessageRequest{Body: strings.Repeat("x", maxMessageBodyBytes+1)})
	req := httptest.NewRequest(http.MethodPost, "/api/messages/alice", strings.NewReader(string(oversized)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status code = %d, want 422", rec.Code)
	}
	if called {
		t.Error("SendMessageFunc was called, want short-circuited before send")
	}
}

// TestSendMessageEndpointMalformedJSONReturns400 asserts undecodable JSON is
// a 400, not a panic or a 422.
func TestSendMessageEndpointMalformedJSONReturns400(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newMessagesTestHandler(reg, noopConversations, noopThread, noopSendMessage, noopMarkRead)

	req := httptest.NewRequest(http.MethodPost, "/api/messages/alice", strings.NewReader(`{not json`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status code = %d, want 400", rec.Code)
	}
}

// TestSendMessageEndpointNilSendReturns503 asserts POST
// /api/messages/{username} answers 503 with a generic message when Send is
// nil (no backend can send).
func TestSendMessageEndpointNilSendReturns503(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newMessagesTestHandler(reg, noopConversations, noopThread, nil, noopMarkRead)

	req := httptest.NewRequest(http.MethodPost, "/api/messages/alice", strings.NewReader(`{"body":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status code = %d, want 503, body = %s", rec.Code, rec.Body.String())
	}
}

// TestSendMessageEndpointSendFailureReturns502WithoutRawMessage asserts an
// arbitrary SendMessageFunc error maps to 502 and never echoes the
// underlying error text - observ cannot import internal/soulseek and must
// not leak its internals.
func TestSendMessageEndpointSendFailureReturns502WithoutRawMessage(t *testing.T) {
	reg := prometheus.NewRegistry()
	sensitive := "soulseek: peer connection reset by 10.0.0.5:2234"
	send := func(ctx context.Context, username, body string) (core.PrivateMessage, error) {
		return core.PrivateMessage{}, errors.New(sensitive)
	}
	h := newMessagesTestHandler(reg, noopConversations, noopThread, send, noopMarkRead)

	req := httptest.NewRequest(http.MethodPost, "/api/messages/alice", strings.NewReader(`{"body":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status code = %d, want 502, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), sensitive) {
		t.Errorf("response body echoed the raw error text: %s", rec.Body.String())
	}
}

// TestMarkReadEndpointHappyPath asserts POST
// /api/messages/{username}/read forwards the decoded username and reports
// how many rows were marked.
func TestMarkReadEndpointHappyPath(t *testing.T) {
	reg := prometheus.NewRegistry()
	var gotUsername string
	markRead := func(ctx context.Context, username string) (int, error) {
		gotUsername = username
		return 3, nil
	}
	h := newMessagesTestHandler(reg, noopConversations, noopThread, noopSendMessage, markRead)

	req := httptest.NewRequest(http.MethodPost, "/api/messages/alice/read", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotUsername != "alice" {
		t.Errorf("username passed to MarkReadFunc = %q, want alice", gotUsername)
	}
	var got markReadResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Marked != 3 {
		t.Errorf("Marked = %d, want 3", got.Marked)
	}
}

// TestMarkReadEndpointStoreErrorReturns500 asserts a failing MarkReadFunc
// maps to 500.
func TestMarkReadEndpointStoreErrorReturns500(t *testing.T) {
	reg := prometheus.NewRegistry()
	markRead := func(ctx context.Context, username string) (int, error) { return 0, errors.New("db down") }
	h := newMessagesTestHandler(reg, noopConversations, noopThread, noopSendMessage, markRead)

	req := httptest.NewRequest(http.MethodPost, "/api/messages/alice/read", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status code = %d, want 500", rec.Code)
	}
}

// TestMessagesEndpointsUsernameDecoding asserts unusual usernames — one with
// a space, one with an encoded slash — reach the deps intact, exercising Go
// 1.22+ ServeMux's wildcard unescaping.
func TestMessagesEndpointsUsernameDecoding(t *testing.T) {
	tests := []struct {
		name      string
		encoded   string
		wantExact string
	}{
		{name: "space", encoded: "john%20doe", wantExact: "john doe"},
		{name: "encoded slash", encoded: "dj%2Fslash", wantExact: "dj/slash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			var gotUsername string
			thread := func(ctx context.Context, username string, limit int, beforeID int64) ([]core.PrivateMessage, error) {
				gotUsername = username
				return nil, nil
			}
			h := newMessagesTestHandler(reg, noopConversations, thread, noopSendMessage, noopMarkRead)

			req := httptest.NewRequest(http.MethodGet, "/api/messages/"+tt.encoded, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
			}
			if gotUsername != tt.wantExact {
				t.Errorf("username reached ThreadFunc as %q, want %q", gotUsername, tt.wantExact)
			}
		})
	}
}

// TestMessagesEndpointsRejectMethodNotAllowed asserts every messages route
// rejects unsupported methods with an Allow header.
func TestMessagesEndpointsRejectMethodNotAllowed(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		path      string
		wantAllow string
	}{
		{name: "conversations", method: http.MethodPost, path: "/api/messages", wantAllow: "GET"},
		{name: "thread/send", method: http.MethodDelete, path: "/api/messages/alice", wantAllow: "GET, POST"},
		{name: "mark read", method: http.MethodGet, path: "/api/messages/alice/read", wantAllow: "POST"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			h := newMessagesTestHandler(reg, noopConversations, noopThread, noopSendMessage, noopMarkRead)

			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status code = %d, want 405", rec.Code)
			}
			if allow := rec.Header().Get("Allow"); allow != tt.wantAllow {
				t.Errorf("Allow header = %q, want %q", allow, tt.wantAllow)
			}
		})
	}
}

// TestThreadEndpointBlankUsernameReturns404 asserts a blank username (the
// trailing-slash route with nothing after it) is a 404.
func TestThreadEndpointBlankUsernameReturns404(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newMessagesTestHandler(reg, noopConversations, noopThread, noopSendMessage, noopMarkRead)

	req := httptest.NewRequest(http.MethodGet, "/api/messages/%20", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status code = %d, want 404", rec.Code)
	}
}
