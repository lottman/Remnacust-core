package tls

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	gotls "crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
	"golang.org/x/crypto/cryptobyte"
)

func echDNSConfig(t *testing.T, count int) []byte {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var b cryptobyte.Builder
	b.AddUint16(0xfe0d)
	b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
		b.AddUint8(1)
		b.AddUint16(0x20)
		b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) { b.AddBytes(key.PublicKey().Bytes()) })
		b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) { b.AddUint16(1); b.AddUint16(1) })
		b.AddUint8(0)
		b.AddUint8LengthPrefixed(func(b *cryptobyte.Builder) { b.AddBytes([]byte("public.example.test")) })
		b.AddUint16(0)
	})
	config, err := b.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	configs := bytes.Repeat(config, count)
	return append(binary.BigEndian.AppendUint16(nil, uint16(len(configs))), configs...)
}

func echDNSReply(query *dns.Msg, config []byte) *dns.Msg {
	reply := new(dns.Msg).SetReply(query)
	reply.Answer = []dns.RR{&dns.HTTPS{SVCB: dns.SVCB{
		Hdr:      dns.RR_Header{Name: query.Question[0].Name, Rrtype: dns.TypeHTTPS, Class: dns.ClassINET, Ttl: 60},
		Priority: 1, Target: ".", Value: []dns.SVCBKeyValue{&dns.SVCBECHConfig{ECH: config}},
	}}}
	return reply
}

func echDNSPack(t *testing.T, reply *dns.Msg) []byte {
	t.Helper()
	data, err := reply.Pack()
	if err != nil {
		t.Error(err)
	}
	return data
}

func echStartUDPDNS(t *testing.T, address string, handler dns.HandlerFunc) string {
	t.Helper()
	packet, err := net.ListenPacket("udp", address)
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

func echStartTCPDNS(t *testing.T, handler func(net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
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
		handler(conn)
	}()
	t.Cleanup(func() {
		listener.Close()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			t.Error("DNS TCP handler did not stop")
		}
	})
	return listener.Addr().String()
}

func echReadTCPQuery(t *testing.T, conn net.Conn) *dns.Msg {
	t.Helper()
	var length [2]byte
	if _, err := io.ReadFull(conn, length[:]); err != nil {
		t.Error(err)
		return nil
	}
	data := make([]byte, int(binary.BigEndian.Uint16(length[:])))
	if _, err := io.ReadFull(conn, data); err != nil {
		t.Error(err)
		return nil
	}
	query := new(dns.Msg)
	if err := query.Unpack(data); err != nil {
		t.Error(err)
		return nil
	}
	return query
}

type echTestSystemDialer struct {
	dial func(context.Context, xnet.Address, xnet.Destination, *internet.SocketConfig) (net.Conn, error)
}

func (d *echTestSystemDialer) Dial(ctx context.Context, source xnet.Address, dest xnet.Destination, sockopt *internet.SocketConfig) (net.Conn, error) {
	return d.dial(ctx, source, dest, sockopt)
}

func (*echTestSystemDialer) DestIpAddress() xnet.IP { return nil }

func echUseSystemDialer(t *testing.T, dialer internet.SystemDialer) {
	t.Helper()
	internet.UseAlternativeSystemDialer(dialer)
	t.Cleanup(func() { internet.UseAlternativeSystemDialer(nil) })
}

func echClearDNSCache(t *testing.T, server, domain string, sockopt *internet.SocketConfig) {
	t.Helper()
	key := ECHCacheKey(server, domain, sockopt)
	t.Cleanup(func() { GlobalECHConfigCache.Delete(key) })
}

func TestECHDNSUDPSizeAndCase(t *testing.T) {
	const domain = "public.example.test"
	expected := echDNSConfig(t, 8)
	var queries atomic.Int64
	server := echStartUDPDNS(t, "127.0.0.1:0", func(w dns.ResponseWriter, query *dns.Msg) {
		queries.Add(1)
		if opt := query.IsEdns0(); opt == nil || opt.UDPSize() != 1232 {
			t.Errorf("UDP ECH query must advertise 1232 bytes: %v", opt)
		}
		reply := echDNSReply(query, expected)
		reply.Question[0].Name = strings.ToUpper(domain) + "."
		reply.Answer[0].Header().Name = strings.ToUpper(domain) + "."
		if _, err := w.Write(echDNSPack(t, reply)); err != nil {
			t.Error(err)
		}
	})
	echClearDNSCache(t, server, domain, nil)
	for range 2 {
		actual, err := QueryRecord(domain, server, nil)
		if err != nil || !slices.Equal(actual, expected) {
			t.Fatalf("large mixed-case ECH response: config length=%d, error=%v", len(actual), err)
		}
	}
	if queries.Load() != 1 {
		t.Errorf("valid response was not cached: queries=%d", queries.Load())
	}
}

func echInvalidDNSReplies() []struct {
	name   string
	mutate func(*dns.Msg)
} {
	return []struct {
		name   string
		mutate func(*dns.Msg)
	}{
		{"id", func(r *dns.Msg) { r.Id ^= 1 }},
		{"qr", func(r *dns.Msg) { r.Response = false }},
		{"opcode", func(r *dns.Msg) { r.Opcode = dns.OpcodeNotify }},
		{"rcode", func(r *dns.Msg) { r.Rcode = dns.RcodeServerFailure }},
		{"question-name", func(r *dns.Msg) { r.Question[0].Name = "unrelated.example.test." }},
		{"question-type", func(r *dns.Msg) { r.Question[0].Qtype = dns.TypeA }},
		{"question-class", func(r *dns.Msg) { r.Question[0].Qclass = dns.ClassCHAOS }},
		{"no-question", func(r *dns.Msg) { r.Question = nil }},
		{"extra-question", func(r *dns.Msg) { r.Question = append(r.Question, r.Question[0]) }},
		{"owner", func(r *dns.Msg) { r.Answer[0].Header().Name = "unrelated.example.test." }},
		{"answer-class", func(r *dns.Msg) { r.Answer[0].Header().Class = dns.ClassCHAOS }},
		{"empty-ech", func(r *dns.Msg) { r.Answer[0].(*dns.HTTPS).Value[0].(*dns.SVCBECHConfig).ECH = nil }},
	}
}

func TestECHDNSRejectInvalidWithoutCaching(t *testing.T) {
	const domain = "public.example.test"
	expected := echDNSConfig(t, 1)
	for _, test := range echInvalidDNSReplies() {
		t.Run(test.name, func(t *testing.T) {
			var queries atomic.Int64
			server := echStartUDPDNS(t, "127.0.0.1:0", func(w dns.ResponseWriter, query *dns.Msg) {
				reply := echDNSReply(query, expected)
				if queries.Add(1) == 1 {
					test.mutate(reply)
				}
				if _, err := w.Write(echDNSPack(t, reply)); err != nil {
					t.Error(err)
				}
			})
			echClearDNSCache(t, server, domain, nil)
			config := &gotls.Config{ServerName: domain}
			if err := ApplyECH(&Config{EchConfigList: server}, config); err == nil {
				t.Fatal("invalid DNS response was accepted")
			}
			if !slices.Equal(config.EncryptedClientHelloConfigList, []byte{1, 1, 4, 5, 1, 4}) {
				t.Fatal("ApplyECH did not fail closed")
			}
			if cached, ok := GlobalECHConfigCache.Load(ECHCacheKey(server, domain, nil)); ok {
				record := cached.configRecord.Load()
				if len(record.config) != 0 || !record.expire.IsZero() {
					t.Fatal("invalid DNS response populated the cache")
				}
			}
			actual, err := QueryRecord(domain, server, nil)
			if err != nil || !slices.Equal(actual, expected) || queries.Load() != 2 {
				t.Fatalf("retry after invalid reply failed: queries=%d, error=%v", queries.Load(), err)
			}
		})
	}
}

func TestECHDNSTCPFallback(t *testing.T) {
	const domain = "public.example.test"
	expected := echDNSConfig(t, 20)
	for _, partial := range []bool{false, true} {
		name := "complete-tc"
		if partial {
			name = "partial-tc"
		}
		t.Run(name, func(t *testing.T) {
			var udpQuery atomic.Pointer[dns.Msg]
			address := echStartTCPDNS(t, func(conn net.Conn) {
				query := echReadTCPQuery(t, conn)
				if query == nil {
					return
				}
				original := udpQuery.Load()
				if original == nil || query.Id != original.Id || !slices.Equal(query.Question, original.Question) {
					t.Error("TCP retry changed the DNS query")
					return
				}
				data := echDNSPack(t, echDNSReply(query, expected))
				framed := append(binary.BigEndian.AppendUint16(nil, uint16(len(data))), data...)
				for _, chunk := range [][]byte{framed[:1], framed[1:2], framed[2:19], framed[19:]} {
					if _, err := conn.Write(chunk); err != nil {
						t.Error(err)
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			})
			server := echStartUDPDNS(t, address, func(w dns.ResponseWriter, query *dns.Msg) {
				udpQuery.Store(query)
				reply := echDNSReply(query, expected[:2])
				reply.Truncated = true
				data := echDNSPack(t, reply)
				if partial {
					data = data[:len(data)-1]
				}
				if _, err := w.Write(data); err != nil {
					t.Error(err)
				}
			})
			sockopt := &internet.SocketConfig{}
			echClearDNSCache(t, server, domain, sockopt)
			var calls []xnet.Destination
			var deadlines []time.Time
			var base internet.DefaultSystemDialer
			echUseSystemDialer(t, &echTestSystemDialer{dial: func(ctx context.Context, source xnet.Address, dest xnet.Destination, options *internet.SocketConfig) (net.Conn, error) {
				if options != sockopt || dest.NetAddr() != address {
					t.Errorf("retry changed resolver/socket settings: %v, %p", dest, options)
				}
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Error("DNS dial has no deadline")
				}
				calls = append(calls, dest)
				deadlines = append(deadlines, deadline)
				return base.Dial(ctx, source, dest, options)
			}})
			actual, err := QueryRecord(domain, server, sockopt)
			if err != nil || !slices.Equal(actual, expected) {
				t.Fatalf("TCP fallback failed: config length=%d, error=%v", len(actual), err)
			}
			if len(calls) != 2 || calls[0].Network != xnet.Network_UDP || calls[1].Network != xnet.Network_TCP || !deadlines[0].Equal(deadlines[1]) {
				t.Fatalf("fallback did not preserve overall timeout and transport routing: %v %v", calls, deadlines)
			}
		})
	}
}

func TestECHDNSTCPRejectInvalid(t *testing.T) {
	expected := echDNSConfig(t, 1)
	tests := echInvalidDNSReplies()
	tests = append(tests, struct {
		name   string
		mutate func(*dns.Msg)
	}{"tc", func(r *dns.Msg) { r.Truncated = true }})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			address := echStartTCPDNS(t, func(conn net.Conn) {
				query := echReadTCPQuery(t, conn)
				if query == nil {
					return
				}
				reply := echDNSReply(query, expected)
				test.mutate(reply)
				data := echDNSPack(t, reply)
				if _, err := conn.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(data))), data...)); err != nil {
					t.Error(err)
				}
			})
			server := echStartUDPDNS(t, address, func(w dns.ResponseWriter, query *dns.Msg) {
				reply := new(dns.Msg).SetReply(query)
				reply.Truncated = true
				if err := w.WriteMsg(reply); err != nil {
					t.Error(err)
				}
			})
			echClearDNSCache(t, server, "public.example.test", nil)
			if config, err := QueryRecord("public.example.test", server, nil); err == nil || len(config) != 0 {
				t.Fatalf("invalid TCP response accepted: length=%d, error=%v", len(config), err)
			}
		})
	}
}

func TestECHDNSTCPIncompleteFrame(t *testing.T) {
	for _, test := range []struct {
		name string
		data []byte
	}{{"empty", nil}, {"short-prefix", []byte{0}}, {"zero-length", []byte{0, 0}}, {"short-body", []byte{0, 12, 0, 1, 2}}} {
		t.Run(test.name, func(t *testing.T) {
			var tcpQueries atomic.Int64
			address := echStartTCPDNS(t, func(conn net.Conn) {
				if echReadTCPQuery(t, conn) == nil {
					return
				}
				tcpQueries.Add(1)
				if _, err := conn.Write(test.data); err != nil {
					t.Error(err)
				}
			})
			server := echStartUDPDNS(t, address, func(w dns.ResponseWriter, query *dns.Msg) {
				reply := new(dns.Msg).SetReply(query)
				reply.Truncated = true
				if err := w.WriteMsg(reply); err != nil {
					t.Error(err)
				}
			})
			if config, _, err := dnsQuery(server, "public.example.test", nil); err == nil || len(config) != 0 {
				t.Fatalf("incomplete TCP frame accepted: length=%d, error=%v", len(config), err)
			}
			if tcpQueries.Load() != 1 {
				t.Fatal("TCP retry was not attempted")
			}
		})
	}
}

func TestECHDNSShortDatagram(t *testing.T) {
	expected := echDNSConfig(t, 1)
	server := echStartUDPDNS(t, "127.0.0.1:0", func(w dns.ResponseWriter, query *dns.Msg) {
		data := echDNSPack(t, echDNSReply(query, expected))
		if _, err := w.Write(data[:len(data)-1]); err != nil {
			t.Error(err)
		}
	})
	if config, _, err := dnsQuery(server, "public.example.test", nil); err == nil || len(config) != 0 {
		t.Fatalf("short UDP datagram was padded into a valid DNS answer: length=%d, error=%v", len(config), err)
	}
}

func TestECHDNSTCValidateBeforeRetry(t *testing.T) {
	expected := echDNSConfig(t, 1)
	for _, test := range echInvalidDNSReplies()[:9] {
		t.Run(test.name, func(t *testing.T) {
			server := echStartUDPDNS(t, "127.0.0.1:0", func(w dns.ResponseWriter, query *dns.Msg) {
				reply := echDNSReply(query, expected)
				reply.Truncated = true
				test.mutate(reply)
				if _, err := w.Write(echDNSPack(t, reply)); err != nil {
					t.Error(err)
				}
			})
			var calls int
			var base internet.DefaultSystemDialer
			echUseSystemDialer(t, &echTestSystemDialer{dial: func(ctx context.Context, source xnet.Address, dest xnet.Destination, options *internet.SocketConfig) (net.Conn, error) {
				calls++
				if dest.Network != xnet.Network_UDP {
					return nil, errors.New("invalid TC reply triggered TCP")
				}
				return base.Dial(ctx, source, dest, options)
			}})
			if _, _, err := dnsQuery(server, "public.example.test", nil); err == nil || calls != 1 {
				t.Fatalf("invalid TC reply accepted or retried: calls=%d, error=%v", calls, err)
			}
		})
	}
}

type echWriteFailureConn struct {
	net.Conn
	writeErr error
	closed   atomic.Bool
}

func (c *echWriteFailureConn) Write(data []byte) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return len(data) - 1, nil
}

func (*echWriteFailureConn) Read([]byte) (int, error) { return 0, io.EOF }

func (c *echWriteFailureConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

func TestECHDNSWriteFailures(t *testing.T) {
	writeErr := errors.New("ECH DNS test write failure")
	for _, network := range []xnet.Network{xnet.Network_UDP, xnet.Network_TCP} {
		for _, short := range []bool{false, true} {
			name := network.SystemString() + "-error"
			if short {
				name = network.SystemString() + "-short-write"
			}
			t.Run(name, func(t *testing.T) {
				raw, peer := net.Pipe()
				t.Cleanup(func() { peer.Close() })
				conn := &echWriteFailureConn{Conn: raw, writeErr: writeErr}
				wantErr := writeErr
				if short {
					conn.writeErr = nil
					wantErr = io.ErrShortWrite
				}
				t.Cleanup(func() { raw.Close() })
				server := echStartUDPDNS(t, "127.0.0.1:0", func(w dns.ResponseWriter, query *dns.Msg) {
					reply := new(dns.Msg).SetReply(query)
					reply.Truncated = true
					if err := w.WriteMsg(reply); err != nil {
						t.Error(err)
					}
				})
				var base internet.DefaultSystemDialer
				echUseSystemDialer(t, &echTestSystemDialer{dial: func(ctx context.Context, source xnet.Address, dest xnet.Destination, options *internet.SocketConfig) (net.Conn, error) {
					if dest.Network == network {
						return conn, nil
					}
					return base.Dial(ctx, source, dest, options)
				}})
				if _, _, err := dnsQuery(server, "public.example.test", nil); !errors.Is(err, wantErr) {
					t.Fatalf("write failure was hidden: got=%v, want=%v", err, wantErr)
				}
				if !conn.closed.Load() {
					t.Fatal("failed DNS connection was not closed")
				}
			})
		}
	}
}

type echNoDeadlineConn struct {
	net.Conn
	deadline time.Time
	closed   atomic.Int64
}

func (c *echNoDeadlineConn) SetDeadline(deadline time.Time) error {
	c.deadline = deadline
	return nil
}

func (c *echNoDeadlineConn) Close() error {
	c.closed.Add(1)
	return c.Conn.Close()
}

func TestECHDNSContextClosesConnectionWithoutDeadlines(t *testing.T) {
	raw, peer := net.Pipe()
	t.Cleanup(func() { raw.Close(); peer.Close() })
	conn := &echNoDeadlineConn{Conn: raw}
	echUseSystemDialer(t, &echTestSystemDialer{dial: func(context.Context, xnet.Address, xnet.Destination, *internet.SocketConfig) (net.Conn, error) {
		return conn, nil
	}})
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		io.Copy(io.Discard, peer)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	query := new(dns.Msg).SetQuestion("public.example.test.", dns.TypeHTTPS)
	data := echDNSPack(t, query)
	dest := xnet.TCPDestination(xnet.LocalHostIP, 53)
	result := make(chan error, 1)
	go func() {
		_, err := exchangeECHDNS(ctx, dest, data, nil)
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled DNS exchange succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("context deadline did not interrupt DNS I/O")
	}
	deadline, _ := ctx.Deadline()
	if !conn.deadline.Equal(deadline) || conn.closed.Load() != 1 {
		t.Fatalf("DNS deadline/cleanup mismatch: deadline=%v, closes=%d", conn.deadline, conn.closed.Load())
	}
	<-peerDone
}
