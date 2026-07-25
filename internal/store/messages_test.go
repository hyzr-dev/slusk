package store

import (
	"context"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/store/storetest"
)

func TestRecordIncomingMessageIsIdempotentOnServerID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	serverID := int64(42)

	msg := core.PrivateMessage{
		Username: "alice", Body: "hi", ServerMessageID: &serverID,
		SentAt: now, ReceivedAt: now,
	}

	_, inserted1, err := s.RecordIncomingMessage(ctx, msg)
	if err != nil {
		t.Fatalf("RecordIncomingMessage: %v", err)
	}
	if !inserted1 {
		t.Fatal("first insert: inserted = false, want true")
	}

	id2, inserted2, err := s.RecordIncomingMessage(ctx, msg)
	if err != nil {
		t.Fatalf("RecordIncomingMessage (redelivery): %v", err)
	}
	if inserted2 {
		t.Fatal("redelivered insert: inserted = true, want false")
	}
	if id2 != 0 {
		t.Errorf("redelivered insert: id = %d, want 0 (no row was created)", id2)
	}

	thread, err := s.Thread(ctx, "alice", 10, 0)
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if len(thread) != 1 {
		t.Fatalf("thread has %d messages, want exactly 1 (idempotent insert)", len(thread))
	}
}

func TestRecordIncomingMessageWithoutServerIDAlwaysInserts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	msg := core.PrivateMessage{Username: "bob", Body: "hey", SentAt: now, ReceivedAt: now}

	if _, inserted, err := s.RecordIncomingMessage(ctx, msg); err != nil || !inserted {
		t.Fatalf("first insert: inserted=%v err=%v", inserted, err)
	}
	if _, inserted, err := s.RecordIncomingMessage(ctx, msg); err != nil || !inserted {
		t.Fatalf("second insert: inserted=%v err=%v", inserted, err)
	}

	thread, err := s.Thread(ctx, "bob", 10, 0)
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if len(thread) != 2 {
		t.Fatalf("thread has %d messages, want 2 (id 0 must never dedup)", len(thread))
	}
}

func TestConversationsRollup(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	// alice: one older read IN, one newer unread IN.
	if _, _, err := s.RecordIncomingMessage(ctx, core.PrivateMessage{
		Username: "alice", Body: "old", SentAt: t0, ReceivedAt: t0,
	}); err != nil {
		t.Fatalf("RecordIncomingMessage: %v", err)
	}
	if _, err := s.MarkConversationRead(ctx, "alice", t0.Add(30*time.Second)); err != nil {
		t.Fatalf("MarkConversationRead (alice, before second message): %v", err)
	}
	if _, _, err := s.RecordIncomingMessage(ctx, core.PrivateMessage{
		Username: "alice", Body: "newer unread", SentAt: t0.Add(time.Minute), ReceivedAt: t0.Add(time.Minute),
	}); err != nil {
		t.Fatalf("RecordIncomingMessage: %v", err)
	}

	// bob: one outgoing message, most recent overall.
	if _, err := s.RecordOutgoingMessage(ctx, "bob", "sent to bob", t0.Add(2*time.Minute)); err != nil {
		t.Fatalf("RecordOutgoingMessage: %v", err)
	}

	convs, err := s.Conversations(ctx)
	if err != nil {
		t.Fatalf("Conversations: %v", err)
	}
	if len(convs) != 2 {
		t.Fatalf("got %d conversations, want 2", len(convs))
	}

	if convs[0].Username != "bob" {
		t.Errorf("convs[0].Username = %q, want bob (most recent activity first)", convs[0].Username)
	}
	if convs[0].LastMessage != "sent to bob" || convs[0].LastDirection != core.MessageOutgoing {
		t.Errorf("convs[0] last message/direction = %q/%q, want %q/%q",
			convs[0].LastMessage, convs[0].LastDirection, "sent to bob", core.MessageOutgoing)
	}
	if convs[0].Unread != 0 || convs[0].Total != 1 {
		t.Errorf("convs[0] unread/total = %d/%d, want 0/1", convs[0].Unread, convs[0].Total)
	}

	if convs[1].Username != "alice" {
		t.Errorf("convs[1].Username = %q, want alice", convs[1].Username)
	}
	if convs[1].LastMessage != "newer unread" || convs[1].LastDirection != core.MessageIncoming {
		t.Errorf("convs[1] last message/direction = %q/%q, want %q/%q",
			convs[1].LastMessage, convs[1].LastDirection, "newer unread", core.MessageIncoming)
	}
	if convs[1].Unread != 1 || convs[1].Total != 2 {
		t.Errorf("convs[1] unread/total = %d/%d, want 1/2", convs[1].Unread, convs[1].Total)
	}
}

func TestThreadOrderingAndKeysetPagination(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	var ids []int64
	for i := 0; i < 5; i++ {
		id, err := s.RecordOutgoingMessage(ctx, "carol", "msg", t0.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatalf("RecordOutgoingMessage %d: %v", i, err)
		}
		ids = append(ids, id.ID)
	}

	page1, err := s.Thread(ctx, "carol", 2, 0)
	if err != nil {
		t.Fatalf("Thread page1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 has %d messages, want 2", len(page1))
	}
	if page1[0].ID != ids[4] || page1[1].ID != ids[3] {
		t.Fatalf("page1 ids = [%d %d], want [%d %d] (newest first)", page1[0].ID, page1[1].ID, ids[4], ids[3])
	}

	page2, err := s.Thread(ctx, "carol", 2, page1[1].ID)
	if err != nil {
		t.Fatalf("Thread page2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2 has %d messages, want 2", len(page2))
	}
	if page2[0].ID != ids[2] || page2[1].ID != ids[1] {
		t.Fatalf("page2 ids = [%d %d], want [%d %d] (no overlap with page1)", page2[0].ID, page2[1].ID, ids[2], ids[1])
	}
}

func TestMarkConversationReadOnlyTouchesIncomingUnread(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	if _, _, err := s.RecordIncomingMessage(ctx, core.PrivateMessage{
		Username: "dave", Body: "hi", SentAt: t0, ReceivedAt: t0,
	}); err != nil {
		t.Fatalf("RecordIncomingMessage dave: %v", err)
	}
	if _, err := s.RecordOutgoingMessage(ctx, "dave", "reply", t0.Add(time.Minute)); err != nil {
		t.Fatalf("RecordOutgoingMessage dave: %v", err)
	}
	if _, _, err := s.RecordIncomingMessage(ctx, core.PrivateMessage{
		Username: "erin", Body: "hi from erin", SentAt: t0, ReceivedAt: t0,
	}); err != nil {
		t.Fatalf("RecordIncomingMessage erin: %v", err)
	}

	n, err := s.MarkConversationRead(ctx, "dave", t0.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("MarkConversationRead: %v", err)
	}
	if n != 1 {
		t.Fatalf("MarkConversationRead marked %d, want 1", n)
	}

	n2, err := s.MarkConversationRead(ctx, "dave", t0.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("MarkConversationRead second call: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second MarkConversationRead marked %d, want 0", n2)
	}

	unread, err := s.UnreadMessageCount(ctx)
	if err != nil {
		t.Fatalf("UnreadMessageCount: %v", err)
	}
	if unread != 1 {
		t.Fatalf("UnreadMessageCount = %d, want 1 (erin's message untouched)", unread)
	}
}

func TestRecordOutgoingMessageAppearsInThread(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	sent, err := s.RecordOutgoingMessage(ctx, "frank", "outgoing body", now)
	if err != nil {
		t.Fatalf("RecordOutgoingMessage: %v", err)
	}
	if sent.Direction != core.MessageOutgoing {
		t.Errorf("Direction = %q, want OUT", sent.Direction)
	}
	if sent.ReadAt != nil {
		t.Errorf("ReadAt = %v, want nil", sent.ReadAt)
	}

	thread, err := s.Thread(ctx, "frank", 10, 0)
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if len(thread) != 1 {
		t.Fatalf("thread has %d messages, want 1", len(thread))
	}
	if thread[0].Direction != core.MessageOutgoing || thread[0].ReadAt != nil {
		t.Errorf("thread[0] direction/readAt = %q/%v, want OUT/nil", thread[0].Direction, thread[0].ReadAt)
	}

	unread, err := s.UnreadMessageCount(ctx)
	if err != nil {
		t.Fatalf("UnreadMessageCount: %v", err)
	}
	if unread != 0 {
		t.Fatalf("UnreadMessageCount = %d, want 0 (outgoing messages are never unread)", unread)
	}
}

func TestUnreadMessageCountAcrossPeers(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	for _, username := range []string{"gina", "hank", "gina"} {
		if _, _, err := s.RecordIncomingMessage(ctx, core.PrivateMessage{
			Username: username, Body: "hi", SentAt: now, ReceivedAt: now,
		}); err != nil {
			t.Fatalf("RecordIncomingMessage %s: %v", username, err)
		}
	}

	unread, err := s.UnreadMessageCount(ctx)
	if err != nil {
		t.Fatalf("UnreadMessageCount: %v", err)
	}
	if unread != 3 {
		t.Fatalf("UnreadMessageCount = %d, want 3", unread)
	}
}

func TestPrivateMessagesSurviveReopen(t *testing.T) {
	dsn := storetest.DSN(t)
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, _, err := s.RecordIncomingMessage(context.Background(), core.PrivateMessage{
		Username: "ivy", Body: "before reopen", SentAt: now, ReceivedAt: now,
	}); err != nil {
		t.Fatalf("RecordIncomingMessage: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })

	thread, err := reopened.Thread(context.Background(), "ivy", 10, 0)
	if err != nil {
		t.Fatalf("Thread after reopen: %v", err)
	}
	if len(thread) != 1 || thread[0].Body != "before reopen" {
		t.Fatalf("thread after reopen = %+v, want one message %q", thread, "before reopen")
	}
}
