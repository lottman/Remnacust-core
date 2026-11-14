package tls

import (
	"context"
	gotls "crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/transport/internet"
)

var (
	echIPv6TCPProbe = sync.OnceValue(func() error {
		listener, err := net.Listen("tcp6", "[::1]:0")
		if err != nil {
			return err
		}
		defer listener.Close()
		conn, err := net.DialTimeout("tcp6", listener.Addr().String(), time.Second)
		if err != nil {
			return err
		}
		return conn.Close()
	})
	echIPv6UDPProbe = sync.OnceValue(func() error {
		server, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
		if err != nil {
			return err
		}
		defer server.Close()
		client, err := net.DialUDP("udp6", nil, server.LocalAddr().(*net.UDPAddr))
		if err != nil {
			return err
		}
		defer client.Close()
		deadline := time.Now().Add(time.Second)
		if err := server.SetDeadline(deadline); err != nil {
			return err
		}
		if err := client.SetDeadline(deadline); err != nil {
			return err
		}
		probe := []byte("ECH IPv6 loopback probe")
		if n, err := client.Write(probe); err != nil {
			return err
		} else if n != len(probe) {
			return io.ErrShortWrite
		}
		buffer := make([]byte, len(probe)+1)
		n, source, err := server.ReadFromUDP(buffer)
		if err != nil {
			return err
		}
		if !slices.Equal(buffer[:n], probe) || !source.IP.Equal(net.IPv6loopback) {
			return errors.New("IPv6 UDP probe received an unexpected datagram")
		}
		if n, err := server.WriteToUDP(buffer[:n], source); err != nil {
			return err
		} else if n != len(probe) {
			return io.ErrShortWrite
		}
		n, err = client.Read(buffer)
		if err != nil {
			return err
		}
		if !slices.Equal(buffer[:n], probe) {
			return errors.New("IPv6 UDP echo did not match the probe")
		}
		return nil
	})
)

func echRequireIPv6Loopback(t *testing.T, networks ...string) {
	t.Helper()
	var unavailable []error
	for _, network := range networks {
		var err error
		switch network {
		case "tcp6":
			err = echIPv6TCPProbe()
		case "udp6":
			err = echIPv6UDPProbe()
		default:
			t.Fatalf("unexpected IPv6 probe network: %s", network)
		}
		if err != nil {
			unavailable = append(unavailable, fmt.Errorf("%s: %w", network, err))
		}
	}
	if len(unavailable) != 0 {
		message := fmt.Sprintf("IPv6 loopback unavailable before ECH on %s: %v", runtime.GOOS, errors.Join(unavailable...))
		if os.Getenv("REMNACUST_REQUIRE_IPV6_TESTS") == "1" {
			t.Fatal(message)
		}
		t.Skip(message)
	}
}

func echIPv6UDPDNSServer(t *testing.T, address string, handler dns.HandlerFunc) string {
	t.Helper()
	packet, err := net.ListenPacket("udp6", address)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	server := &dns.Server{PacketConn: packet, Handler: handler, NotifyStartedFunc: func() { close(started) }}
	go func() {
		if err := server.ActivateAndServe(); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.ShutdownContext(ctx); err != nil {
			t.Error(err)
		}
	})
	<-started
	return "udp://" + packet.LocalAddr().String()
}

func TestECHIPv6DNSLargeUDP(t *testing.T) {
	echRequireIPv6Loopback(t, "udp6")
	const domain = "public.example.test"
	expected := echDNSConfig(t, 8)
	var queries atomic.Int64
	server := echIPv6UDPDNSServer(t, "[::1]:0", func(w dns.ResponseWriter, query *dns.Msg) {
		queries.Add(1)
		if opt := query.IsEdns0(); opt == nil || opt.UDPSize() != 1232 {
			t.Errorf("IPv6 ECH query did not advertise EDNS 1232: %v", opt)
		}
		reply := echDNSPack(t, echDNSReply(query, expected))
		if len(reply) <= 512 || len(reply) > 1232 {
			t.Errorf("invalid large UDP test message size: %d", len(reply))
		}
		if _, err := w.Write(reply); err != nil {
			t.Error(err)
		}
	})
	sockopt := &internet.SocketConfig{}
	echClearDNSCache(t, server, domain, sockopt)
	var dials atomic.Int64
	var base internet.DefaultSystemDialer
	echUseSystemDialer(t, &echTestSystemDialer{dial: func(ctx context.Context, source xnet.Address, dest xnet.Destination, options *internet.SocketConfig) (net.Conn, error) {
		dials.Add(1)
		if dest.Network != xnet.Network_UDP || !dest.Address.Family().IsIPv6() || "udp://"+dest.NetAddr() != server || options != sockopt {
			t.Errorf("IPv6 UDP query changed resolver or socket settings: %v %p", dest, options)
			return nil, errors.New("unexpected IPv6 DNS destination")
		}
		return base.Dial(ctx, source, dest, options)
	}})
	for range 2 {
		actual, err := QueryRecord(domain, server, sockopt)
		if err != nil || !slices.Equal(actual, expected) {
			t.Fatalf("large ECH record over IPv6 UDP failed: length=%d, error=%v", len(actual), err)
		}
	}
	if queries.Load() != 1 || dials.Load() != 1 {
		t.Fatalf("IPv6 DNS cache did not reuse the valid record: queries=%d, dials=%d", queries.Load(), dials.Load())
	}
}

func TestECHIPv6DNSTCPFallback(t *testing.T) {
	echRequireIPv6Loopback(t, "tcp6", "udp6")
	const domain = "public.example.test"
	expected := echDNSConfig(t, 20)
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	var original atomic.Pointer[dns.Msg]
	var tcpQueries atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				t.Error(err)
			}
			return
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(7 * time.Second)); err != nil {
			t.Error(err)
			return
		}
		query := echReadTCPQuery(t, conn)
		if query == nil {
			return
		}
		tcpQueries.Add(1)
		udp := original.Load()
		if udp == nil || query.Id != udp.Id || !slices.Equal(query.Question, udp.Question) {
			t.Error("IPv6 TCP retry changed the DNS question or ID")
			return
		}
		data := echDNSPack(t, echDNSReply(query, expected))
		if len(data) <= 1232 {
			t.Errorf("TCP response should exceed the advertised UDP size, got %d", len(data))
		}
		frame := append(binary.BigEndian.AppendUint16(nil, uint16(len(data))), data...)
		for _, fragment := range [][]byte{frame[:1], frame[1:2], frame[2:23], frame[23:]} {
			if _, err := conn.Write(fragment); err != nil {
				t.Error(err)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			t.Error("IPv6 DNS TCP handler did not stop")
		}
	})
	server := echIPv6UDPDNSServer(t, listener.Addr().String(), func(w dns.ResponseWriter, query *dns.Msg) {
		original.Store(query)
		reply := new(dns.Msg).SetReply(query)
		reply.Truncated = true
		if err := w.WriteMsg(reply); err != nil {
			t.Error(err)
		}
	})
	sockopt := &internet.SocketConfig{}
	echClearDNSCache(t, server, domain, sockopt)
	var networks []xnet.Network
	var deadlines []time.Time
	var base internet.DefaultSystemDialer
	echUseSystemDialer(t, &echTestSystemDialer{dial: func(ctx context.Context, source xnet.Address, dest xnet.Destination, options *internet.SocketConfig) (net.Conn, error) {
		if !dest.Address.Family().IsIPv6() || dest.NetAddr() != listener.Addr().String() || options != sockopt {
			t.Errorf("IPv6 retry changed resolver/port/socket settings: %v %p", dest, options)
			return nil, errors.New("unexpected IPv6 DNS destination")
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("IPv6 DNS dial has no deadline")
		}
		networks = append(networks, dest.Network)
		deadlines = append(deadlines, deadline)
		return base.Dial(ctx, source, dest, options)
	}})
	actual, err := QueryRecord(domain, server, sockopt)
	if err != nil || !slices.Equal(actual, expected) {
		t.Fatalf("IPv6 TC to TCP failed: length=%d, error=%v", len(actual), err)
	}
	if !slices.Equal(networks, []xnet.Network{xnet.Network_UDP, xnet.Network_TCP}) || len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) || tcpQueries.Load() != 1 {
		t.Fatalf("IPv6 fallback did not preserve its overall timeout and routing: %v %v, queries=%d", networks, deadlines, tcpQueries.Load())
	}
}

func TestECHIPv6DNSDefaultPort(t *testing.T) {
	stop := errors.New("stop after parsing the configured DNS destination")
	for _, test := range []struct {
		server string
		want   string
	}{
		{"udp://[::1]", "[::1]:53"},
		{"udp://[::1]:", "[::1]:53"},
		{"udp://[::1]:5353", "[::1]:5353"},
		{"udp://[0:0:0:0:0:0:0:1]", "[::1]:53"},
	} {
		t.Run(test.server, func(t *testing.T) {
			var calls int
			sockopt := &internet.SocketConfig{}
			echUseSystemDialer(t, &echTestSystemDialer{dial: func(_ context.Context, _ xnet.Address, dest xnet.Destination, options *internet.SocketConfig) (net.Conn, error) {
				calls++
				if dest.Network != xnet.Network_UDP || !dest.Address.Family().IsIPv6() || dest.NetAddr() != test.want || options != sockopt {
					t.Errorf("incorrect IPv6 DNS destination: %v %p, want %s", dest, options, test.want)
				}
				return nil, stop
			}})
			if _, _, err := dnsQuery(test.server, "public.example.test", sockopt); !errors.Is(err, stop) || calls != 1 {
				t.Fatalf("IPv6 DNS endpoint did not normalize before dialing: calls=%d, error=%v", calls, err)
			}
		})
	}
}

func TestECHIPv6DNSRejectUnbracketedEndpoint(t *testing.T) {
	for _, server := range []string{"udp://::1", "udp://::1:", "udp://::"} {
		t.Run(server, func(t *testing.T) {
			var calls int
			echUseSystemDialer(t, &echTestSystemDialer{dial: func(context.Context, xnet.Address, xnet.Destination, *internet.SocketConfig) (net.Conn, error) {
				calls++
				return nil, errors.New("invalid IPv6 endpoint reached dialer")
			}})
			if _, _, err := dnsQuery(server, "public.example.test", nil); err == nil || calls != 0 {
				t.Fatalf("unbracketed IPv6 DNS endpoint was normalized into a resolver: calls=%d, error=%v", calls, err)
			}
		})
	}
}

func TestECHIPv6DoHAddressAndTLSRouting(t *testing.T) {
	stop := errors.New("stop before any DoH socket is opened")
	for _, scheme := range []string{"https", "h2c"} {
		for _, port := range []string{"", ":8443"} {
			server := scheme + "://[::1]" + port + "/dns-query"
			t.Run(server, func(t *testing.T) {
				wantPort := port
				if wantPort == "" {
					wantPort = ":443"
				}
				sockopt := &internet.SocketConfig{}
				var calls atomic.Int64
				echUseSystemDialer(t, &echTestSystemDialer{dial: func(ctx context.Context, _ xnet.Address, dest xnet.Destination, options *internet.SocketConfig) (net.Conn, error) {
					calls.Add(1)
					if dest.Network != xnet.Network_TCP || !dest.Address.Family().IsIPv6() || dest.NetAddr() != "[::1]"+wantPort || options != sockopt {
						t.Errorf("DoH lost IPv6 endpoint brackets/port/socket settings: %v %p", dest, options)
					}
					name := session.MitmServerNameFromContext(ctx)
					if (scheme == "h2c" && name != "::1") || (scheme == "https" && name != "") || session.MitmAlpn11FromContext(ctx) {
						t.Errorf("incorrect raw TLS routing name for %s: %q", scheme, name)
					}
					return nil, stop
				}})
				echClearDoHClient(t, server, sockopt)
				if _, _, err := dnsQuery(server, "public.example.test", sockopt); !errors.Is(err, stop) || calls.Load() != 1 {
					t.Fatalf("DoH IPv6 endpoint changed or retried: calls=%d, error=%v", calls.Load(), err)
				}
			})
		}
	}
}

func TestECHIPv6DoHTLSLoopback(t *testing.T) {
	for _, scheme := range []string{"h2c", "https"} {
		t.Run(scheme, func(t *testing.T) {
			echRequireIPv6Loopback(t, "tcp6")
			listener, err := net.Listen("tcp6", "[::1]:0")
			if err != nil {
				t.Fatal(err)
			}
			expected := echDNSConfig(t, 8)
			var hellos, requests atomic.Int64
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if scheme != "h2c" {
					t.Error("untrusted IPv6 DoH server received an HTTP request")
					return
				}
				if r.TLS == nil || r.ProtoMajor != 2 || r.Host != listener.Addr().String() || r.Method != http.MethodPost {
					t.Error("IPv6 DoH did not preserve HTTP/2 over routed TLS and bracketed authority")
				}
				data, err := io.ReadAll(io.LimitReader(r.Body, dns.MaxMsgSize+1))
				if err != nil {
					t.Error(err)
					return
				}
				query := new(dns.Msg)
				if err := query.Unpack(data); err != nil || query.Id != 0 || len(query.Question) != 1 {
					t.Errorf("invalid IPv6 DoH query: %v", err)
					return
				}
				w.Header().Set("Content-Type", "application/dns-message")
				if _, err := w.Write(echDNSPack(t, echDNSReply(query, expected))); err != nil {
					t.Error(err)
				}
			}))
			server.Listener.Close()
			server.Listener = listener
			server.EnableHTTP2 = true
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			server.TLS = &gotls.Config{GetConfigForClient: func(hello *gotls.ClientHelloInfo) (*gotls.Config, error) {
				hellos.Add(1)
				if hello.ServerName != "" {
					t.Errorf("IPv6 literal appeared in the TLS SNI extension: %q", hello.ServerName)
				}
				return nil, nil
			}}
			server.StartTLS()
			t.Cleanup(server.Close)
			if err := server.Certificate().VerifyHostname("::1"); err != nil {
				t.Fatalf("IPv6 TLS fixture lacks the loopback IP SAN: %v", err)
			}
			pool := x509.NewCertPool()
			pool.AddCert(server.Certificate())
			sockopt := &internet.SocketConfig{}
			var base internet.DefaultSystemDialer
			var opened, closed atomic.Int64
			echUseSystemDialer(t, &echTestSystemDialer{dial: func(ctx context.Context, source xnet.Address, dest xnet.Destination, options *internet.SocketConfig) (net.Conn, error) {
				if dest.Network != xnet.Network_TCP || !dest.Address.Family().IsIPv6() || dest.NetAddr() != listener.Addr().String() || options != sockopt {
					t.Errorf("DoH changed the configured IPv6 resolver/socket settings: %v %p", dest, options)
					return nil, errors.New("unexpected IPv6 DoH destination")
				}
				name := session.MitmServerNameFromContext(ctx)
				if (scheme == "h2c" && name != "::1") || (scheme == "https" && name != "") || strings.ContainsAny(name, "[]") {
					t.Errorf("IPv6 endpoint brackets leaked into the TLS routing name: %q", name)
				}
				conn, err := base.Dial(ctx, source, dest, options)
				if err != nil {
					return nil, err
				}
				t.Cleanup(func() { conn.Close() })
				if scheme == "h2c" {
					client := gotls.Client(conn, &gotls.Config{ServerName: name, RootCAs: pool, NextProtos: []string{"h2"}})
					if err := client.HandshakeContext(ctx); err != nil {
						client.Close()
						return nil, err
					}
					return client, nil
				}
				opened.Add(1)
				return &echTrackedConn{Conn: conn, closed: &closed}, nil
			}})
			url := scheme + "://" + listener.Addr().String() + "/dns-query"
			echClearDoHClient(t, url, sockopt)
			actual, _, err := dnsQuery(url, "public.example.test", sockopt)
			if scheme == "h2c" {
				if err != nil || !slices.Equal(actual, expected) || requests.Load() != 1 {
					t.Fatalf("IPv6 routed DoH TLS failed: length=%d, requests=%d, error=%v", len(actual), requests.Load(), err)
				}
			} else {
				var untrusted x509.UnknownAuthorityError
				if !errors.As(err, &untrusted) || requests.Load() != 0 || opened.Load() == 0 || closed.Load() < opened.Load() {
					t.Fatalf("IPv6 DoH did not preserve certificate verification/cleanup: opened=%d, closed=%d, requests=%d, error=%v", opened.Load(), closed.Load(), requests.Load(), err)
				}
			}
			if hellos.Load() != 1 {
				t.Errorf("expected one IPv6 TLS ClientHello, got %d", hellos.Load())
			}
		})
	}
}
