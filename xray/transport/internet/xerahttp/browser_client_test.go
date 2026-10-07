package xerahttp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
)

func TestBrowserPacketReleasesPayloadOnInvalidURL(t *testing.T) {
	b := buf.New()
	b.Write([]byte("test"))
	c := &BrowserDialerClient{transportConfig: &Config{}}
	if err := c.PostPacket(context.Background(), "://bad", "", "0", buf.MultiBuffer{b}); err == nil {
		t.Fatal("invalid URL accepted")
	}
	if !b.IsEmpty() {
		b.Release()
		t.Fatal("payload survived failed request construction")
	}
}

func TestBrowserPacketCancellationReleasesPayload(t *testing.T) {
	b := buf.New()
	b.Write([]byte("test"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &BrowserDialerClient{transportConfig: &Config{}}
	if err := c.PostPacket(ctx, "https://example.com/", "", "0", buf.MultiBuffer{b}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if !b.IsEmpty() {
		b.Release()
		t.Fatal("payload survived cancellation")
	}
}

func TestBrowserStreamWaitHonorsDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	c := &BrowserDialerClient{transportConfig: &Config{}}
	if _, _, _, err := c.OpenStream(ctx, "https://example.com/", "", nil, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("browser wait lost deadline: %v", err)
	}
}
