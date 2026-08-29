package webui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/00101010xyz/mcpaw/internal/engine"
	"github.com/00101010xyz/mcpaw/internal/service"
	"github.com/00101010xyz/mcpaw/internal/usage"
)

// PostSettingsUsage saves the log level and size cap, and it must not be
// possible to poison the stored level with something the store itself would
// reject — the handler passes whatever the form sends straight through, so
// this also exercises UpdateSettings' own validation.
func TestPostSettingsUsageSavesLevelAndCap(t *testing.T) {
	h := newHarness(t)
	admin := h.createAdmin(t, "admin@example.com")

	form := url.Values{"level": {"full"}, "max_mb": {"256"}}
	req := authedRequest(http.MethodPost, "/settings/usage", admin)
	req.Body = io.NopCloser(strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	h.server.PostSettingsUsage(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect", rec.Code)
	}

	settings, err := h.usage.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if settings.Level != usage.LevelFull {
		t.Errorf("level = %q, want full", settings.Level)
	}
	if settings.MaxBytes != 256*(1<<20) {
		t.Errorf("max bytes = %d, want %d", settings.MaxBytes, 256*(1<<20))
	}
}

// GetUsage must render recorded entries and respect the instance/tool
// filters — the whole reason the page exists is to let an operator narrow
// down "what has this instance's token been calling".
func TestGetUsageListsAndFiltersEntries(t *testing.T) {
	h := newHarness(t)
	admin := h.createAdmin(t, "admin@example.com")
	inst := h.createInstance(t, "My Zotero", "usage-instance")

	resolved := &service.Resolved{Instance: inst}
	ctx := context.Background()
	h.usage.Record(ctx, service.Actor{Type: service.ActorToken, ID: "tok_1"}, resolved, "search",
		nil, &engine.Result{StatusCode: 200}, nil, 5*time.Millisecond)
	h.usage.Record(ctx, service.Actor{Type: service.ActorToken, ID: "tok_1"}, resolved, "get_item",
		nil, &engine.Result{StatusCode: 200}, nil, 5*time.Millisecond)

	rec := httptest.NewRecorder()
	h.server.GetUsage(rec, authedRequest(http.MethodGet, "/usage", admin))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "search") || !strings.Contains(rec.Body.String(), "get_item") {
		t.Error("page does not list both recorded calls")
	}

	rec = httptest.NewRecorder()
	h.server.GetUsage(rec, authedRequest(http.MethodGet, "/usage?tool=search", admin))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "search") {
		t.Error("filtered page is missing the matching call")
	}
	if strings.Contains(body, "get_item") {
		t.Error("filtered page still shows a call for a different tool")
	}
}

// PostUsageClear must remove every stored entry, for an operator who wants a
// clean slate without waiting for the size cap to catch up.
func TestPostUsageClearRemovesEntries(t *testing.T) {
	h := newHarness(t)
	admin := h.createAdmin(t, "admin@example.com")
	inst := h.createInstance(t, "My Zotero", "usage-clear-instance")

	ctx := context.Background()
	h.usage.Record(ctx, service.Actor{Type: service.ActorToken, ID: "tok_1"}, &service.Resolved{Instance: inst},
		"search", nil, &engine.Result{StatusCode: 200}, nil, time.Millisecond)

	rec := httptest.NewRecorder()
	h.server.PostUsageClear(rec, authedRequest(http.MethodPost, "/usage/clear", admin))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect", rec.Code)
	}

	entries, err := h.usage.List(ctx, usage.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries after Clear, want 0", len(entries))
	}
}
