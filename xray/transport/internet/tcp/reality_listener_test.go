package tcp_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	goreality "github.com/xtls/reality"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/proxy"
	"github.com/xtls/xray-core/proxy/vless"
	testtcp "github.com/xtls/xray-core/testing/servers/tcp"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/reality"
	"github.com/xtls/xray-core/transport/internet/stat"
	rawtcp "github.com/xtls/xray-core/transport/internet/tcp"
)

func realityTCPReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("RAW REALITY loopback fixture timed out")
		var zero T
		return zero
	}
}

func realityTCPConfig(t *testing.T, target string) *reality.Config {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &reality.Config{Type: "tcp", Dest: target, PrivateKey: key.Bytes()}
}

func realityTCPListen(t *testing.T, cfg *reality.Config) (internet.Listener, <-chan stat.Connection) {
	t.Helper()
	accepted := make(chan stat.Connection, 1)
	settings := &internet.MemoryStreamConfig{ProtocolName: "tcp", ProtocolSettings: &rawtcp.Config{}, SecurityType: "reality", SecuritySettings: cfg}
	listener, err := rawtcp.ListenTCP(context.Background(), xnet.LocalHostIP, testtcp.PickPort(), settings, func(conn stat.Connection) { accepted <- conn })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener, accepted
}

func realityTCPDial(t *testing.T, listener internet.Listener) *net.TCPConn {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn.(*net.TCPConn)
}

func realityTCPHeldTarget(t *testing.T) (string, <-chan net.Conn, <-chan net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted, closed := make(chan net.Conn, 512), make(chan net.Conn, 512)
	done := make(chan struct{})
	var mu sync.Mutex
	var conns []net.Conn
	var readers sync.WaitGroup
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			accepted <- conn
			readers.Go(func() { _, _ = io.Copy(io.Discard, conn); closed <- conn })
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
		readers.Wait()
	})
	return listener.Addr().String(), accepted, closed
}

func TestRAWRealityCloseReleasesPendingTargets(t *testing.T) {
	address, started, closed := realityTCPHeldTarget(t)
	listener, _ := realityTCPListen(t, realityTCPConfig(t, address))
	for range 8 {
		realityTCPDial(t, listener)
		realityTCPReceive(t, started)
	}
	_ = listener.Close()
	for range 8 {
		realityTCPReceive(t, closed)
	}
}

func TestRAWRealityBoundsPendingAdmission(t *testing.T) {
	address, started, closed := realityTCPHeldTarget(t)
	listener, accepted := realityTCPListen(t, realityTCPConfig(t, address))
	clients := make([]*net.TCPConn, 264)
	for i := range clients {
		clients[i] = realityTCPDial(t, listener)
	}
	for range 256 {
		realityTCPReceive(t, started)
	}
	select {
	case <-started:
		t.Fatal("264 stalled clients started more than 256 pending REALITY targets")
	case <-time.After(250 * time.Millisecond):
	}
	_ = clients[0].Close()
	realityTCPReceive(t, closed)
	realityTCPReceive(t, started)
	select {
	case <-started:
		t.Fatal("releasing one pending connection admitted more than one replacement")
	case <-time.After(250 * time.Millisecond):
	}
	_ = listener.Close()
	for range 256 {
		realityTCPReceive(t, closed)
	}
	select {
	case conn := <-accepted:
		_ = conn.Close()
		t.Fatal("unauthenticated client reached the RAW handler")
	default:
	}
}

func TestRAWRealityAbsentTargetClosesClients(t *testing.T) {
	target, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := target.Addr().String()
	_ = target.Close()
	listener, accepted := realityTCPListen(t, realityTCPConfig(t, address))
	for range 16 {
		client := realityTCPDial(t, listener)
		_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
		if n, err := client.Read(make([]byte, 1)); n != 0 || err == nil {
			t.Fatalf("unavailable target left client usable: n=%d err=%v", n, err)
		} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatal("unavailable target retained client", err)
		}
	}
	select {
	case conn := <-accepted:
		_ = conn.Close()
		t.Fatal("failed target dial reached the RAW handler")
	default:
	}
}

func TestRAWRealityVerifiedTLSFallback(t *testing.T) {
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "fallback-ok") }))
	target.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	target.StartTLS()
	defer target.Close()
	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	name := target.Certificate().DNSNames[0]
	listener, _ := realityTCPListen(t, realityTCPConfig(t, target.Listener.Addr().String()))
	client := tls.Client(realityTCPDial(t, listener), &tls.Config{RootCAs: roots, ServerName: name, MinVersion: tls.VersionTLS13})
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	if err := client.Handshake(); err != nil {
		t.Fatal(err)
	}
	if len(client.ConnectionState().VerifiedChains) == 0 {
		t.Fatal("fallback certificate was not verified")
	}
	if _, err := fmt.Fprintf(client, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", name); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(body) != "fallback-ok" {
		t.Fatalf("fallback response %q: %v", body, err)
	}
}

func TestRAWRealityAuthenticatedStreamsSurviveListenerClose(t *testing.T) {
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	target.EnableHTTP2 = true
	target.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	target.StartTLS()
	defer target.Close()
	defer target.CloseClientConnections()
	cfg := realityTCPConfig(t, target.Listener.Addr().String())
	cfg.ServerNames = []string{"example.com"}
	cfg.ShortIds = [][]byte{{1, 2, 3, 4, 5, 6, 7, 8}}
	private, err := ecdh.X25519().NewPrivateKey(cfg.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	listener, accepted := realityTCPListen(t, cfg)
	for deadline := time.Now().Add(12 * time.Second); ; {
		if value, ok := goreality.GlobalPostHandshakeRecordsLens.Load(cfg.Dest + " example.com 2"); ok {
			if _, ready := value.([]int); ready {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("local REALITY target detection did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	const active = 257
	clients, servers := make([]net.Conn, 0, active), make([]net.Conn, 0, active)
	var echoes sync.WaitGroup
	t.Cleanup(func() {
		for _, conn := range clients {
			_ = conn.Close()
		}
		for _, conn := range servers {
			_ = conn.Close()
		}
		done := make(chan struct{})
		go func() { echoes.Wait(); close(done) }()
		realityTCPReceive(t, done)
	})
	for range active {
		raw := realityTCPDial(t, listener)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		client, err := reality.UClient(raw, &reality.Config{Fingerprint: "chrome", ServerName: "example.com", PublicKey: private.PublicKey().Bytes(), ShortId: cfg.ShortIds[0]}, ctx, xnet.TCPDestination(xnet.DomainAddress("example.com"), 443))
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
		if !client.(*reality.UConn).Verified {
			t.Fatal("client did not authenticate REALITY")
		}
		server := realityTCPReceive(t, accepted)
		servers = append(servers, server)
		wrapped, ok := server.(*reality.Conn)
		if !ok {
			t.Fatalf("RAW handler received %T instead of *reality.Conn", server)
		}
		input, rawInput := vless.AccessTLSBuffers(wrapped.Conn)
		if input == nil || rawInput == nil {
			t.Fatal("REALITY connection does not expose Vision TLS buffers")
		}
		unwrapped, _, _ := proxy.UnwrapRawConn(wrapped)
		if unwrapped != wrapped.NetConn() {
			t.Fatal("Vision did not unwrap the original RAW connection")
		}
	}
	_ = listener.Close()
	time.Sleep(350 * time.Millisecond)
	for _, server := range servers {
		_ = server.SetDeadline(time.Now().Add(10 * time.Second))
		echoes.Go(func() { _, _ = io.Copy(server, server) })
	}
	for _, client := range clients {
		_ = client.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := client.Write([]byte{42}); err != nil {
			t.Fatal(err)
		}
		var data [1]byte
		if _, err := io.ReadFull(client, data[:]); err != nil || data[0] != 42 {
			t.Fatalf("transferred stream did not survive Close: %v", err)
		}
	}
	payload := bytes.Repeat([]byte{0x5a}, 16*1024)
	data := make([]byte, len(payload))
	for range 64 {
		if _, err := clients[0].Write(payload); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(clients[0], data); err != nil || !bytes.Equal(data, payload) {
			t.Fatalf("authenticated 1 MiB roundtrip failed: %v", err)
		}
	}
}
