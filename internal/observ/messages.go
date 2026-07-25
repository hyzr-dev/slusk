// Package observ: messages.go serves Soulseek private messaging (issue
// #183): GET /api/messages lists conversations, GET /api/messages/{username}
// pages one thread, POST /api/messages/{username} sends a message, and POST
// /api/messages/{username}/read marks a conversation read. observ already
// imports internal/core for its other DTOs (see JobsFunc), so
// ConversationsFunc/ThreadFunc/SendMessageFunc/MarkReadFunc use core types
// directly rather than declaring their own transport shapes the way
// SharesFunc does — there is no soulseek-specific type to keep out.
package observ

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// maxThreadLimit caps the page size GET /api/messages/{username} will honor,
// regardless of the requested ?limit=.
const maxThreadLimit = 500

// defaultThreadLimit is the page size used when ?limit= is absent or invalid.
const defaultThreadLimit = 100

// maxMessageBodyBytes caps an outgoing message body before it ever reaches
// SendMessageFunc. observ cannot import internal/soulseek to reuse its
// maxPrivateMessageBytes (see the package comment), so this uses the shared
// core.MaxPrivateMessageBytes instead of a second, independently maintained
// copy of the same number.
const maxMessageBodyBytes = core.MaxPrivateMessageBytes

// maxSendMessageRequestBytes bounds the raw POST /api/messages/{username} request body
// http.MaxBytesReader is allowed to read, before json.Decode ever runs. It is larger
// than maxMessageBodyBytes, not equal to it, to leave room for the JSON envelope
// (`{"body":"..."}`) and for ordinary escaping (quotes, backslashes, newlines) without
// every legitimate maxMessageBodyBytes-sized message tipping over the limit — the goal
// here is only to stop an unbounded decode of a hostile multi-gigabyte body, not to
// duplicate the exact-length check the handler already does on the decoded value.
const maxSendMessageRequestBytes = maxMessageBodyBytes*2 + 256

// ConversationsFunc lists every private-message conversation, newest activity
// first, for GET /api/messages.
type ConversationsFunc func(ctx context.Context) ([]core.Conversation, error)

// ThreadFunc pages one peer's message history newest-first for GET
// /api/messages/{username}. beforeID > 0 pages backwards from that message id;
// limit is already clamped by the caller.
type ThreadFunc func(ctx context.Context, username string, limit int, beforeID int64) ([]core.PrivateMessage, error)

// SendMessageFunc sends a private message and returns the persisted row for
// POST /api/messages/{username}. Nil when no Soulseek backend capable of
// sending is configured — the handler then answers 503 rather than panicking.
type SendMessageFunc func(ctx context.Context, username, body string) (core.PrivateMessage, error)

// MarkReadFunc marks every unread incoming message from username as read for
// POST /api/messages/{username}/read, returning how many rows were touched.
type MarkReadFunc func(ctx context.Context, username string) (int, error)

// messageDTO is the JSON shape of one message in both GET /api/messages/{username}
// and the POST /api/messages/{username} response.
//
// Body is served verbatim: it is peer-supplied text, not sanitized or escaped
// on this path, and a future web UI consuming it MUST NOT render it as raw
// HTML.
type messageDTO struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Direction string `json:"direction"`
	Body      string `json:"body"`
	SentAt    string `json:"sentAt"`
	Read      bool   `json:"read"`
	Admin     bool   `json:"admin"`
}

func toMessageDTO(m core.PrivateMessage) messageDTO {
	return messageDTO{
		ID:        m.ID,
		Username:  m.Username,
		Direction: string(m.Direction),
		Body:      m.Body,
		SentAt:    m.SentAt.Format(timeFormat),
		Read:      m.ReadAt != nil,
		Admin:     m.Admin,
	}
}

func toMessageDTOs(messages []core.PrivateMessage) []messageDTO {
	dtos := make([]messageDTO, len(messages))
	for i, m := range messages {
		dtos[i] = toMessageDTO(m)
	}
	return dtos
}

// conversationDTO is the JSON shape of one entry in GET /api/messages.
type conversationDTO struct {
	Username      string `json:"username"`
	LastMessage   string `json:"lastMessage"`
	LastMessageAt string `json:"lastMessageAt"`
	LastDirection string `json:"lastDirection"`
	Unread        int    `json:"unread"`
	Total         int    `json:"total"`
}

func toConversationDTO(c core.Conversation) conversationDTO {
	return conversationDTO{
		Username:      c.Username,
		LastMessage:   c.LastMessage,
		LastMessageAt: c.LastMessageAt.Format(timeFormat),
		LastDirection: string(c.LastDirection),
		Unread:        c.Unread,
		Total:         c.Total,
	}
}

// threadResponse is the JSON body of GET /api/messages/{username}.
type threadResponse struct {
	Username string       `json:"username"`
	Messages []messageDTO `json:"messages"`
	HasMore  bool         `json:"hasMore"`
}

// sendMessageRequest is the JSON body of POST /api/messages/{username}.
type sendMessageRequest struct {
	Body string `json:"body"`
}

// markReadResponse is the JSON body of POST /api/messages/{username}/read.
type markReadResponse struct {
	Marked int `json:"marked"`
}

// registerMessages wires GET /api/messages, GET/POST /api/messages/{username}
// and POST /api/messages/{username}/read onto mux.
func registerMessages(mux *http.ServeMux, conversations ConversationsFunc, thread ThreadFunc, send SendMessageFunc, markRead MarkReadFunc) {
	mux.HandleFunc("/api/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if conversations == nil {
			writeConfigError(w, http.StatusServiceUnavailable, "private messaging is not enabled in the configuration", nil)
			return
		}
		convs, err := conversations(r.Context())
		if err != nil {
			writeConfigError(w, http.StatusInternalServerError, "failed to list conversations", nil)
			return
		}
		dtos := make([]conversationDTO, len(convs))
		for i, c := range convs {
			dtos[i] = toConversationDTO(c)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dtos)
	})

	mux.HandleFunc("/api/messages/{username}", func(w http.ResponseWriter, r *http.Request) {
		username := r.PathValue("username")
		if strings.TrimSpace(username) == "" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			serveThread(w, r, thread, username)
		case http.MethodPost:
			serveSendMessage(w, r, send, username)
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/messages/{username}/read", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		username := r.PathValue("username")
		if strings.TrimSpace(username) == "" {
			http.NotFound(w, r)
			return
		}
		if markRead == nil {
			writeConfigError(w, http.StatusServiceUnavailable, "private messaging is not enabled in the configuration", nil)
			return
		}
		marked, err := markRead(r.Context(), username)
		if err != nil {
			writeConfigError(w, http.StatusInternalServerError, "failed to mark conversation read", nil)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(markReadResponse{Marked: marked})
	})
}

func serveThread(w http.ResponseWriter, r *http.Request, thread ThreadFunc, username string) {
	if thread == nil {
		writeConfigError(w, http.StatusServiceUnavailable, "private messaging is not enabled in the configuration", nil)
		return
	}
	limit := parseThreadLimit(r.URL.Query().Get("limit"))
	var beforeID int64
	if raw := r.URL.Query().Get("before"); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			beforeID = parsed
		}
	}

	// Fetch one extra row to detect whether more history remains, then trim
	// it back off before serving.
	messages, err := thread(r.Context(), username, limit+1, beforeID)
	if err != nil {
		writeConfigError(w, http.StatusInternalServerError, "failed to load thread", nil)
		return
	}
	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(threadResponse{
		Username: username,
		Messages: toMessageDTOs(messages),
		HasMore:  hasMore,
	})
}

func parseThreadLimit(raw string) int {
	if raw == "" {
		return defaultThreadLimit
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		return defaultThreadLimit
	}
	if limit > maxThreadLimit {
		return maxThreadLimit
	}
	return limit
}

func serveSendMessage(w http.ResponseWriter, r *http.Request, send SendMessageFunc, username string) {
	if send == nil {
		writeConfigError(w, http.StatusServiceUnavailable, "sending private messages is not enabled in the configuration", nil)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSendMessageRequestBytes)
	var req sendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeConfigError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}

	body := strings.TrimSpace(req.Body)
	if body == "" {
		writeConfigError(w, http.StatusUnprocessableEntity, "message body is empty", nil)
		return
	}
	if len(body) > maxMessageBodyBytes {
		writeConfigError(w, http.StatusUnprocessableEntity, "message body too long", nil)
		return
	}

	msg, err := send(r.Context(), username, body)
	if err != nil {
		// Do not echo err.Error(): observ deliberately does not import
		// internal/soulseek (see the package comment), so it cannot
		// distinguish the underlying cause beyond "the send failed" — and
		// doing so risks leaking soulseek-internal detail into the response,
		// same reasoning as registerShares' default branch.
		writeConfigError(w, http.StatusBadGateway, "failed to send message", nil)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toMessageDTO(msg))
}
