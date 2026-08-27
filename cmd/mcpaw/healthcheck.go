package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// runHealthcheck queries /healthz on the locally running instance and exits
// non-zero on anything but 200.
//
// This is what the container's HEALTHCHECK directive shells out to. A plain
// `curl` would work too, but shipping a distroless final image means there is
// no shell and no curl binary available — the check has to be the binary
// itself.
func runHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	addr := fs.String("addr", envOr("MCPAW_ADDR", ":8080"), "address to check, e.g. :8080 or localhost:8080")
	timeout := fs.Duration("timeout", 3*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	url := "http://" + resolveHost(*addr) + "/healthz"
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: unhealthy (status %d)", url, resp.StatusCode)
	}
	return nil
}

// resolveHost turns a listen address ("", ":8080", "0.0.0.0:8080") into
// something a loopback HTTP client can actually dial.
func resolveHost(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	if strings.HasPrefix(addr, "0.0.0.0:") {
		return "localhost" + strings.TrimPrefix(addr, "0.0.0.0")
	}
	return addr
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
