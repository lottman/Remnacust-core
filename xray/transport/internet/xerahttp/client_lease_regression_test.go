package xerahttp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/transport/internet"
)

func leaseEventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("lease cleanup did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func leaseLocalServer(t *testing.T, network string) (*httptest.Server, xnet.Destination, *atomic.Int32) {
	t.Helper()
	address := "127.0.0.1:0"
	if network == "tcp6" {
		address = "[::1]:0"
	}
	listener, err := net.Listen(network, address)
	if err == nil && network == "tcp6" {
		var probe net.Conn
		probe, err = net.DialTimeout(network, listener.Addr().String(), time.Second)
		if probe != nil {
			probe.Close()
		}
	}
	if err != nil {
		if listener != nil {
			listener.Close()
		}
		if network == "tcp6" && os.Getenv("REMNACUST_REQUIRE_IPV6_TESTS") != "1" {
			t.Skipf("IPv6 loopback unavailable: %v", err)
		}
		t.Fatal(err)
	}
	posts := new(atomic.Int32)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		io.Copy(io.Discard, r.Body)
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	t.Cleanup(server.CloseClientConnections)
	dest, err := xnet.ParseDestination("tcp:" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return server, dest, posts
}

func TestClientLeaseLogicalConcurrency(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			for _, limit := range []int32{1, 2} {
				t.Run(string(rune('0'+limit)), func(t *testing.T) {
					server, dest, posts := leaseLocalServer(t, network)
					settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: &Config{Mode: "packet-up", Host: server.Listener.Addr().String(), ScMaxEachPostBytes: &RangeConfig{From: 1, To: 1}, ScMinPostsIntervalMs: &RangeConfig{}, Xmux: &XmuxConfig{MaxConcurrency: &RangeConfig{From: limit, To: limit}}}}
					t.Cleanup(func() { settings.Close() })
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					for range 2 {
						conn, err := Dial(ctx, dest, settings)
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { conn.Close() })
						if _, err := conn.Write([]byte("abcd")); err != nil {
							t.Fatal(err)
						}
					}
					leaseEventually(t, func() bool { return posts.Load() == 8 })
					globalDialerAccess.Lock()
					manager := globalDialerMap[dialerConf{dest, settings}]
					count := len(manager.xmuxClients)
					var running int32
					for _, client := range manager.xmuxClients {
						running += client.Running.Load()
					}
					globalDialerAccess.Unlock()
					if count != int(2/limit) || running != 2 {
						t.Fatalf("two logical sessions, 8 acknowledged packets: clients=%d running=%d limit=%d", count, running, limit)
					}
				})
			}
		})
	}
}

type leaseProbeClient struct {
	closed     atomic.Int32
	posts      atomic.Int32
	opens      atomic.Int32
	readClosed atomic.Int32
	failOpen   int32
	openErr    error
	started    chan struct{}
	canceled   chan struct{}
	cleanup    <-chan struct{}
}

func (c *leaseProbeClient) IsClosed() bool { return c.closed.Load() != 0 }
func (c *leaseProbeClient) Close() error   { c.closed.Add(1); return nil }
func (c *leaseProbeClient) OpenStream(context.Context, string, string, io.Reader, bool) (io.ReadCloser, xnet.Addr, xnet.Addr, error) {
	if c.opens.Add(1) == c.failOpen {
		return nil, nil, nil, c.openErr
	}
	return &leaseProbeReader{Reader: bytes.NewReader(nil), closed: &c.readClosed}, nil, nil, nil
}

type leaseProbeReader struct {
	io.Reader
	closed *atomic.Int32
}

func (r *leaseProbeReader) Close() error { r.closed.Add(1); return nil }

func (c *leaseProbeClient) PostPacket(ctx context.Context, _, _, _ string, payload buf.MultiBuffer) error {
	defer buf.ReleaseMulti(payload)
	c.posts.Add(1)
	if c.started != nil {
		close(c.started)
		<-ctx.Done()
		close(c.canceled)
		<-c.cleanup
		return ctx.Err()
	}
	return nil
}

func leaseSeedClient(t *testing.T, settings *internet.MemoryStreamConfig, dest xnet.Destination, fake *leaseProbeClient) *XmuxClient {
	t.Helper()
	original, lease := getHTTPClient(context.Background(), dest, settings)
	releaseHTTPClient(lease, false)
	if closer, ok := original.(io.Closer); ok {
		closer.Close()
	}
	globalDialerAccess.Lock()
	lease.XmuxConn = fake
	globalDialerAccess.Unlock()
	t.Cleanup(func() { settings.Close() })
	return lease
}

func TestClientLeaseRetirementWaitsForPendingPostCleanup(t *testing.T) {
	settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: &Config{Mode: "packet-up", Xmux: &XmuxConfig{MaxConcurrency: &RangeConfig{From: 1, To: 1}}}}
	dest := xnet.TCPDestination(xnet.LocalHostIP, 1)
	cleanup := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(cleanup) }) })
	fake := &leaseProbeClient{started: make(chan struct{}), canceled: make(chan struct{}), cleanup: cleanup}
	lease := leaseSeedClient(t, settings, dest, fake)
	conn, err := Dial(context.Background(), dest, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fake.started:
	case <-time.After(time.Second):
		t.Fatal("POST did not start")
	}
	settings.Close()
	conn.Close()
	select {
	case <-fake.canceled:
	case <-time.After(time.Second):
		t.Fatal("POST was not canceled")
	}
	if lease.Running.Load() != 0 || lease.LifecycleRefs.Load() < 1 || fake.closed.Load() != 0 {
		t.Fatalf("retired transport closed before request cleanup: running=%d refs=%d closed=%d", lease.Running.Load(), lease.LifecycleRefs.Load(), fake.closed.Load())
	}
	once.Do(func() { close(cleanup) })
	leaseEventually(t, func() bool { return lease.LifecycleRefs.Load() == 0 && fake.closed.Load() == 1 })
	lease.maybeClose()
	if fake.closed.Load() != 1 {
		t.Fatal("transport closed more than once")
	}
}

func TestClientLeaseRotationReleasesEveryTransport(t *testing.T) {
	settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: &Config{Mode: "packet-up", ScMaxEachPostBytes: &RangeConfig{From: 1, To: 1}, Xmux: &XmuxConfig{MaxConcurrency: &RangeConfig{From: 1, To: 1}, HMaxRequestTimes: &RangeConfig{From: 2, To: 2}}}}
	dest := xnet.TCPDestination(xnet.LocalHostIP, 1)
	first := new(leaseProbeClient)
	leaseSeedClient(t, settings, dest, first)
	var mu sync.Mutex
	created := []*leaseProbeClient{first}
	globalDialerAccess.Lock()
	manager := globalDialerMap[dialerConf{dest, settings}]
	manager.newConnFunc = func() XmuxConn {
		client := new(leaseProbeClient)
		mu.Lock()
		created = append(created, client)
		mu.Unlock()
		return client
	}
	globalDialerAccess.Unlock()
	conn, err := Dial(context.Background(), dest, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Write([]byte("abcde")); err != nil {
		t.Fatal(err)
	}
	leaseEventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		var total int32
		for _, client := range created {
			total += client.posts.Load()
		}
		return total == 5
	})
	settings.Close()
	conn.Close()
	leaseEventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, client := range created {
			if client.closed.Load() != 1 {
				return false
			}
		}
		return true
	})
}

func TestClientLeaseDialFailuresReleaseHolders(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        string
		download    bool
		mainFailure int32
		downFailure int32
	}{
		{name: "stream-one", mode: "stream-one", mainFailure: 1},
		{name: "packet-down", mode: "packet-up", mainFailure: 1},
		{name: "stream-up", mode: "stream-up", mainFailure: 2},
		{name: "separate-down", mode: "packet-up", download: true, downFailure: 1},
		{name: "separate-up", mode: "stream-up", download: true, mainFailure: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := errors.New("controlled stream failure")
			dest := xnet.TCPDestination(xnet.LocalHostIP, 1)
			config := &Config{Mode: tc.mode}
			if tc.download {
				config.DownloadSettings = leaseDownloadConfig(dest).DownloadSettings
			}
			settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: config}
			main := &leaseProbeClient{failOpen: tc.mainFailure, openErr: want}
			leases := []*XmuxClient{leaseSeedClient(t, settings, dest, main)}
			clients := []*leaseProbeClient{main}
			if tc.download {
				child, err := prepareDownloadSettings(settings, config)
				if err != nil {
					t.Fatal(err)
				}
				down := &leaseProbeClient{failOpen: tc.downFailure, openErr: want}
				leases = append(leases, leaseSeedClient(t, child, dest, down))
				clients = append(clients, down)
			}
			conn, err := Dial(context.Background(), dest, settings)
			if conn != nil {
				conn.Close()
			}
			if conn != nil || !errors.Is(err, want) {
				t.Fatalf("conn=%v error=%v", conn, err)
			}
			for _, lease := range leases {
				if lease.Running.Load() != 0 || lease.LifecycleRefs.Load() != 0 {
					t.Fatalf("failed Dial retained holders: running=%d refs=%d", lease.Running.Load(), lease.LifecycleRefs.Load())
				}
			}
			settings.Close()
			for _, client := range clients {
				expectedReaders := client.opens.Load()
				if client.failOpen > 0 && client.failOpen <= expectedReaders {
					expectedReaders--
				}
				if client.closed.Load() != 1 || client.readClosed.Load() != expectedReaders {
					t.Fatalf("failed Dial cleanup: closed=%d readersClosed=%d expected=%d", client.closed.Load(), client.readClosed.Load(), expectedReaders)
				}
			}
		})
	}
}

func TestClientLeaseCanceledDialCreatesNoPool(t *testing.T) {
	settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: &Config{Mode: "packet-up"}}
	t.Cleanup(func() { settings.Close() })
	dest := xnet.TCPDestination(xnet.LocalHostIP, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if conn, err := Dial(ctx, dest, settings); conn != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Dial: %v %v", conn, err)
	}
	globalDialerAccess.Lock()
	_, found := globalDialerMap[dialerConf{dest, settings}]
	globalDialerAccess.Unlock()
	if found {
		t.Fatal("canceled Dial created a pool")
	}
}

func TestDownloadLeaseRetirementPreservesActiveStream(t *testing.T) {
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	t.Cleanup(func() { proceedOnce.Do(func() { close(proceed) }) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("a"))
		w.(http.Flusher).Flush()
		select {
		case <-proceed:
		case <-r.Context().Done():
			return
		}
		w.Write([]byte("b"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	t.Cleanup(server.CloseClientConnections)
	dest, err := xnet.ParseDestination("tcp:" + server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	config := leaseDownloadConfig(dest)
	settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: config}
	t.Cleanup(func() { settings.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := Dial(ctx, dest, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	one := make([]byte, 1)
	if _, err := io.ReadFull(conn, one); err != nil || one[0] != 'a' {
		t.Fatalf("before retirement: %q %v", one, err)
	}
	child, err := settings.GetDownloadSettings()
	if err != nil {
		t.Fatal(err)
	}
	globalDialerAccess.Lock()
	main := globalDialerMap[dialerConf{dest, settings}].xmuxClients[0]
	down := globalDialerMap[dialerConf{dest, child}].xmuxClients[0]
	globalDialerAccess.Unlock()
	settings.Close()
	if main.XmuxConn.IsClosed() || down.XmuxConn.IsClosed() {
		t.Fatal("retirement killed an active transport")
	}
	proceedOnce.Do(func() { close(proceed) })
	if _, err := io.ReadFull(conn, one); err != nil || one[0] != 'b' {
		t.Fatalf("after retirement: %q %v", one, err)
	}
	conn.Close()
	leaseEventually(t, func() bool {
		return main.XmuxConn.IsClosed() && down.XmuxConn.IsClosed() && main.LifecycleRefs.Load() == 0 && down.LifecycleRefs.Load() == 0
	})
}

func leaseDownloadConfig(dest xnet.Destination) *Config {
	return &Config{Mode: "packet-up", DownloadSettings: &internet.StreamConfig{Address: xnet.NewIPOrDomain(dest.Address), Port: uint32(dest.Port), ProtocolName: protocolName, TransportSettings: []*internet.TransportConfig{{ProtocolName: protocolName, Settings: serial.ToTypedMessage(&Config{})}}}}
}

func TestDownloadLeaseRejectsRetiredParentAndChild(t *testing.T) {
	dest := xnet.TCPDestination(xnet.LocalHostIP, 1)
	for _, cached := range []bool{false, true} {
		config := leaseDownloadConfig(dest)
		settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: config}
		if cached {
			if _, err := prepareDownloadSettings(settings, config); err != nil {
				t.Fatal(err)
			}
		}
		settings.Close()
		conn, err := Dial(context.Background(), dest, settings)
		if conn != nil {
			conn.Close()
		}
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("closed parent cached=%t: conn=%v err=%v", cached, conn, err)
		}
		if child := settings.DownloadSettings; child != nil && !child.IsClosed() {
			t.Fatal("parent retained an open child")
		}
	}
	config := leaseDownloadConfig(dest)
	settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: config}
	child, err := prepareDownloadSettings(settings, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { settings.Close() })
	child.Close()
	if _, err := prepareDownloadSettings(settings, config); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed cached child accepted: %v", err)
	}
	if client, lease := acquireHTTPClient(context.Background(), dest, child, settings, false); lease != nil || !client.IsClosed() {
		t.Fatal("retired child created a pool")
	}
}

func TestDownloadLeaseValidatesCachedSettings(t *testing.T) {
	dest := xnet.TCPDestination(xnet.LocalHostIP, 1)
	cases := []*internet.MemoryStreamConfig{
		{ProtocolSettings: &Config{}},
		{Destination: &xnet.Destination{Port: 443}, ProtocolSettings: &Config{}},
		{Destination: &xnet.Destination{Address: xnet.LocalHostIP}, ProtocolSettings: &Config{}},
		{Destination: &dest, ProtocolSettings: "wrong-transport"},
		{Destination: &dest, ProtocolSettings: (*Config)(nil)},
	}
	for _, child := range cases {
		config := leaseDownloadConfig(dest)
		settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: config, DownloadSettings: child}
		if _, err := prepareDownloadSettings(settings, config); err == nil {
			t.Error("invalid cached download accepted")
		}
		settings.Close()
	}
}

type leaseAuthKey struct{}
type leaseRoundTrip func(*http.Request) (*http.Response, error)

func (f leaseRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGetExistingPoolPreservesRequestAuthContext(t *testing.T) {
	settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: &Config{}}
	t.Cleanup(func() { settings.Close() })
	dest := xnet.TCPDestination(xnet.LocalHostIP, 1)
	first, lease := getHTTPClient(context.WithValue(context.Background(), leaseAuthKey{}, "first"), dest, settings)
	releaseHTTPClient(lease, false)
	ctx := context.WithValue(context.Background(), leaseAuthKey{}, "second")
	second, lease := getHTTPClient(ctx, dest, settings)
	defer releaseHTTPClient(lease, false)
	if first != second {
		t.Fatal("did not select the existing HTTP pool")
	}
	raw, peer := net.Pipe()
	defer raw.Close()
	defer peer.Close()
	client := second.(*DefaultDialerClient)
	roundTrip := leaseRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Context().Value(leaseAuthKey{}) != "second" {
			t.Error("reused pool lost or captured stale request authorization context")
		}
		if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.GotConn != nil {
			trace.GotConn(httptrace.GotConnInfo{Conn: raw})
		}
		if r.Body != nil {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
	})
	client.client.Transport = roundTrip
	if client.uploadClient != nil {
		client.uploadClient.Transport = roundTrip
	}
	reader, _, _, err := client.OpenStream(ctx, "http://127.0.0.1/", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(reader)
	reader.Close()
	if err := client.PostPacket(ctx, "http://127.0.0.1/", "session", "0", buf.MergeBytes(nil, []byte("packet"))); err != nil {
		t.Fatal(err)
	}
}
