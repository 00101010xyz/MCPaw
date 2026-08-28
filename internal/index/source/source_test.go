package source

import (
	"context"
	"testing"
)

type stubCrawler struct{}

func (stubCrawler) RequiredTools() []string                        { return nil }
func (stubCrawler) Crawl(context.Context, Runtime, EmitFunc) error { return nil }

func TestRegisterAndGet(t *testing.T) {
	Register("test-source-register-and-get", stubCrawler{})

	c, ok := Get("test-source-register-and-get")
	if !ok {
		t.Fatal("Get did not find the registered crawler")
	}
	if _, ok := c.(stubCrawler); !ok {
		t.Errorf("Get returned %T, want stubCrawler", c)
	}
}

func TestGetUnknownConnector(t *testing.T) {
	if _, ok := Get("no-such-connector-id"); ok {
		t.Error("Get should report false for an unregistered connector ID")
	}
}

func TestRegisterPanicsOnDuplicate(t *testing.T) {
	Register("test-source-duplicate", stubCrawler{})
	defer func() {
		if recover() == nil {
			t.Error("registering the same connector ID twice must panic — a silent second Register would shadow the first crawler with no signal")
		}
	}()
	Register("test-source-duplicate", stubCrawler{})
}

func TestRegisterPanicsOnEmptyID(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Register with an empty connector ID must panic")
		}
	}()
	Register("", stubCrawler{})
}

func TestRegisterPanicsOnNilCrawler(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Register with a nil Crawler must panic")
		}
	}()
	Register("test-source-nil-crawler", nil)
}
