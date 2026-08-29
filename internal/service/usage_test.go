package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/engine"
	"github.com/00101010xyz/mcpaw/internal/store/sqlitestore"
	"github.com/00101010xyz/mcpaw/internal/usage"
)

func newTestUsage(t *testing.T) *Usage {
	t.Helper()
	ctx := context.Background()

	store, err := usage.Open(ctx, filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("usage.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	st, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "mcpaw.db"))
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	return NewUsage(store, NewAudit(st.Audit(), nil), nil)
}

func testResolved() *Resolved {
	return &Resolved{Instance: &domain.Instance{ID: "inst_1", Name: "Test instance"}}
}

func TestUsageRecordDoesNothingAtLevelOff(t *testing.T) {
	u := newTestUsage(t)
	ctx := context.Background()
	if err := u.UpdateSettings(ctx, SystemActor(), usage.LevelOff, 0); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	u.Record(ctx, Actor{Type: ActorToken, ID: "tok_1"}, testResolved(), "search",
		map[string]any{"q": "hello"}, &engine.Result{StatusCode: 200, Text: "ok"}, nil, 5*time.Millisecond)

	entries, err := u.List(ctx, usage.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries at level off, want 0", len(entries))
	}
}

func TestUsageRecordMetadataOmitsArgsAndResult(t *testing.T) {
	u := newTestUsage(t)
	ctx := context.Background()
	// Metadata is the store's own default; no UpdateSettings call needed.

	u.Record(ctx, Actor{Type: ActorToken, ID: "tok_1", IP: "203.0.113.5"}, testResolved(), "search",
		map[string]any{"q": "secret query"}, &engine.Result{StatusCode: 200, Text: "sensitive response"},
		nil, 7*time.Millisecond)

	entries, err := u.List(ctx, usage.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Args != "" || e.Result != "" {
		t.Errorf("metadata level recorded call content: args=%q result=%q", e.Args, e.Result)
	}
	if e.Tool != "search" || e.InstanceID != "inst_1" || e.ActorID != "tok_1" || e.IP != "203.0.113.5" {
		t.Errorf("metadata fields wrong: %+v", e)
	}
	if !e.Success || e.StatusCode != 200 || e.DurationMs != 7 {
		t.Errorf("outcome fields wrong: %+v", e)
	}
}

func TestUsageRecordFullIncludesArgsAndResult(t *testing.T) {
	u := newTestUsage(t)
	ctx := context.Background()
	if err := u.UpdateSettings(ctx, SystemActor(), usage.LevelFull, 0); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	u.Record(ctx, Actor{Type: ActorToken, ID: "tok_1"}, testResolved(), "search",
		map[string]any{"q": "hello"}, &engine.Result{StatusCode: 200, Text: "the response body"},
		nil, time.Millisecond)

	entries, err := u.List(ctx, usage.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Args == "" || e.Result != "the response body" {
		t.Errorf("full level dropped call content: args=%q result=%q", e.Args, e.Result)
	}
}

func TestUsageRecordCapturesFailures(t *testing.T) {
	u := newTestUsage(t)
	ctx := context.Background()

	u.Record(ctx, Actor{Type: ActorToken, ID: "tok_1"}, testResolved(), "search",
		map[string]any{"q": "hello"}, nil, &engine.Error{Kind: engine.KindUpstreamStatus, Message: "boom"},
		3*time.Millisecond)

	entries, err := u.List(ctx, usage.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Success {
		t.Error("a failed call was recorded as successful")
	}
	if e.Kind != engine.KindUpstreamStatus {
		t.Errorf("kind = %q, want %q", e.Kind, engine.KindUpstreamStatus)
	}
	if e.Error == "" {
		t.Error("a failed call recorded no error message")
	}
}

func TestUsageRecordTruncatesOversizedFields(t *testing.T) {
	u := newTestUsage(t)
	ctx := context.Background()
	if err := u.UpdateSettings(ctx, SystemActor(), usage.LevelFull, 0); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	huge := make([]byte, maxLoggedFieldBytes*2)
	for i := range huge {
		huge[i] = 'x'
	}
	u.Record(ctx, Actor{Type: ActorToken, ID: "tok_1"}, testResolved(), "read_file",
		nil, &engine.Result{StatusCode: 200, Text: string(huge)}, nil, time.Millisecond)

	entries, err := u.List(ctx, usage.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries[0].Result) > maxLoggedFieldBytes+64 {
		t.Errorf("result field not bounded: %d bytes", len(entries[0].Result))
	}
}

func TestUsageNilIsSafe(t *testing.T) {
	var u *Usage
	ctx := context.Background()
	// None of these must panic on a nil *Usage — mirrors how a nil *Indexer
	// leaves semantic search silently absent rather than crashing callers
	// that did not check first.
	u.Record(ctx, Actor{}, testResolved(), "x", nil, nil, nil, 0)
	if _, err := u.List(ctx, usage.Filter{}); err != nil {
		t.Errorf("List on nil Usage returned an error: %v", err)
	}
	if err := u.UpdateSettings(ctx, Actor{}, usage.LevelFull, 0); err != nil {
		t.Errorf("UpdateSettings on nil Usage returned an error: %v", err)
	}
	if err := u.Clear(ctx, Actor{}); err != nil {
		t.Errorf("Clear on nil Usage returned an error: %v", err)
	}
	if _, err := u.Prune(ctx); err != nil {
		t.Errorf("Prune on nil Usage returned an error: %v", err)
	}
	if u.Enabled() {
		t.Error("a nil Usage reported itself enabled")
	}
}
