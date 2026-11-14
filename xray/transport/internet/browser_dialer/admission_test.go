package browser_dialer

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/xtls/xray-core/common/platform"
)

func testBrowserHandler(t *testing.T) (http.Handler, string) {
	t.Helper()
	t.Setenv(platform.BrowserDialerAddress, "127.0.0.1:0")
	Reload()
	t.Cleanup(func() {
		t.Setenv(platform.BrowserDialerAddress, "")
		Reload()
	})
	mu.Lock()
	handler := server.Handler
	mu.Unlock()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("local capability page: %d", recorder.Code)
	}
	match := regexp.MustCompile(`/websocket\?token=([a-f0-9-]+)`).FindStringSubmatch(recorder.Body.String())
	if len(match) != 2 {
		t.Fatal("local capability page has no helper token")
	}
	return handler, match[1]
}

func TestBrowserHelperRejectsCrossOriginCapabilityReads(t *testing.T) {
	handler, _ := testBrowserHandler(t)
	for _, test := range []struct {
		name, host, origin string
	}{
		{"cross-origin", "127.0.0.1:0", "https://foreign.example"},
		{"other-port", "127.0.0.1:0", "http://127.0.0.1:1"},
		{"null-origin", "127.0.0.1:0", "null"},
		{"rebound-host", "foreign.example:0", "http://foreign.example:0"},
		{"rebound-navigation", "foreign.example:0", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://"+test.host+"/", nil)
			if test.origin != "" {
				req.Header.Set("Origin", test.origin)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusForbidden || strings.Contains(recorder.Body.String(), "/websocket?token=") {
				t.Fatalf("foreign page received helper capability: status=%d", recorder.Code)
			}
			if recorder.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("foreign page was granted CORS access")
			}
		})
	}
}

func TestBrowserHelperCapabilityIsNotCacheableOrEmbeddable(t *testing.T) {
	handler, _ := testBrowserHandler(t)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/", nil))
	for name, value := range map[string]string{"Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY"} {
		if recorder.Header().Get(name) != value {
			t.Errorf("%s = %q", name, recorder.Header().Get(name))
		}
	}
	if recorder.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("capability page was shared with all origins")
	}
}

func TestBrowserHelperWebsocketAdmission(t *testing.T) {
	handler, token := testBrowserHandler(t)
	fixture := httptest.NewServer(handler)
	defer fixture.Close()
	for _, test := range []struct {
		name, host, origin, query string
		want                      int
	}{
		{"same-origin", "127.0.0.1:0", "http://127.0.0.1:0", token, http.StatusSwitchingProtocols},
		{"native-helper", "127.0.0.1:0", "", token, http.StatusSwitchingProtocols},
		{"localhost", "localhost:0", "http://localhost:0", token, http.StatusSwitchingProtocols},
		{"foreign-origin", "127.0.0.1:0", "https://foreign.example", token, http.StatusForbidden},
		{"rebound-host", "foreign.example:0", "http://foreign.example:0", token, http.StatusForbidden},
		{"invalid-token", "127.0.0.1:0", "http://127.0.0.1:0", "wrong", http.StatusForbidden},
		{"duplicate-token", "127.0.0.1:0", "http://127.0.0.1:0", token + "&token=" + token, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			header := http.Header{"Host": {test.host}}
			if test.origin != "" {
				header.Set("Origin", test.origin)
			}
			dialer := websocket.Dialer{HandshakeTimeout: time.Second}
			conn, response, err := dialer.Dial("ws"+strings.TrimPrefix(fixture.URL, "http")+"/websocket?token="+test.query, header)
			if conn != nil {
				conn.Close()
			}
			if response == nil {
				t.Fatalf("no handshake response: %v", err)
			}
			defer response.Body.Close()
			io.Copy(io.Discard, response.Body)
			if response.StatusCode != test.want {
				t.Fatalf("handshake status = %d, want %d: %v", response.StatusCode, test.want, err)
			}
		})
	}
}
