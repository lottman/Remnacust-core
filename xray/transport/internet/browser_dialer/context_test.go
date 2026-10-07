package browser_dialer

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestCookieOnlyMetadataIsRetained(t *testing.T) {
	extra := contextHTTPExtra(nil, []*http.Cookie{{Name: "session", Value: "abc"}})
	if extra == nil || extra.Cookies["session"] != "abc" {
		t.Fatal("cookie metadata was dropped")
	}
}

func TestContextCancelsWaitingForBrowser(t *testing.T) {
	for _, packet := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		var err error
		if packet {
			err = DialPacketContext(ctx, "POST", "https://example.com/", nil, nil, []byte("test"))
		} else {
			_, err = DialGetContext(ctx, "https://example.com/", nil, nil)
		}
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiting for browser ignored deadline: %v", err)
		}
	}
}
