package config

import (
	"strings"
	"testing"
	"time"
)

// clearEnv unsets every MCPAW_* variable Load reads, so a test starts from a
// known-empty environment regardless of what the developer running it has
// exported. t.Setenv restores the previous value at test end.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"MCPAW_ADDR", "MCPAW_DATA_DIR", "MCPAW_PUBLIC_URL", "MCPAW_MASTER_KEY",
		"MCPAW_TRUST_PROXY_HEADERS", "MCPAW_SECURE_COOKIES", "MCPAW_LOG_LEVEL",
		"MCPAW_LOG_FORMAT", "MCPAW_MAX_REQUEST_BYTES", "MCPAW_SESSION_IDLE_TIMEOUT",
		"MCPAW_SESSION_ABSOLUTE_TIMEOUT", "MCPAW_LOGIN_RATE_LIMIT_PER_MIN",
		"MCPAW_METRICS_ENABLED",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load with an empty environment: %v", err)
	}

	if c.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", c.Addr)
	}
	if c.DataDir != "/data" {
		t.Errorf("DataDir = %q, want /data", c.DataDir)
	}
	if c.LogLevel != "info" || c.LogFormat != "json" {
		t.Errorf("log defaults = %q/%q, want info/json", c.LogLevel, c.LogFormat)
	}
	if !c.MetricsEnabled {
		t.Error("MetricsEnabled should default to true")
	}
	if c.TrustProxyHeaders {
		t.Error("TrustProxyHeaders must default to false: trusting forwarded headers " +
			"without a proxy in front lets a client spoof its own address")
	}
	if c.SecureCookies {
		t.Error("SecureCookies should default to false without an https PublicURL")
	}
	if c.MaxRequestBytes != 1<<20 {
		t.Errorf("MaxRequestBytes = %d, want %d", c.MaxRequestBytes, 1<<20)
	}
	if c.SessionIdleTimeout != 2*time.Hour || c.SessionAbsoluteTimeout != 24*time.Hour {
		t.Errorf("session timeouts = %v/%v, want 2h/24h", c.SessionIdleTimeout, c.SessionAbsoluteTimeout)
	}
	if c.LoginRateLimitPerMin != 10 {
		t.Errorf("LoginRateLimitPerMin = %d, want 10", c.LoginRateLimitPerMin)
	}
}

func TestLoadOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("MCPAW_ADDR", "127.0.0.1:9090")
	t.Setenv("MCPAW_DATA_DIR", "/var/lib/mcpaw")
	t.Setenv("MCPAW_LOG_LEVEL", "debug")
	t.Setenv("MCPAW_LOG_FORMAT", "text")
	t.Setenv("MCPAW_MAX_REQUEST_BYTES", "8192")
	t.Setenv("MCPAW_SESSION_IDLE_TIMEOUT", "15m")
	t.Setenv("MCPAW_SESSION_ABSOLUTE_TIMEOUT", "8h")
	t.Setenv("MCPAW_LOGIN_RATE_LIMIT_PER_MIN", "3")
	t.Setenv("MCPAW_TRUST_PROXY_HEADERS", "true")
	t.Setenv("MCPAW_METRICS_ENABLED", "false")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Addr != "127.0.0.1:9090" || c.DataDir != "/var/lib/mcpaw" {
		t.Errorf("Addr/DataDir = %q/%q", c.Addr, c.DataDir)
	}
	if c.LogLevel != "debug" || c.LogFormat != "text" {
		t.Errorf("log = %q/%q", c.LogLevel, c.LogFormat)
	}
	if c.MaxRequestBytes != 8192 {
		t.Errorf("MaxRequestBytes = %d", c.MaxRequestBytes)
	}
	if c.SessionIdleTimeout != 15*time.Minute || c.SessionAbsoluteTimeout != 8*time.Hour {
		t.Errorf("timeouts = %v/%v", c.SessionIdleTimeout, c.SessionAbsoluteTimeout)
	}
	if c.LoginRateLimitPerMin != 3 {
		t.Errorf("LoginRateLimitPerMin = %d", c.LoginRateLimitPerMin)
	}
	if !c.TrustProxyHeaders {
		t.Error("TrustProxyHeaders should be true")
	}
	if c.MetricsEnabled {
		t.Error("MetricsEnabled should be false")
	}
}

// A trailing slash on the public URL would otherwise produce "https://x//mcp/s"
// in the endpoint URLs the UI hands to operators.
func TestLoadTrimsPublicURLSlash(t *testing.T) {
	clearEnv(t)
	t.Setenv("MCPAW_PUBLIC_URL", "https://mcpaw.example.com/")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PublicURL != "https://mcpaw.example.com" {
		t.Errorf("PublicURL = %q, want the trailing slash trimmed", c.PublicURL)
	}
}

// Serving over https without the Secure attribute would let a downgrade attack
// read the session cookie, so it is inferred rather than left to the operator.
func TestSecureCookiesInferredFromHTTPSPublicURL(t *testing.T) {
	clearEnv(t)
	t.Setenv("MCPAW_PUBLIC_URL", "https://mcpaw.example.com")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.SecureCookies {
		t.Error("SecureCookies must be inferred true from an https PublicURL")
	}
}

func TestSecureCookiesExplicitOverridesInference(t *testing.T) {
	clearEnv(t)
	t.Setenv("MCPAW_PUBLIC_URL", "https://mcpaw.example.com")
	t.Setenv("MCPAW_SECURE_COOKIES", "false")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SecureCookies {
		t.Error("an explicit MCPAW_SECURE_COOKIES=false must win over the https inference")
	}
}

// Load never returns a partially valid Config: on any rejection the caller gets
// the zero value, so a mistake cannot half-apply.
func TestLoadRejectsInvalid(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"bad log level", map[string]string{"MCPAW_LOG_LEVEL": "verbose"}, "MCPAW_LOG_LEVEL"},
		{"bad log format", map[string]string{"MCPAW_LOG_FORMAT": "xml"}, "MCPAW_LOG_FORMAT"},
		{"relative public url", map[string]string{"MCPAW_PUBLIC_URL": "mcpaw.example.com"}, "MCPAW_PUBLIC_URL"},
		{"non-http public url", map[string]string{"MCPAW_PUBLIC_URL": "ftp://example.com"}, "MCPAW_PUBLIC_URL"},
		{"tiny request cap", map[string]string{"MCPAW_MAX_REQUEST_BYTES": "128"}, "MCPAW_MAX_REQUEST_BYTES"},
		{"non-numeric request cap", map[string]string{"MCPAW_MAX_REQUEST_BYTES": "lots"}, "MCPAW_MAX_REQUEST_BYTES"},
		{"absolute below idle", map[string]string{
			"MCPAW_SESSION_IDLE_TIMEOUT":     "10h",
			"MCPAW_SESSION_ABSOLUTE_TIMEOUT": "1h",
		}, "absolute timeout"},
		{"negative duration", map[string]string{"MCPAW_SESSION_IDLE_TIMEOUT": "-5m"}, "positive"},
		{"unparseable duration", map[string]string{"MCPAW_SESSION_IDLE_TIMEOUT": "fortnight"}, "duration"},
		{"zero login limit", map[string]string{"MCPAW_LOGIN_RATE_LIMIT_PER_MIN": "0"}, "LOGIN_RATE_LIMIT"},
		{"non-boolean proxy trust", map[string]string{"MCPAW_TRUST_PROXY_HEADERS": "yes-please"}, "boolean"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			c, err := Load()
			if err == nil {
				t.Fatalf("Load accepted an invalid configuration, got %+v", c)
			}
			if c != (Config{}) {
				t.Error("Load must return the zero Config on error, never a partially applied one")
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q, so it will not tell an operator which knob is wrong",
					err, tc.wantErr)
			}
		})
	}
}

// An empty or whitespace-only string is indistinguishable from unset, so a
// blank override falls back to the default rather than producing an empty Addr
// that passes the emptiness check and then fails at bind time.
func TestBlankEnvValueFallsBackToDefault(t *testing.T) {
	clearEnv(t)
	t.Setenv("MCPAW_ADDR", "   ")
	t.Setenv("MCPAW_LOG_LEVEL", "")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Addr != ":8080" || c.LogLevel != "info" {
		t.Errorf("blank overrides should fall back to defaults, got %q/%q", c.Addr, c.LogLevel)
	}
}

// MCPAW_MASTER_KEY="$(cat key)" picks up the file's trailing newline. Storing
// it verbatim makes the key fail to decode, with an error that points at the
// key material rather than at the newline.
func TestEnvValuesAreTrimmed(t *testing.T) {
	clearEnv(t)
	t.Setenv("MCPAW_MASTER_KEY", "  c29tZS1rZXktbWF0ZXJpYWw=\n")
	t.Setenv("MCPAW_LOG_LEVEL", " debug\n")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MasterKeyB64 != "c29tZS1rZXktbWF0ZXJpYWw=" {
		t.Errorf("MasterKeyB64 = %q, want the surrounding whitespace trimmed", c.MasterKeyB64)
	}
	if c.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", c.LogLevel, "debug")
	}
}
