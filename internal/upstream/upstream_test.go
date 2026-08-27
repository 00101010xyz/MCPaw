package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCheckIPDefaultPolicyBlocksPrivateDestinations(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"100.64.0.1", "fc00::1", "169.254.169.254", "0.0.0.0",
		"224.0.0.1", "255.255.255.255", "fe80::1",
		"::ffff:127.0.0.1", // IPv4-mapped loopback
	}
	for _, s := range blocked {
		ip := netip.MustParseAddr(s)
		if err := CheckIP(ip, EgressPolicy{}); err == nil {
			t.Errorf("CheckIP(%s) allowed under the default policy", s)
		}
	}

	allowed := []string{"1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"}
	for _, s := range allowed {
		if err := CheckIP(netip.MustParseAddr(s), EgressPolicy{}); err != nil {
			t.Errorf("CheckIP(%s) = %v, want allowed", s, err)
		}
	}
}

func TestPrivateOptInUnlocksLoopbackButNeverMetadata(t *testing.T) {
	policy := EgressPolicy{AllowPrivateNetworks: true}

	// The opt-in exists so that a local Zotero can be reached.
	for _, s := range []string{"127.0.0.1", "::1", "192.168.1.10", "10.0.0.5", "fc00::1"} {
		if err := CheckIP(netip.MustParseAddr(s), policy); err != nil {
			t.Errorf("CheckIP(%s) with the opt-in = %v, want allowed", s, err)
		}
	}

	// It must never unlock cloud instance metadata or multicast, whatever the
	// operator ticks in the UI.
	for _, s := range []string{"169.254.169.254", "169.254.1.1", "fe80::1", "224.0.0.1", "0.0.0.0"} {
		if err := CheckIP(netip.MustParseAddr(s), policy); err == nil {
			t.Errorf("CheckIP(%s) with the opt-in was allowed; metadata/multicast must stay blocked", s)
		}
	}
}

func TestCheckIPRejectsInvalidAddress(t *testing.T) {
	if err := CheckIP(netip.Addr{}, EgressPolicy{}); err == nil {
		t.Fatal("invalid address accepted")
	}
}

func TestCheckAddressRequiresResolvedIP(t *testing.T) {
	if err := CheckAddress("example.com:443", EgressPolicy{}); err == nil {
		t.Fatal("unresolved hostname accepted")
	}
	if err := CheckAddress("garbage", EgressPolicy{}); err == nil {
		t.Fatal("malformed address accepted")
	}
	if err := CheckAddress("1.1.1.1:443", EgressPolicy{}); err != nil {
		t.Fatalf("public address rejected: %v", err)
	}
}

// End-to-end proof that the dialer hook, not just the helper function, refuses
// a loopback destination.
func TestClientRefusesLoopbackWithoutOptIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Options{})
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	_, err := c.Do(req, EgressPolicy{}, 1<<20)
	if err == nil {
		t.Fatal("request to loopback succeeded under the default policy")
	}
	var blocked *BlockedIPError
	if !errors.As(err, &blocked) {
		t.Fatalf("got %v, want a BlockedIPError", err)
	}
}

func TestClientAllowsLoopbackWithOptIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New(Options{})
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req, EgressPolicy{AllowPrivateNetworks: true}, 1<<20)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != 200 || string(resp.Body) != `{"ok":true}` {
		t.Fatalf("unexpected response %d %q", resp.StatusCode, resp.Body)
	}
	if resp.Truncated {
		t.Fatal("small response reported as truncated")
	}
	if resp.Duration <= 0 {
		t.Fatal("duration was not measured")
	}
}

func TestResponseSizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", 10_000)))
	}))
	defer srv.Close()

	c := New(Options{})
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req, EgressPolicy{AllowPrivateNetworks: true}, 100)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !resp.Truncated {
		t.Fatal("oversized body was not flagged as truncated")
	}
	if len(resp.Body) != 100 {
		t.Fatalf("body length = %d, want the cap of 100", len(resp.Body))
	}
}

func TestDoRejectsInvalidArguments(t *testing.T) {
	c := New(Options{})
	if _, err := c.Do(nil, EgressPolicy{}, 10); err == nil {
		t.Fatal("nil request accepted")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if _, err := c.Do(req, EgressPolicy{}, 0); err == nil {
		t.Fatal("zero size cap accepted")
	}
}

// A hostile upstream must not be able to bounce a request — and the connector
// credential attached to it — to a different origin.
func TestCrossOriginRedirectsAreRefused(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("should not be reached"))
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusFound)
	}))
	defer redirector.Close()

	c := New(Options{})
	req, _ := http.NewRequest(http.MethodGet, redirector.URL, nil)
	_, err := c.Do(req, EgressPolicy{AllowPrivateNetworks: true}, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "cross-origin redirect") {
		t.Fatalf("got %v, want a cross-origin redirect refusal", err)
	}
}

func TestSameOriginRedirectsAreFollowedAndBounded(t *testing.T) {
	var mux http.ServeMux
	srv := httptest.NewServer(&mux)
	defer srv.Close()

	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/end", http.StatusFound)
	})
	mux.HandleFunc("/end", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("arrived"))
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	})

	c := New(Options{})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/start", nil)
	resp, err := c.Do(req, EgressPolicy{AllowPrivateNetworks: true}, 1<<20)
	if err != nil {
		t.Fatalf("same-origin redirect not followed: %v", err)
	}
	if string(resp.Body) != "arrived" {
		t.Fatalf("unexpected body %q", resp.Body)
	}

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/loop", nil)
	if _, err := c.Do(req, EgressPolicy{AllowPrivateNetworks: true}, 1<<20); err == nil {
		t.Fatal("infinite redirect loop was followed")
	}
}

func TestRequestTimeoutIsHonoured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)

	c := New(Options{})
	start := time.Now()
	if _, err := c.Do(req, EgressPolicy{AllowPrivateNetworks: true}, 1<<20); err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout took %v, context deadline was not honoured", elapsed)
	}
}

func TestBreakerOpensAfterThresholdAndRecovers(t *testing.T) {
	clock := time.Now()
	b := NewBreaker(BreakerConfig{FailureThreshold: 3, OpenDuration: time.Minute, HalfOpenSuccesses: 2})
	b.now = func() time.Time { return clock }

	for i := 0; i < 2; i++ {
		if err := b.Allow("k"); err != nil {
			t.Fatalf("breaker opened early after %d failures", i)
		}
		b.Record("k", false)
	}
	if err := b.Allow("k"); err != nil {
		t.Fatal("breaker opened before reaching the threshold")
	}
	b.Record("k", false) // third failure

	if err := b.Allow("k"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("got %v, want ErrCircuitOpen", err)
	}
	if b.State("k") != StateOpen {
		t.Fatalf("state = %s, want open", b.State("k"))
	}

	// After the cooldown a single probe is allowed through.
	clock = clock.Add(2 * time.Minute)
	if err := b.Allow("k"); err != nil {
		t.Fatalf("probe refused after cooldown: %v", err)
	}
	b.Record("k", true)
	if b.State("k") != StateHalfOpen {
		t.Fatalf("state = %s, want half-open after one success", b.State("k"))
	}
	b.Record("k", true)
	if b.State("k") != StateClosed {
		t.Fatalf("state = %s, want closed after the required successes", b.State("k"))
	}
}

func TestBreakerReopensImmediatelyOnFailedProbe(t *testing.T) {
	clock := time.Now()
	b := NewBreaker(BreakerConfig{FailureThreshold: 1, OpenDuration: time.Minute})
	b.now = func() time.Time { return clock }

	b.Record("k", false)
	clock = clock.Add(2 * time.Minute)
	if err := b.Allow("k"); err != nil {
		t.Fatalf("probe refused: %v", err)
	}
	b.Record("k", false)
	clock = clock.Add(time.Second)
	if err := b.Allow("k"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("got %v, want the circuit to reopen immediately", err)
	}
}

func TestBreakerIsPerKeyAndResettable(t *testing.T) {
	b := NewBreaker(BreakerConfig{FailureThreshold: 1})
	b.Record("a", false)
	if err := b.Allow("a"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal("key a should be open")
	}
	if err := b.Allow("b"); err != nil {
		t.Fatalf("key b was affected by key a: %v", err)
	}
	b.Reset("a")
	if err := b.Allow("a"); err != nil {
		t.Fatalf("Reset did not clear the breaker: %v", err)
	}
}

func TestMemoryLimiterEnforcesBudget(t *testing.T) {
	l := NewMemoryLimiter()
	clock := time.Now()
	l.now = func() time.Time { return clock }

	for i := 0; i < 3; i++ {
		if !l.Allow("k", 3) {
			t.Fatalf("request %d was limited within budget", i)
		}
	}
	if l.Allow("k", 3) {
		t.Fatal("budget was exceeded without being limited")
	}

	// Tokens refill over time: a third of a minute restores one token.
	clock = clock.Add(21 * time.Second)
	if !l.Allow("k", 3) {
		t.Fatal("bucket did not refill")
	}
}

func TestMemoryLimiterUnlimitedAndPerKey(t *testing.T) {
	l := NewMemoryLimiter()
	for i := 0; i < 100; i++ {
		if !l.Allow("k", 0) {
			t.Fatal("a non-positive budget must mean unlimited")
		}
	}
	l2 := NewMemoryLimiter()
	if !l2.Allow("a", 1) || !l2.Allow("b", 1) {
		t.Fatal("buckets are not per key")
	}
	if l2.Allow("a", 1) {
		t.Fatal("key a should be exhausted")
	}
}

// Lowering the budget must not hand back tokens the caller had already spent.
func TestMemoryLimiterRescalesWithoutRefunding(t *testing.T) {
	l := NewMemoryLimiter()
	clock := time.Now()
	l.now = func() time.Time { return clock }

	for i := 0; i < 10; i++ {
		l.Allow("k", 10)
	}
	if l.Allow("k", 10) {
		t.Fatal("bucket should be empty")
	}
	if l.Allow("k", 5) {
		t.Fatal("shrinking the budget refunded tokens")
	}
}

func TestGateBoundsConcurrency(t *testing.T) {
	g := NewGate()
	ctx := context.Background()

	r1, err := g.Acquire(ctx, "k", 1)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// The second acquisition must block until the first is released.
	blocked := make(chan struct{})
	go func() {
		r2, err := g.Acquire(ctx, "k", 1)
		if err == nil {
			r2()
		}
		close(blocked)
	}()

	select {
	case <-blocked:
		t.Fatal("gate allowed two concurrent holders of a single slot")
	case <-time.After(50 * time.Millisecond):
	}
	r1()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("release did not free the slot")
	}
}

func TestGateRespectsContextCancellation(t *testing.T) {
	g := NewGate()
	release, _ := g.Acquire(context.Background(), "k", 1)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := g.Acquire(ctx, "k", 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want DeadlineExceeded", err)
	}
}

func TestGateUnlimitedAndDoubleRelease(t *testing.T) {
	g := NewGate()
	release, err := g.Acquire(context.Background(), "k", 0)
	if err != nil {
		t.Fatalf("Acquire with no limit: %v", err)
	}
	release()

	release, _ = g.Acquire(context.Background(), "k2", 1)
	release()
	// Releasing twice must not free a slot that was never held.
	release()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := g.Acquire(context.Background(), "k2", 1)
			if err == nil {
				r()
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gate deadlocked after a double release")
	}
	g.Forget("k2")
}
