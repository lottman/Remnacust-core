package browser_dialer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestBrowserAdmissionCompatibilityMatrix(t *testing.T) {
	for _, test := range []struct {
		name, bind, host, origin, local string
		allowed                         bool
	}{
		{"ipv4", "127.0.0.1:9000", "127.0.0.1:9000", "http://127.0.0.1:9000", "", true},
		{"ipv4-localhost", "127.0.0.1:9000", "localhost:9000", "http://localhost:9000", "", true},
		{"ipv6", "[::1]:9000", "[::1]:9000", "http://[::1]:9000", "", true},
		{"ipv6-expanded", "[::1]:9000", "[0:0:0:0:0:0:0:1]:9000", "", "", true},
		{"ipv6-origin-normalized", "[::1]:9000", "[0:0:0:0:0:0:0:1]:9000", "http://[::1]:9000", "", true},
		{"localhost-ipv6", "localhost:9000", "[::1]:9000", "http://[::1]:9000", "", true},
		{"explicit-ip", "192.0.2.20:9000", "192.0.2.20:9000", "http://192.0.2.20:9000", "", true},
		{"explicit-ip-other", "192.0.2.20:9000", "192.0.2.21:9000", "", "", false},
		{"explicit-name", "dialer.internal:9000", "DIALER.INTERNAL:9000", "http://DIALER.INTERNAL:9000", "", true},
		{"wildcard-v4", "0.0.0.0:9000", "127.0.0.1:9000", "http://127.0.0.1:9000", "127.0.0.1", true},
		{"wildcard-v4-localhost", "0.0.0.0:9000", "localhost:9000", "http://localhost:9000", "127.0.0.1", true},
		{"wildcard-v6", "[::]:9000", "[::1]:9000", "http://[::1]:9000", "::1", true},
		{"wildcard-v6-localhost", "[::]:9000", "localhost:9000", "http://localhost:9000", "::1", true},
		{"wildcard-nonlocal-localhost", "0.0.0.0:9000", "localhost:9000", "http://localhost:9000", "192.0.2.20", false},
		{"empty-bind", ":9000", "127.0.0.1:9000", "", "127.0.0.1", true},
		{"wildcard-missing-local", ":9000", "127.0.0.1:9000", "", "", false},
		{"wildcard-other-local", ":9000", "127.0.0.2:9000", "", "127.0.0.1", false},
		{"foreign-host", "127.0.0.1:9000", "foreign.invalid:9000", "http://foreign.invalid:9000", "", false},
		{"wrong-port", "127.0.0.1:9000", "127.0.0.1:9001", "", "", false},
		{"wrong-origin-port", "127.0.0.1:9000", "127.0.0.1:9000", "http://127.0.0.1:9001", "", false},
		{"wrong-origin-host", "127.0.0.1:9000", "127.0.0.1:9000", "http://localhost:9000", "", false},
		{"wrong-origin-scheme", "127.0.0.1:9000", "127.0.0.1:9000", "https://127.0.0.1:9000", "", false},
		{"null-origin", "127.0.0.1:9000", "127.0.0.1:9000", "null", "", false},
		{"origin-userinfo", "127.0.0.1:9000", "127.0.0.1:9000", "http://user@127.0.0.1:9000", "", false},
		{"origin-path", "127.0.0.1:9000", "127.0.0.1:9000", "http://127.0.0.1:9000/", "", false},
		{"origin-query", "127.0.0.1:9000", "127.0.0.1:9000", "http://127.0.0.1:9000?x=1", "", false},
		{"origin-empty-query", "127.0.0.1:9000", "127.0.0.1:9000", "http://127.0.0.1:9000?", "", false},
		{"origin-empty-fragment", "127.0.0.1:9000", "127.0.0.1:9000", "http://127.0.0.1:9000#", "", false},
		{"default-port-implicit", "127.0.0.1:80", "127.0.0.1", "http://127.0.0.1", "", true},
		{"default-port-origin-normalized", "127.0.0.1:80", "127.0.0.1:80", "http://127.0.0.1", "", true},
		{"default-port-host-normalized", "127.0.0.1:80", "127.0.0.1", "http://127.0.0.1:80", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://"+test.host+"/", nil)
			if test.origin != "" {
				r.Header.Set("Origin", test.origin)
			}
			if test.local != "" {
				r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP(test.local), Port: 9000}))
			}
			if got := browserRequestAllowed(r, test.bind); got != test.allowed {
				t.Fatalf("allowed = %v, want %v", got, test.allowed)
			}
		})
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9000/", nil)
	request.Header["Origin"] = []string{"http://127.0.0.1:9000", "http://127.0.0.1:9000"}
	if browserRequestAllowed(request, "127.0.0.1:9000") {
		t.Fatal("duplicate Origin accepted")
	}
}

type browserCompatibilityFixture struct {
	origin    string
	available chan *websocket.Conn
}

func browserCompatibilityIPv6Unavailable(t *testing.T, err error) {
	t.Helper()
	if os.Getenv("REMNACUST_REQUIRE_IPV6_TESTS") == "1" {
		t.Fatalf("required IPv6 loopback sockets unavailable before HTTP admission: %v", err)
	}
	t.Skipf("IPv6 loopback sockets unavailable before HTTP admission: %v", err)
}

func browserCompatibilityChromeDependencies(t *testing.T) {
	t.Helper()
	for _, name := range []string{"XRAY_BROWSER_NODE", "XRAY_BROWSER_CHROME", "XRAY_BROWSER_PLAYWRIGHT_PATH"} {
		if os.Getenv(name) == "" {
			if os.Getenv("REMNACUST_REQUIRE_BROWSER_TESTS") == "1" {
				t.Fatalf("required real-Chrome integration dependency %s is missing", name)
			}
			t.Skipf("set %s to enable isolated real-Chrome integration", name)
		}
	}
}

func newBrowserCompatibilityFixture(t *testing.T, ipv6, wildcard bool) browserCompatibilityFixture {
	t.Helper()
	fixture := httptest.NewUnstartedServer(nil)
	if ipv6 {
		fixture.Listener.Close()
		listener, err := net.Listen("tcp6", "[::1]:0")
		if err != nil {
			browserCompatibilityIPv6Unavailable(t, err)
		}
		fixture.Listener = listener
		probe, err := net.DialTimeout("tcp6", listener.Addr().String(), time.Second)
		if err != nil {
			listener.Close()
			browserCompatibilityIPv6Unavailable(t, err)
		}
		probe.Close()
	}
	address := fixture.Listener.Addr().String()
	if wildcard {
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			t.Fatal(err)
		}
		host := "0.0.0.0"
		if ipv6 {
			host = "::"
		}
		address = net.JoinHostPort(host, port)
	}
	available := make(chan *websocket.Conn, 32)
	page := bytes.ReplaceAll(webpage, []byte("csrfToken"), []byte("integration-capability"))
	fixture.Config.Handler = newBrowserHandler(address, "integration-capability", page, available)
	fixture.Start()
	t.Cleanup(func() {
		fixture.Close()
		for len(available) > 0 {
			(<-available).Close()
		}
	})
	return browserCompatibilityFixture{origin: fixture.URL, available: available}
}

func TestBrowserCompatibilityIPv6HTTPAndWebSocket(t *testing.T) {
	t.Run("explicit", func(t *testing.T) { browserCompatibilityIPv6Transfer(t, false) })
	t.Run("wildcard", func(t *testing.T) { browserCompatibilityIPv6Transfer(t, true) })
}

func browserCompatibilityIPv6Transfer(t *testing.T, wildcard bool) {
	t.Helper()
	fixture := newBrowserCompatibilityFixture(t, true, wildcard)
	client := &http.Client{Transport: &http.Transport{}, Timeout: 3 * time.Second}
	response, err := client.Get(fixture.origin)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	client.CloseIdleConnections()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("IPv6 helper page status=%d cache=%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	conn, response, err := dialer.Dial("ws"+strings.TrimPrefix(fixture.origin, "http")+"/websocket?token=integration-capability", http.Header{"Origin": {fixture.origin}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	response.Body.Close()
	select {
	case accepted := <-fixture.available:
		defer accepted.Close()
		if err := accepted.WriteMessage(websocket.TextMessage, []byte("task-transfer")); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, payload, err := conn.ReadMessage()
		if err != nil || string(payload) != "task-transfer" {
			t.Fatalf("IPv6 task transfer payload=%q error=%v", payload, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("IPv6 WebSocket was not admitted")
	}
}

const browserCompatibilityDriver = `
const {chromium} = require(process.env.XRAY_BROWSER_PLAYWRIGHT_PATH);
const configuration = JSON.parse(process.argv[1]);
(async () => {
  const context = await chromium.launchPersistentContext(configuration.profile, {
    executablePath: process.env.XRAY_BROWSER_CHROME,
    headless: true,
    chromiumSandbox: true,
    serviceWorkers: 'block',
    args: [
      '--disable-background-networking', '--disable-component-update',
      '--disable-default-apps', '--disable-sync', '--disable-extensions',
      '--no-first-run', '--no-default-browser-check',
      '--proxy-server=' + configuration.proxy,
      '--proxy-bypass-list=localhost;127.0.0.1;[::1]',
      '--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE localhost, EXCLUDE 127.0.0.1, EXCLUDE ::1, EXCLUDE [::1]'
    ]
  });
  let pageErrors = 0;
  const pages = [];
  try {
    console.error('Chrome ' + context.browser().version());
    const statuses = [];
    for (const url of configuration.urls) {
      const page = await context.newPage();
      page.on('pageerror', () => { pageErrors += 1; });
      const response = await page.goto(url, {waitUntil: 'load'});
      statuses.push(response.status());
      if (response.status() === 200) pages.push(page);
    }
    console.log('READY ' + JSON.stringify(statuses));
    process.stdin.resume();
    await new Promise(resolve => process.stdin.on('end', resolve));
    for (const page of pages) {
      await page.waitForFunction(() => clientIdleCount === 1 && upstreamGetCount === 0 && upstreamWsCount === 0 && upstreamPostCount === 0, null, {timeout: 3000});
    }
  } finally {
    await context.close();
  }
  if (pageErrors !== 0) throw new Error('Unhandled browser page errors: ' + pageErrors);
})().catch(error => { console.error(error.stack); process.exitCode = 1; });
`

func startBrowserCompatibilityChrome(t *testing.T, origins []string) []int {
	t.Helper()
	browserCompatibilityChromeDependencies(t)
	guard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "non-loopback requests are disabled", http.StatusForbidden)
	}))
	t.Cleanup(guard.Close)
	configuration, err := json.Marshal(map[string]any{"profile": t.TempDir(), "proxy": guard.URL, "urls": origins})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	command := exec.CommandContext(ctx, os.Getenv("XRAY_BROWSER_NODE"), "-e", browserCompatibilityDriver, string(configuration))
	input, err := command.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		input.Close()
		if err := command.Wait(); err != nil {
			t.Errorf("Chrome driver: %v", err)
		}
		cancel()
		t.Log(strings.TrimSpace(diagnostics.String()))
	})
	ready := make(chan bool, 1)
	var statuses []int
	go func() {
		scanner := bufio.NewScanner(output)
		if !scanner.Scan() || !strings.HasPrefix(scanner.Text(), "READY ") {
			ready <- false
			return
		}
		ready <- json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "READY ")), &statuses) == nil && len(statuses) == len(origins)
	}()
	select {
	case success := <-ready:
		if !success {
			t.Fatal("Chrome driver exited before helper pages loaded")
		}
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("Chrome driver readiness timed out")
	}
	return statuses
}

func browserCompatibilityReadStream(t *testing.T, conn *websocket.Conn, wantClose int) string {
	t.Helper()
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	var body strings.Builder
	for {
		kind, payload, err := conn.ReadMessage()
		if err != nil {
			if wantClose != 0 && !websocket.IsCloseError(err, wantClose) {
				t.Fatalf("stream ended with %v, want WebSocket close %d", err, wantClose)
			}
			if wantClose == 4000 {
				var closeError *websocket.CloseError
				if !errors.As(err, &closeError) || closeError.Text != "Browser task failed" {
					t.Fatalf("task failure did not use a generic close reason: %v", err)
				}
			}
			if wantClose == 0 && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseNoStatusReceived) {
				t.Fatalf("stream ended with %v", err)
			}
			return body.String()
		}
		if kind != websocket.BinaryMessage {
			t.Fatalf("stream message type = %d", kind)
		}
		body.Write(payload)
	}
}

func TestBrowserCompatibilityChrome(t *testing.T) {
	browserCompatibilityChromeDependencies(t)
	primary := newBrowserCompatibilityFixture(t, false, false)
	alias := newBrowserCompatibilityFixture(t, false, false)
	alias.origin = strings.Replace(alias.origin, "127.0.0.1", "localhost", 1)
	wildcard := newBrowserCompatibilityFixture(t, false, true)
	wildcardAlias := newBrowserCompatibilityFixture(t, false, true)
	wildcardAlias.origin = strings.Replace(wildcardAlias.origin, "127.0.0.1", "localhost", 1)
	fixtures := []browserCompatibilityFixture{primary, alias, wildcard, wildcardAlias}
	t.Run("ipv6-browser-availability", func(probeT *testing.T) {
		listener, err := net.Listen("tcp6", "[::1]:0")
		if err != nil {
			browserCompatibilityIPv6Unavailable(probeT, err)
		}
		defer listener.Close()
		probe, err := net.DialTimeout("tcp6", listener.Addr().String(), time.Second)
		if err != nil {
			browserCompatibilityIPv6Unavailable(probeT, err)
		}
		probe.Close()
		fixtures = append(fixtures, newBrowserCompatibilityFixture(t, true, false), newBrowserCompatibilityFixture(t, true, true))
		alias := newBrowserCompatibilityFixture(t, true, true)
		alias.origin = strings.Replace(alias.origin, "[::1]", "localhost", 1)
		fixtures = append(fixtures, alias)
	})
	var origins []string
	for _, fixture := range fixtures {
		origins = append(origins, fixture.origin)
	}
	var cacheHits, redirectHits, upstreamHits atomic.Int32
	streamClosed := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "X-Browser-Test")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		upstreamHits.Add(1)
		switch r.URL.Path {
		case "/echo", "/packet":
			if r.Method == http.MethodPost {
				body, err := io.ReadAll(r.Body)
				wantHeader := "packet"
				if r.URL.Path == "/packet" {
					wantHeader = ""
				}
				if err != nil || string(body) != "binary-packet" || r.Header.Get("X-Browser-Test") != wantHeader {
					http.Error(w, "invalid packet", http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusOK)
				return
			}
			fmt.Fprintf(w, "%s|%s|%s", r.Host, r.Header.Get("Origin"), r.Header.Get("X-Browser-Test"))
		case "/cache":
			w.Header().Set("Cache-Control", "public, max-age=3600")
			fmt.Fprintf(w, "cache-%d", cacheHits.Add(1))
		case "/redirect":
			http.Redirect(w, r, "/redirect-target", http.StatusFound)
		case "/redirect-target":
			redirectHits.Add(1)
			io.WriteString(w, "unexpected redirect target")
		case "/error":
			http.Error(w, "error body must not enter tunnel", http.StatusServiceUnavailable)
		case "/stream":
			io.WriteString(w, "stream-open")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			streamClosed <- struct{}{}
		case "/ws":
			if r.Header.Get("Sec-WebSocket-Protocol") != "ZWFybHk" {
				http.Error(w, "invalid WebSocket protocol", http.StatusBadRequest)
				return
			}
			upgrader := websocket.Upgrader{Subprotocols: []string{"ZWFybHk"}, CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == primary.origin }}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			conn.SetReadDeadline(time.Now().Add(4 * time.Second))
			kind, payload, err := conn.ReadMessage()
			if err == nil {
				conn.WriteMessage(kind, append([]byte("echo:"), payload...))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	statuses := startBrowserCompatibilityChrome(t, origins)
	mu.Lock()
	original := conns
	conns = primary.available
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		conns = original
		mu.Unlock()
	})
	for index, fixture := range fixtures {
		t.Run(fmt.Sprintf("bind-%d", index), func(t *testing.T) {
			if statuses[index] != http.StatusOK {
				t.Fatalf("real Chrome helper navigation to %s returned HTTP %d", fixture.origin, statuses[index])
			}
			mu.Lock()
			conns = fixture.available
			mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			conn, err := DialGetContext(ctx, upstream.URL+"/echo", http.Header{"X-Browser-Test": {"get"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := strings.TrimPrefix(upstream.URL, "http://") + "|" + fixture.origin + "|get"
			if got := browserCompatibilityReadStream(t, conn, 0); got != want {
				t.Fatalf("upstream Host/Origin/header = %q, want %q", got, want)
			}
		})
	}
	mu.Lock()
	conns = primary.available
	mu.Unlock()
	t.Run("post-echo", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if err := DialPacketContext(ctx, "POST", upstream.URL+"/echo", http.Header{"X-Browser-Test": {"packet"}}, nil, []byte("binary-packet")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("no-store", func(t *testing.T) {
		for iteration := 1; iteration <= 2; iteration++ {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			conn, err := DialGetContext(ctx, upstream.URL+"/cache", http.Header{"X-Browser-Test": {"cache"}}, nil)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			if got, want := browserCompatibilityReadStream(t, conn, 0), fmt.Sprintf("cache-%d", iteration); got != want {
				t.Fatalf("response = %q, want %q", got, want)
			}
		}
		if cacheHits.Load() != 2 {
			t.Fatalf("upstream cache hits = %d", cacheHits.Load())
		}
	})
	for _, path := range []string{"/error", "/redirect"} {
		t.Run("get"+path, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			conn, err := DialGetContext(ctx, upstream.URL+path, http.Header{"X-Browser-Test": {"failure"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if body := browserCompatibilityReadStream(t, conn, 4000); body != "" {
				t.Fatalf("failed HTTP response entered tunnel: %q", body)
			}
		})
		t.Run("post"+path, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			if err := DialPacketContext(ctx, "POST", upstream.URL+path, http.Header{"X-Browser-Test": {"failure"}}, nil, []byte("packet")); err == nil {
				t.Fatal("failed HTTP request was accepted")
			}
		})
	}
	if redirectHits.Load() != 0 {
		t.Errorf("redirect target reached %d times", redirectHits.Load())
	}
	t.Run("cancel-stream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		conn, err := DialGetContext(ctx, upstream.URL+"/stream", http.Header{"X-Browser-Test": {"stream"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		kind, payload, err := conn.ReadMessage()
		if err != nil || kind != websocket.BinaryMessage || string(payload) != "stream-open" {
			t.Fatalf("stream opening type=%d payload=%q error=%v", kind, payload, err)
		}
		conn.Close()
		select {
		case <-streamClosed:
		case <-time.After(3 * time.Second):
			t.Fatal("upstream Fetch survived helper client close")
		}
	})
	t.Run("websocket-echo", func(t *testing.T) {
		var conn *websocket.Conn
		select {
		case conn = <-primary.available:
		case <-time.After(4 * time.Second):
			t.Fatal("no browser WebSocket available")
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(4 * time.Second))
		if err := conn.WriteJSON(task{Method: "WS", URL: "ws" + strings.TrimPrefix(upstream.URL, "http") + "/ws", Extra: webSocketExtra{Protocol: "ZWFybHk"}, StreamResponse: true}); err != nil {
			t.Fatal(err)
		}
		if err := CheckOK(conn); err != nil {
			t.Fatal(err)
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, []byte("socket-payload")); err != nil {
			t.Fatal(err)
		}
		kind, payload, err := conn.ReadMessage()
		if err != nil || kind != websocket.BinaryMessage || string(payload) != "echo:socket-payload" {
			t.Fatalf("WebSocket echo type=%d payload=%q error=%v", kind, payload, err)
		}
	})
	t.Run("empty-metadata", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		conn, err := DialGetContext(ctx, upstream.URL+"/cache", nil, nil)
		if err != nil {
			t.Fatalf("GET without extra metadata: %v", err)
		}
		if body := browserCompatibilityReadStream(t, conn, 0); body != "cache-3" {
			t.Fatalf("GET without extra metadata returned %q", body)
		}
	})
	t.Run("empty-metadata-post", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := DialPacketContext(ctx, "POST", upstream.URL+"/packet", nil, nil, []byte("binary-packet")); err != nil {
			t.Fatalf("POST without extra metadata: %v", err)
		}
	})
	controlJSON := func(fields map[string]any) string {
		value := map[string]any{"method": "GET", "url": upstream.URL + "/cache", "streamResponse": true}
		for name, field := range fields {
			value[name] = field
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	for _, test := range []struct {
		name, payload string
	}{
		{"malformed-json", "{"},
		{"binary-control", controlJSON(nil)},
		{"null-task", "null"},
		{"array-task", "[]"},
		{"method-type", controlJSON(map[string]any{"method": 7})},
		{"response-type", controlJSON(map[string]any{"streamResponse": "true"})},
		{"extra-type", controlJSON(map[string]any{"extra": []string{}})},
		{"header-value", controlJSON(map[string]any{"extra": map[string]any{"headers": map[string]any{"X-Test": 7}}})},
		{"cookie-value", controlJSON(map[string]any{"extra": map[string]any{"cookies": map[string]any{"session": false}}})},
		{"referrer-type", controlJSON(map[string]any{"extra": map[string]any{"referrer": 7}})},
		{"referrer-url", controlJSON(map[string]any{"extra": map[string]any{"referrer": "://"}})},
		{"header-name", controlJSON(map[string]any{"extra": map[string]any{"headers": map[string]any{"bad\nname": "value"}}})},
		{"target-scheme", controlJSON(map[string]any{"url": "ftp://127.0.0.1/"})},
		{"ws-protocol-type", controlJSON(map[string]any{"method": "WS", "url": "ws" + strings.TrimPrefix(upstream.URL, "http") + "/ws", "extra": map[string]any{"protocol": 7}})},
		{"ws-protocol-token", controlJSON(map[string]any{"method": "WS", "url": "ws" + strings.TrimPrefix(upstream.URL, "http") + "/ws", "extra": map[string]any{"protocol": "invalid token"}})},
	} {
		t.Run("invalid-control/"+test.name, func(t *testing.T) {
			var conn *websocket.Conn
			select {
			case conn = <-primary.available:
			case <-time.After(4 * time.Second):
				t.Fatal("no browser control connection available")
			}
			before := upstreamHits.Load()
			messageType := websocket.TextMessage
			if test.name == "binary-control" {
				messageType = websocket.BinaryMessage
			}
			if err := conn.WriteMessage(messageType, []byte(test.payload)); err != nil {
				conn.Close()
				t.Fatal(err)
			}
			if body := browserCompatibilityReadStream(t, conn, 4000); body != "" {
				t.Fatalf("invalid control returned data: %q", body)
			}
			if got := upstreamHits.Load(); got != before {
				t.Fatalf("invalid control reached upstream: %d -> %d", before, got)
			}
		})
	}
	t.Run("after-invalid-control", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		conn, err := DialGetContext(ctx, upstream.URL+"/cache", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if body := browserCompatibilityReadStream(t, conn, 0); body != "cache-4" {
			t.Fatalf("control recovery returned %q", body)
		}
	})
}
