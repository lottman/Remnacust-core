package tcp_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
	"github.com/xtls/xray-core/common/serial"
	testtcp "github.com/xtls/xray-core/testing/servers/tcp"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/finalmask"
	"github.com/xtls/xray-core/transport/internet/httpupgrade"
	"github.com/xtls/xray-core/transport/internet/reality"
	"github.com/xtls/xray-core/transport/internet/stat"
	rawtcp "github.com/xtls/xray-core/transport/internet/tcp"
	"github.com/xtls/xray-core/transport/internet/tls"
	"github.com/xtls/xray-core/transport/internet/websocket"
)

type cleanupTrackedConn struct {
	net.Conn
	closes   atomic.Int32
	writeErr error
}

func TestHTTPUpgradeRejectedResponseClosesOwnedRaw(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	observed := make(chan error, 1)
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			observed <- err
			return
		}
		defer peer.Close()
		peer.SetDeadline(time.Now().Add(3 * time.Second))
		request, err := http.ReadRequest(bufio.NewReader(peer))
		if err != nil {
			observed <- err
			return
		}
		request.Body.Close()
		if _, err := io.WriteString(peer, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"); err != nil {
			observed <- err
			return
		}
		n, err := peer.Read(make([]byte, 1))
		if n != 0 || err != io.EOF {
			observed <- errors.New("rejected HTTP upgrade retained raw socket")
			return
		}
		observed <- nil
	}()
	dialed := make(chan *cleanupTrackedConn, 1)
	settings := &internet.MemoryStreamConfig{ProtocolName: "httpupgrade", ProtocolSettings: &httpupgrade.Config{Path: "/cleanup"}, FinalMask: cleanupMask(dialed, nil)}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := httpupgrade.Dial(ctx, xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(listener.Addr().(*net.TCPAddr).Port)), settings)
	if conn != nil {
		conn.Close()
		t.Fatal("rejected upgrade returned a connection")
	}
	if err == nil || !strings.Contains(err.Error(), "unrecognized reply") {
		t.Fatal(err)
	}
	tracked := <-dialed
	defer tracked.Conn.Close()
	if err := <-observed; err != nil {
		t.Error(err)
	}
	if count := tracked.closes.Load(); count != 1 {
		t.Fatalf("raw Close calls=%d, want exactly one", count)
	}
}

func (c *cleanupTrackedConn) Close() error {
	c.closes.Add(1)
	return c.Conn.Close()
}

func (c *cleanupTrackedConn) Write(data []byte) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return c.Conn.Write(data)
}

type cleanupTransport struct {
	name   string
	config func() any
	dial   func(context.Context, xnet.Destination, *internet.MemoryStreamConfig) (stat.Connection, error)
	listen func(context.Context, xnet.Address, xnet.Port, *internet.MemoryStreamConfig, internet.ConnHandler) (internet.Listener, error)
}

var cleanupTransports = []cleanupTransport{
	{"tcp", func() any { return &rawtcp.Config{} }, rawtcp.Dial, rawtcp.ListenTCP},
	{"websocket", func() any { return &websocket.Config{Path: "/cleanup"} }, websocket.Dial, websocket.ListenWS},
	{"httpupgrade", func() any { return &httpupgrade.Config{Path: "/cleanup"} }, httpupgrade.Dial, httpupgrade.ListenHTTPUpgrade},
}

func cleanupMask(dialed chan<- *cleanupTrackedConn, writeErr error) *finalmask.FinalMask {
	return finalmask.NewFinalMask(nil, nil, func(ctx context.Context, dest xnet.Destination) (xnet.Conn, error) {
		raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", dest.NetAddr())
		if err != nil {
			return nil, err
		}
		tracked := &cleanupTrackedConn{Conn: raw, writeErr: writeErr}
		dialed <- tracked
		return tracked, nil
	}, nil, nil, nil)
}

func TestDialFailureClosesOwnedRawConnection(t *testing.T) {
	for _, transport := range cleanupTransports {
		t.Run(transport.name+"/legacy-minimum13", func(t *testing.T) {
			settings := &internet.MemoryStreamConfig{ProtocolName: transport.name, ProtocolSettings: transport.config(), SecurityType: "tls", SecuritySettings: &tls.Config{ServerName: "localhost", Fingerprint: "hellochrome_58", MinVersion: "1.3"}}
			testCleanupFailure(t, transport, settings, nil, "version policy")
		})
	}
	for _, scenario := range []string{"reality", "header-instance", "header-authenticator"} {
		t.Run("tcp/"+scenario, func(t *testing.T) {
			settings := &internet.MemoryStreamConfig{ProtocolName: "tcp", ProtocolSettings: &rawtcp.Config{}}
			want := ""
			switch scenario {
			case "reality":
				settings.SecurityType = "reality"
				settings.SecuritySettings = &reality.Config{Fingerprint: "nonexistent-fingerprint"}
				want = "fingerprint"
			case "header-instance":
				settings.ProtocolSettings = &rawtcp.Config{HeaderSettings: &serial.TypedMessage{Type: "remnacust.nonexistent"}}
				want = "get header settings"
			case "header-authenticator":
				settings.ProtocolSettings = &rawtcp.Config{HeaderSettings: serial.ToTypedMessage(&tls.Config{})}
				want = "create header authenticator"
			}
			testCleanupFailure(t, cleanupTransports[0], settings, nil, want)
		})
	}
	t.Run("httpupgrade/request-write", func(t *testing.T) {
		settings := &internet.MemoryStreamConfig{ProtocolName: "httpupgrade", ProtocolSettings: &httpupgrade.Config{}}
		testCleanupFailure(t, cleanupTransports[2], settings, errors.New("injected request write failure"), "injected request write failure")
	})
}

func testCleanupFailure(t *testing.T, transport cleanupTransport, settings *internet.MemoryStreamConfig, writeErr error, want string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	observed := make(chan error, 1)
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			observed <- err
			return
		}
		defer peer.Close()
		peer.SetReadDeadline(time.Now().Add(time.Second))
		n, err := peer.Read(make([]byte, 1))
		if n != 0 || err != io.EOF {
			observed <- errors.New("raw socket remained open or sent TLS/application bytes before policy failure")
			return
		}
		observed <- nil
	}()
	dialed := make(chan *cleanupTrackedConn, 1)
	settings.FinalMask = cleanupMask(dialed, writeErr)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := transport.dial(ctx, xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(listener.Addr().(*net.TCPAddr).Port)), settings)
	if conn != nil {
		conn.Close()
		t.Fatal("failed dial returned a connection")
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("wrong dial failure: %v", err)
	}
	tracked := <-dialed
	defer tracked.Conn.Close()
	if err := <-observed; err != nil {
		t.Error(err)
	}
	if count := tracked.closes.Load(); count != 1 {
		t.Fatalf("raw Close calls=%d, want exactly one", count)
	}
}

func TestDialTransfersUsableTLSConnectionAndRejectsUntrustedCertificate(t *testing.T) {
	for _, transport := range cleanupTransports {
		for _, trusted := range []bool{false, true} {
			name := "untrusted"
			if trusted {
				name = "trusted"
			}
			t.Run(transport.name+"/"+name, func(t *testing.T) {
				certificate, hash := cert.MustGenerate(nil, cert.CommonName("localhost"), cert.DNSNames("localhost"))
				serverSettings := &internet.MemoryStreamConfig{ProtocolName: transport.name, ProtocolSettings: transport.config(), SecurityType: "tls", SecuritySettings: &tls.Config{Certificate: []*tls.Certificate{tls.ParseCertificate(certificate)}, MinVersion: "1.2", MaxVersion: "1.2"}}
				port := testtcp.PickPort()
				server, err := transport.listen(context.Background(), xnet.LocalHostIP, port, serverSettings, func(c stat.Connection) {
					go func() { defer c.Close(); c.SetDeadline(time.Now().Add(3 * time.Second)); io.Copy(c, c) }()
				})
				if err != nil {
					t.Fatal(err)
				}
				defer server.Close()
				tlsConfig := &tls.Config{ServerName: "localhost", Fingerprint: "hellochrome_58", MinVersion: "1.2", MaxVersion: "1.2"}
				if trusted {
					tlsConfig.PinnedPeerCertSha256 = [][]byte{hash[:]}
				}
				dialed := make(chan *cleanupTrackedConn, 1)
				settings := &internet.MemoryStreamConfig{ProtocolName: transport.name, ProtocolSettings: transport.config(), SecurityType: "tls", SecuritySettings: tlsConfig, FinalMask: cleanupMask(dialed, nil)}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				conn, err := transport.dial(ctx, xnet.TCPDestination(xnet.LocalHostIP, port), settings)
				tracked := <-dialed
				defer tracked.Conn.Close()
				if !trusted {
					if conn != nil {
						conn.Close()
						t.Fatal("untrusted dial returned connection")
					}
					if err == nil || !strings.Contains(strings.ToLower(err.Error()), "certificate") {
						t.Fatalf("certificate validation lost: %v", err)
					}
					if count := tracked.closes.Load(); count != 1 {
						t.Fatalf("failed certificate raw Close calls=%d", count)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				if count := tracked.closes.Load(); count != 0 {
					t.Fatalf("transferred raw connection already closed %d times", count)
				}
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				if _, err := conn.Write([]byte("payload")); err != nil {
					t.Fatal(err)
				}
				got := make([]byte, 7)
				if _, err := io.ReadFull(conn, got); err != nil || string(got) != "payload" {
					t.Fatal(string(got), err)
				}
				if count := tracked.closes.Load(); count != 0 {
					t.Fatalf("live transferred socket closed %d times", count)
				}
			})
		}
	}
}
