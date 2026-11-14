package xerahttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
)

func TestNativePacketFetchHeadersMatchRequestOrigin(t *testing.T) {
	seen := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		seen <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	c := &DefaultDialerClient{transportConfig: &Config{}, client: server.Client()}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.PostPacket(ctx, server.URL+"/api/private", "session", "0", buf.MergeBytes(nil, []byte("data"))); err != nil {
		t.Fatal(err)
	}
	headers := <-seen
	if headers.Get("Origin") != server.URL || headers.Get("Referer") != server.URL+"/" {
		t.Fatalf("same-origin fetch missing its browser origin: %v", headers)
	}
	reader, _, _, err := c.OpenStream(ctx, server.URL+"/api/private", "session", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	headers = <-seen
	if headers.Get("Origin") != "" || headers.Get("Referer") != server.URL+"/" {
		t.Fatalf("GET fetch has incorrect origin metadata: %v", headers)
	}
}

func TestFetchOriginDoesNotExposePathCredentialsOrOverrideExplicitHeaders(t *testing.T) {
	for _, scenario := range []struct {
		url      string
		method   string
		explicit map[string]string
		site     string
		origin   string
		referrer string
	}{
		{url: "https://user:password@example.test:443/private?token=secret#fragment", method: "POST", origin: "https://example.test", referrer: "https://example.test/"},
		{url: "https://[2001:db8::1]:443/private?token=secret", method: "POST", origin: "https://[2001:db8::1]", referrer: "https://[2001:db8::1]/"},
		{url: "https://[2001:db8::1]:8443/private", method: "POST", origin: "https://[2001:db8::1]:8443", referrer: "https://[2001:db8::1]:8443/"},
		{url: "https://example.test/private", method: "GET", referrer: "https://example.test/"},
		{url: "https://example.test/private", method: "HEAD", referrer: "https://example.test/"},
		{url: "http://example.test:80/private", method: "POST", origin: "http://example.test", referrer: "http://example.test/"},
		{url: "https://example.test/private", method: "POST", explicit: map[string]string{"origin": "", "referer": ""}},
		{url: "https://example.test/private", method: "POST", explicit: map[string]string{"Origin": "https://own.test", "Referer": "https://own.test/page"}, origin: "https://own.test", referrer: "https://own.test/page"},
		{url: "https://example.test/private", method: "POST", site: "cross-site"},
	} {
		t.Run(scenario.method+scenario.url, func(t *testing.T) {
			request := httptest.NewRequest(scenario.method, scenario.url, nil)
			request.Header.Set("Sec-Fetch-Mode", "cors")
			request.Header.Set("Sec-Fetch-Site", "same-origin")
			if scenario.site != "" {
				request.Header.Set("Sec-Fetch-Site", scenario.site)
			}
			for name, value := range scenario.explicit {
				request.Header.Set(name, value)
			}
			config := &Config{Headers: scenario.explicit}
			config.applyFetchOriginHeaders(request)
			if request.Header.Get("Origin") != scenario.origin || request.Header.Get("Referer") != scenario.referrer {
				t.Fatal("origin or referrer differs", request.Header)
			}
		})
	}
}

func TestBrowserRequestConstructionLeavesOriginToBrowser(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://example.test/api/", nil)
	config := &Config{}
	if err := config.FillPacketRequest(request, "session", "0", buf.MergeBytes(nil, []byte("data"))); err != nil {
		t.Fatal(err)
	}
	if len(request.Header.Values("Origin")) != 0 || len(request.Header.Values("Referer")) != 0 {
		t.Fatal("browser request builder supplied browser-controlled headers")
	}
}
