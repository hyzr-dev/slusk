package main

import (
	"context"

	"github.com/samuelenocsson/slusk/internal/core"
	"github.com/samuelenocsson/slusk/internal/soulseek"
)

// uploadHistoryStore is the narrow slice of *store.Store uploadSink needs, so
// tests can supply a fake rather than a real database.
type uploadHistoryStore interface {
	RecordUpload(ctx context.Context, e core.UploadHistoryEntry) error
}

// uploadSink persists finished uploads so the history survives a restart
// (issue #325). Unlike messageSink it carries no durability contract with the
// protocol: the peer has already been served whatever happens here, so an error
// is simply returned for the client to log and drop — there is nothing to
// redeliver.
type uploadSink struct {
	store uploadHistoryStore // narrow interface, for tests
}

// RecordUpload implements soulseek.UploadSink. It is a straight field mapping:
// the client has already decided the status, computed the resume-aware byte
// delta and the streaming-phase rate, and chosen a detail string safe to serve
// over the API, so nothing is derived or sanitized here.
func (s *uploadSink) RecordUpload(ctx context.Context, r soulseek.UploadRecord) error {
	return s.store.RecordUpload(ctx, core.UploadHistoryEntry{
		Username:          r.Username,
		Filename:          r.Filename,
		Size:              r.Size,
		BytesSent:         r.BytesSent,
		AvgBytesPerSecond: r.AvgBytesPerSecond,
		Status:            r.Status,
		Detail:            r.Detail,
		StartedAt:         r.StartedAt.UTC(),
		FinishedAt:        r.FinishedAt.UTC(),
	})
}
