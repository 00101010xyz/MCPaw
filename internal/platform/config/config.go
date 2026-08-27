// Package config loads and validates process configuration from the
// environment.
//
// Configuration is read exactly once at startup into an immutable value. No
// other package reads os.Getenv, so the full set of knobs is discoverable here
// and every component can be constructed in a test without touching the
// environment.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the validated runtime configuration of the process.
type Config struct {
	// Addr is the listen address for the HTTP server.
	Addr string
	// DataDir holds the SQLite database and the generated master key file.
	DataDir string
	// PublicURL is the externally reachable base URL, used to render MCP
	// endpoint URLs in the UI. Optional.
	PublicURL string

	// MasterKeyB64 is the base64 (std or raw-url) encoded 32-byte key used to
	// derive encryption subkeys. When empty, a key file is generated in DataDir.
	MasterKeyB64 string

	// TrustProxyHeaders makes the server honour X-Forwarded-For/Proto. Only
	// enable when a trusted reverse proxy is actually in front of the process,
	// otherwise clients can spoof their own IP in the audit log.
	TrustProxyHeaders bool
	// SecureCookies forces the Secure attribute on cookies. Auto-enabled when
	// PublicURL is https.
	SecureCookies bool

	// LogLevel is one of debug, info, warn, error.
	LogLevel string
	// LogFormat is json or text.
	LogFormat string

	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration

	// MaxRequestBytes caps the size of admin API and MCP request bodies.
	MaxRequestBytes int64

	// SessionIdleTimeout and SessionAbsoluteTimeout bound web sessions.
	SessionIdleTimeout     time.Duration
	SessionAbsoluteTimeout time.Duration

	// LoginRateLimitPerMin throttles password attempts per client IP.
	LoginRateLimitPerMin int

	// MetricsEnabled exposes /metrics in Prometheus text format.
	MetricsEnabled bool
}

// Load reads configuration from the environment, applies defaults and
// validates the result. It never returns a partially valid Config.
func Load() (Config, error) {
	c := Config{
		Addr:                   env("MCPAW_ADDR", ":8080"),
		DataDir:                env("MCPAW_DATA_DIR", "/data"),
		PublicURL:              strings.TrimRight(env("MCPAW_PUBLIC_URL", ""), "/"),
		MasterKeyB64:           env("MCPAW_MASTER_KEY", ""),
		LogLevel:               strings.ToLower(env("MCPAW_LOG_LEVEL", "info")),
		LogFormat:              strings.ToLower(env("MCPAW_LOG_FORMAT", "json")),
		ReadHeaderTimeout:      5 * time.Second,
		ReadTimeout:            30 * time.Second,
		WriteTimeout:           120 * time.Second,
		IdleTimeout:            90 * time.Second,
		ShutdownTimeout:        20 * time.Second,
		MaxRequestBytes:        1 << 20,
		SessionIdleTimeout:     2 * time.Hour,
		SessionAbsoluteTimeout: 24 * time.Hour,
		LoginRateLimitPerMin:   10,
		MetricsEnabled:         true,
	}

	var err error
	if c.TrustProxyHeaders, err = envBool("MCPAW_TRUST_PROXY_HEADERS", false); err != nil {
		return Config{}, err
	}
	if c.MetricsEnabled, err = envBool("MCPAW_METRICS_ENABLED", true); err != nil {
		return Config{}, err
	}
	if c.MaxRequestBytes, err = envInt64("MCPAW_MAX_REQUEST_BYTES", c.MaxRequestBytes); err != nil {
		return Config{}, err
	}
	if c.LoginRateLimitPerMin, err = envInt("MCPAW_LOGIN_RATE_LIMIT_PER_MIN", c.LoginRateLimitPerMin); err != nil {
		return Config{}, err
	}
	if c.SessionIdleTimeout, err = envDuration("MCPAW_SESSION_IDLE_TIMEOUT", c.SessionIdleTimeout); err != nil {
		return Config{}, err
	}
	if c.SessionAbsoluteTimeout, err = envDuration("MCPAW_SESSION_ABSOLUTE_TIMEOUT", c.SessionAbsoluteTimeout); err != nil {
		return Config{}, err
	}

	secure, err := envBool("MCPAW_SECURE_COOKIES", strings.HasPrefix(c.PublicURL, "https://"))
	if err != nil {
		return Config{}, err
	}
	c.SecureCookies = secure

	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) validate() error {
	if c.Addr == "" {
		return fmt.Errorf("config: MCPAW_ADDR must not be empty")
	}
	if c.DataDir == "" {
		return fmt.Errorf("config: MCPAW_DATA_DIR must not be empty")
	}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("config: MCPAW_PUBLIC_URL must be an absolute http(s) URL")
		}
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: MCPAW_LOG_LEVEL must be debug|info|warn|error, got %q", c.LogLevel)
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		return fmt.Errorf("config: MCPAW_LOG_FORMAT must be json|text, got %q", c.LogFormat)
	}
	if c.MaxRequestBytes < 4096 {
		return fmt.Errorf("config: MCPAW_MAX_REQUEST_BYTES must be at least 4096")
	}
	if c.SessionAbsoluteTimeout < c.SessionIdleTimeout {
		return fmt.Errorf("config: session absolute timeout must be >= idle timeout")
	}
	if c.LoginRateLimitPerMin < 1 {
		return fmt.Errorf("config: MCPAW_LOGIN_RATE_LIMIT_PER_MIN must be >= 1")
	}
	return nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("config: %s must be a boolean: %w", key, err)
	}
	return b, nil
}

func envInt(key string, def int) (int, error) {
	v, err := envInt64(key, int64(def))
	return int(v), err
}

func envInt64(key string, def int64) (int64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer: %w", key, err)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a duration (e.g. 30m): %w", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("config: %s must be positive", key)
	}
	return d, nil
}
