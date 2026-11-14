package tls

import (
	"bytes"
	"context"
	gotls "crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/miekg/dns"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/transport/internet"
)

func echClearDoHClient(t *testing.T, server string, sockopt *internet.SocketConfig) {
	t.Helper()
	key := ECHCacheKey(server, "", sockopt)
	t.Cleanup(func() {
		if client, ok := clientForECHDOH.Load(key); ok {
			client.CloseIdleConnections()
		}
		clientForECHDOH.Delete(key)
	})
}

func echStartRoutedDoH(t *testing.T, handler http.HandlerFunc) (string, *internet.SocketConfig) {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	sockopt := &internet.SocketConfig{}
	echUseSystemDialer(t, &echTestSystemDialer{dial: func(ctx context.Context, _ xnet.Address, dest xnet.Destination, options *internet.SocketConfig) (net.Conn, error) {
		if dest.Network != xnet.Network_TCP || dest.NetAddr() != server.Listener.Addr().String() || options != sockopt {
			t.Errorf("DoH changed configured resolver or socket settings: %v %p", dest, options)
			return nil, errors.New("unexpected DoH destination")
		}
		if session.MitmServerNameFromContext(ctx) != "127.0.0.1" || session.MitmAlpn11FromContext(ctx) {
			t.Error("h2c did not preserve the outbound TLS routing hints")
		}
		dialer := &gotls.Dialer{Config: &gotls.Config{RootCAs: pool, NextProtos: []string{"h2"}}}
		return dialer.DialContext(ctx, "tcp", dest.NetAddr())
	}})
	url := "h2c://" + server.Listener.Addr().String() + "/dns-query"
	echClearDoHClient(t, url, sockopt)
	return url, sockopt
}

func echPaddedDNSReply(t *testing.T, reply *dns.Msg, size int) []byte {
	t.Helper()
	if size == 0 {
		return echDNSPack(t, reply)
	}
	reply.SetEdns0(4096, false)
	padding := &dns.EDNS0_PADDING{}
	reply.IsEdns0().Option = append(reply.IsEdns0().Option, padding)
	padding.Padding = make([]byte, dns.MaxMsgSize-len(echDNSPack(t, reply)))
	data := echDNSPack(t, reply)
	if len(data) != dns.MaxMsgSize {
		t.Errorf("expected a maximum-size DNS message, got %d", len(data))
	}
	return append(data, make([]byte, size-len(data))...)
}

func TestECHDoHLoopbackResponses(t *testing.T) {
	const domain = "public.example.test"
	expected := echDNSConfig(t, 1)
	tests := []struct {
		name    string
		size    int
		known   bool
		mutate  func(*dns.Msg)
		wantErr bool
	}{
		{name: "small"},
		{name: "maximum", size: dns.MaxMsgSize, known: true},
		{name: "oversized-known", size: dns.MaxMsgSize + 1, known: true, wantErr: true},
		{name: "oversized-stream", size: dns.MaxMsgSize + 1, wantErr: true},
		{name: "tc", mutate: func(r *dns.Msg) { r.Truncated = true }, wantErr: true},
		{name: "case", mutate: func(r *dns.Msg) {
			r.Question[0].Name = strings.ToUpper(domain) + "."
			r.Answer[0].Header().Name = strings.ToUpper(domain) + "."
		}},
	}
	for _, invalid := range echInvalidDNSReplies() {
		tests = append(tests, struct {
			name    string
			size    int
			known   bool
			mutate  func(*dns.Msg)
			wantErr bool
		}{name: "invalid-" + invalid.name, mutate: invalid.mutate, wantErr: true})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var queries atomic.Int64
			server, sockopt := echStartRoutedDoH(t, func(w http.ResponseWriter, r *http.Request) {
				queries.Add(1)
				if r.TLS == nil || r.ProtoMajor != 2 || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/dns-message" || r.Header.Get("Accept") != "application/dns-message" {
					t.Error("DoH request did not use routed TLS, HTTP/2 and DNS-message POST")
				}
				data, err := io.ReadAll(io.LimitReader(r.Body, dns.MaxMsgSize+1))
				if err != nil {
					t.Error(err)
					return
				}
				query := new(dns.Msg)
				if err := query.Unpack(data); err != nil || query.Id != 0 || len(query.Question) != 1 {
					t.Errorf("invalid DoH query: %v", err)
					return
				}
				reply := echDNSReply(query, expected)
				if test.mutate != nil {
					test.mutate(reply)
				}
				body := echPaddedDNSReply(t, reply, test.size)
				w.Header().Set("Content-Type", "application/dns-message")
				if test.known {
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				}
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				if _, err := w.Write(body); err != nil && !test.wantErr {
					t.Error(err)
				}
			})
			echClearDNSCache(t, server, domain, sockopt)
			actual, err := QueryRecord(domain, server, sockopt)
			if test.wantErr {
				if err == nil || len(actual) != 0 {
					t.Fatalf("invalid DoH response accepted: length=%d, error=%v", len(actual), err)
				}
				if cached, ok := GlobalECHConfigCache.Load(ECHCacheKey(server, domain, sockopt)); ok && len(cached.configRecord.Load().config) != 0 {
					t.Fatal("invalid DoH response populated the ECH cache")
				}
			} else if err != nil || !slices.Equal(actual, expected) {
				t.Fatalf("valid DoH response rejected: length=%d, error=%v", len(actual), err)
			}
			if queries.Load() != 1 {
				t.Errorf("unexpected DoH query/fallback count: %d", queries.Load())
			}
		})
	}
}

type echTestRoundTripper func(*http.Request) (*http.Response, error)

func (f echTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type echCountedBody struct {
	io.Reader
	read   int
	closed bool
}

func (b *echCountedBody) Read(data []byte) (int, error) {
	n, err := b.Reader.Read(data)
	b.read += n
	return n, err
}

func (b *echCountedBody) Close() error { b.closed = true; return nil }

func TestECHDoHReadBound(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadGateway} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			const server = "https://127.0.0.1:1/dns-query"
			body := &echCountedBody{Reader: bytes.NewReader(make([]byte, 4*dns.MaxMsgSize))}
			client := &http.Client{Transport: echTestRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Body: body, ContentLength: -1, Header: make(http.Header)}, nil
			})}
			clientForECHDOH.Store(ECHCacheKey(server, "", nil), client)
			echClearDoHClient(t, server, nil)
			if _, _, err := dnsQuery(server, "public.example.test", nil); err == nil {
				t.Fatal("oversized/error DoH response was accepted")
			}
			if body.read > dns.MaxMsgSize+1 || !body.closed {
				t.Fatalf("DoH body was not bounded and closed: read=%d, closed=%t", body.read, body.closed)
			}
		})
	}
}

type echTrackedConn struct {
	net.Conn
	closed *atomic.Int64
}

func (c *echTrackedConn) Close() error {
	c.closed.Add(1)
	return c.Conn.Close()
}

func TestECHDoHHandshakeFailureClosesConnection(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("untrusted DoH server received an HTTP request")
	}))
	server.EnableHTTP2 = true
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	var opened, closed atomic.Int64
	var base internet.DefaultSystemDialer
	echUseSystemDialer(t, &echTestSystemDialer{dial: func(ctx context.Context, source xnet.Address, dest xnet.Destination, sockopt *internet.SocketConfig) (net.Conn, error) {
		if dest.Network != xnet.Network_TCP || dest.NetAddr() != server.Listener.Addr().String() {
			t.Errorf("DoH failed over to an unconfigured/plaintext resolver: %v", dest)
			return nil, errors.New("unexpected fallback")
		}
		conn, err := base.Dial(ctx, source, dest, sockopt)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { conn.Close() })
		opened.Add(1)
		return &echTrackedConn{Conn: conn, closed: &closed}, nil
	}})
	echClearDoHClient(t, server.URL, nil)
	_, _, err := dnsQuery(server.URL, "public.example.test", nil)
	var untrusted x509.UnknownAuthorityError
	if !errors.As(err, &untrusted) {
		t.Fatalf("DoH certificate verification was not preserved: %v", err)
	}
	if opened.Load() == 0 || closed.Load() < opened.Load() {
		t.Fatalf("failed TLS handshake leaked its raw connection: opened=%d, closed=%d", opened.Load(), closed.Load())
	}
}
