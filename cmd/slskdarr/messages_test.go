package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/soulseek"
)

type fakeIncomingMessageStore struct {
	recorded []core.PrivateMessage
	err      error
}

func (f *fakeIncomingMessageStore) RecordIncomingMessage(ctx context.Context, m core.PrivateMessage) (int64, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	f.recorded = append(f.recorded, m)
	return int64(len(f.recorded)), true, nil
}

func TestMessageSinkPersistsAndMapsFields(t *testing.T) {
	store := &fakeIncomingMessageStore{}
	sink := &messageSink{store: store}

	sentAt := time.Date(2026, 7, 25, 12, 30, 0, 0, time.FixedZone("CEST", 2*3600))
	msg := soulseek.PrivateMessage{
		ServerMessageID: 42,
		Username:        "alice",
		Body:            "hey",
		SentAt:          sentAt,
		Admin:           true,
	}

	if err := sink.HandlePrivateMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandlePrivateMessage: %v", err)
	}

	if len(store.recorded) != 1 {
		t.Fatalf("recorded = %d messages, want 1", len(store.recorded))
	}
	got := store.recorded[0]
	if got.Username != "alice" {
		t.Errorf("Username = %q, want alice", got.Username)
	}
	if got.Direction != core.MessageIncoming {
		t.Errorf("Direction = %q, want %q", got.Direction, core.MessageIncoming)
	}
	if got.Body != "hey" {
		t.Errorf("Body = %q, want hey", got.Body)
	}
	if !got.Admin {
		t.Error("Admin = false, want true")
	}
	if got.ServerMessageID == nil || *got.ServerMessageID != 42 {
		t.Errorf("ServerMessageID = %v, want pointer to 42", got.ServerMessageID)
	}
	if got.SentAt.Location() != time.UTC {
		t.Errorf("SentAt location = %v, want UTC", got.SentAt.Location())
	}
	if !got.SentAt.Equal(sentAt) {
		t.Errorf("SentAt = %v, want %v", got.SentAt, sentAt)
	}
	if got.ReceivedAt.IsZero() {
		t.Error("ReceivedAt is zero, want now")
	}
}

// TestMessageSinkServerMessageIDZeroBecomesNil asserts a wire id of 0 (the
// server sent none) is stored as NULL, not a spurious id-42-like value.
func TestMessageSinkServerMessageIDZeroBecomesNil(t *testing.T) {
	store := &fakeIncomingMessageStore{}
	sink := &messageSink{store: store}

	msg := soulseek.PrivateMessage{Username: "alice", Body: "hey", SentAt: time.Now()}
	if err := sink.HandlePrivateMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandlePrivateMessage: %v", err)
	}

	if got := store.recorded[0].ServerMessageID; got != nil {
		t.Errorf("ServerMessageID = %v, want nil", *got)
	}
}

// TestMessageSinkPropagatesStoreError asserts a store failure is returned
// unchanged, so the caller (soulseek.Client's message worker) withholds the
// ack and the server redelivers at the next login.
func TestMessageSinkPropagatesStoreError(t *testing.T) {
	wantErr := errors.New("db unavailable")
	store := &fakeIncomingMessageStore{err: wantErr}
	sink := &messageSink{store: store}

	msg := soulseek.PrivateMessage{Username: "alice", Body: "hey", SentAt: time.Now()}
	err := sink.HandlePrivateMessage(context.Background(), msg)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}
