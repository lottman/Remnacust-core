package xerahttp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
)

func TestBrowserRejectsUnsupportedMetadataBeforeWaiting(t *testing.T) {
	for _, test := range []struct {
		name   string
		config *Config
	}{
		{"session-cookie", &Config{SessionIDPlacement: PlacementCookie}},
		{"sequence-cookie", &Config{SeqPlacement: PlacementCookie}},
		{"padding-cookie", &Config{XPaddingPlacement: PlacementCookie}},
		{"payload-cookie", &Config{UplinkDataPlacement: PlacementCookie}},
		{"explicit-cookie", &Config{Headers: map[string]string{"cookie": "private=value"}}},
		{"get-body", &Config{UplinkHTTPMethod: "GET", UplinkDataPlacement: PlacementBody}},
		{"head-body", &Config{UplinkHTTPMethod: "HEAD", UplinkDataPlacement: PlacementAuto}},
		{"forbidden-method", &Config{UplinkHTTPMethod: "TRACE"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			c := &BrowserDialerClient{transportConfig: test.config}
			r, _, _, err := c.OpenStream(ctx, "https://example.test/", "session", nil, false)
			if r != nil {
				r.Close()
			}
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("unsupported browser config reached helper wait: %v", err)
			}
			packet := buf.New()
			packet.Write([]byte("payload"))
			ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel2()
			err = c.PostPacket(ctx2, "https://example.test/", "session", "0", buf.MultiBuffer{packet})
			if err == nil || errors.Is(err, context.DeadlineExceeded) || !packet.IsEmpty() {
				t.Fatalf("unsupported packet config waited or retained payload: %v", err)
			}
		})
	}
}
