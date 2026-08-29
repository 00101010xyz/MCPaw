package usage

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestDefaultSettings(t *testing.T) {
	s := newTestStore(t)
	settings, err := s.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if settings.Level != LevelMetadata {
		t.Errorf("default level = %q, want %q", settings.Level, LevelMetadata)
	}
	if settings.MaxBytes != DefaultMaxBytes {
		t.Errorf("default max bytes = %d, want %d", settings.MaxBytes, DefaultMaxBytes)
	}
}

func TestUpdateSettingsRejectsAnUnknownLevel(t *testing.T) {
	s := newTestStore(t)
	err := s.UpdateSettings(context.Background(), Settings{Level: "verbose", MaxBytes: 1024})
	if err == nil {
		t.Fatal("an unknown level was accepted")
	}
}

func TestUpdateSettingsPersists(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpdateSettings(ctx, Settings{Level: LevelFull, MaxBytes: 2048}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	got, err := s.Settings(ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if got.Level != LevelFull || got.MaxBytes != 2048 {
		t.Errorf("got %+v, want {full 2048}", got)
	}
}

func TestUpdateSettingsFallsBackToDefaultMaxBytes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpdateSettings(ctx, Settings{Level: LevelOff, MaxBytes: -5}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	got, err := s.Settings(ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if got.MaxBytes != DefaultMaxBytes {
		t.Errorf("max bytes = %d, want the default %d for a non-positive input", got.MaxBytes, DefaultMaxBytes)
	}
}

func entry(id, instanceID, tool string, at time.Time) *Entry {
	return &Entry{
		ID: id, At: at, InstanceID: instanceID, InstanceName: "Test instance",
		Tool: tool, ActorType: "token", ActorID: "tok_1", IP: "203.0.113.1",
		Success: true, StatusCode: 200, DurationMs: 12,
	}
}

func TestAppendAndList(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)

	if err := s.Append(ctx, entry("use_1", "inst_a", "search", base)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Append(ctx, entry("use_2", "inst_b", "get_item", base.Add(time.Minute))); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Append(ctx, entry("use_3", "inst_a", "get_item", base.Add(2*time.Minute))); err != nil {
		t.Fatalf("Append: %v", err)
	}

	all, err := s.List(ctx, Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d entries, want 3", len(all))
	}
	// Most recent first.
	if all[0].ID != "use_3" || all[2].ID != "use_1" {
		t.Errorf("entries not ordered most-recent-first: %v", []string{all[0].ID, all[1].ID, all[2].ID})
	}

	byInstance, err := s.List(ctx, Filter{InstanceID: "inst_a"})
	if err != nil {
		t.Fatalf("List by instance: %v", err)
	}
	if len(byInstance) != 2 {
		t.Fatalf("got %d entries for inst_a, want 2", len(byInstance))
	}

	byTool, err := s.List(ctx, Filter{Tool: "search"})
	if err != nil {
		t.Fatalf("List by tool: %v", err)
	}
	if len(byTool) != 1 || byTool[0].ID != "use_1" {
		t.Fatalf("unexpected tool filter result: %+v", byTool)
	}
}

func TestClearDeletesEverything(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Append(ctx, entry("use_1", "inst_a", "search", time.Now())); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Clear(ctx); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	remaining, err := s.List(ctx, Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("got %d entries after Clear, want 0", len(remaining))
	}
}

// TestPruneDeletesOldestFirst writes enough entries — each carrying a
// sizeable payload so the file actually grows — that the store exceeds a
// tiny configured cap, then checks that Prune brings it back under budget by
// deleting the oldest rows first, leaving the newest ones behind.
func TestPruneDeletesOldestFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const maxBytes = 40 * 1024 // small enough that a few KB of rows overflow it
	if err := s.UpdateSettings(ctx, Settings{Level: LevelFull, MaxBytes: maxBytes}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	payload := strings.Repeat("x", 2048)
	base := time.Now().UTC().Add(-time.Hour)
	const n = 60
	for i := 0; i < n; i++ {
		e := entry(idFor(i), "inst_a", "read_file", base.Add(time.Duration(i)*time.Second))
		e.Result = payload
		if err := s.Append(ctx, e); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	sizeBefore, err := s.fileSize()
	if err != nil {
		t.Fatalf("fileSize: %v", err)
	}
	if sizeBefore <= maxBytes {
		t.Fatalf("test setup did not exceed the cap: %d bytes, cap %d", sizeBefore, maxBytes)
	}

	deleted, err := s.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if deleted == 0 {
		t.Fatal("Prune reported deleting nothing despite being over budget")
	}

	sizeAfter, err := s.fileSize()
	if err != nil {
		t.Fatalf("fileSize after prune: %v", err)
	}
	if sizeAfter > maxBytes {
		t.Fatalf("file size after Prune = %d, want at most the %d cap", sizeAfter, maxBytes)
	}

	remaining, err := s.List(ctx, Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(remaining) == 0 {
		t.Fatal("Prune deleted every entry rather than just the oldest")
	}
	// The newest entry must have survived, and it must be at least as new as
	// every entry that did survive — i.e. only a prefix of the oldest rows
	// was removed.
	newest := idFor(n - 1)
	found := false
	for _, e := range remaining {
		if e.ID == newest {
			found = true
		}
	}
	if !found {
		t.Fatal("Prune deleted the newest entry")
	}
	oldestSurvivor := remaining[len(remaining)-1]
	if oldestSurvivor.ID == idFor(0) {
		t.Fatal("Prune left the very oldest entry in place; expected it to be deleted first")
	}
}

func idFor(i int) string { return "use_" + string(rune('a'+i%26)) + string(rune('0'+i/26)) }

func TestPruneIsANoOpUnderTheCap(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Append(ctx, entry("use_1", "inst_a", "search", time.Now())); err != nil {
		t.Fatalf("Append: %v", err)
	}
	deleted, err := s.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("Prune deleted %d entries while comfortably under its 1 GiB default cap", deleted)
	}
	remaining, err := s.List(ctx, Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("got %d entries, want the one seeded entry untouched", len(remaining))
	}
}
