package olcrtc

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestReadyAfterShutdown(t *testing.T) {
	failure := errors.New("tunnel stopped")
	h := &Handler{ready: make(chan struct{}), done: make(chan struct{}), runErr: failure}
	close(h.ready)
	close(h.done)
	for range 1000 {
		if err := h.waitReady(context.Background()); !errors.Is(err, failure) {
			t.Fatalf("closed tunnel reported ready: %v", err)
		}
	}
}

func TestReadyAfterCancellation(t *testing.T) {
	h := &Handler{ready: make(chan struct{}), done: make(chan struct{})}
	close(h.ready)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 1000 {
		if err := h.waitReady(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled request reported ready: %v", err)
		}
	}
}

func TestKeyValidationMatchesRuntime(t *testing.T) {
	c := &Config{Provider: "telemost", Transport: "vp8channel", RoomId: "room", CryptoKey: strings.Repeat("ab", 32), DnsServer: "1.1.1.1:53"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{" " + c.CryptoKey, c.CryptoKey + "\n", "zz", ""} {
		c.CryptoKey = key
		if c.Validate() == nil {
			t.Fatal("invalid runtime key accepted")
		}
	}
}
