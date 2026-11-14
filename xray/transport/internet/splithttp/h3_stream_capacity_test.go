package splithttp

import (
	"bytes"
	"context"
	stdtls "crypto/tls"
	"crypto/x509"
	stderrors "errors"
	"fmt"
	"io"
	stdnet "net"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/http3"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
	"github.com/xtls/xray-core/testing/servers/udp"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/internet/tls"
)

type h3CapacityFixture struct {
	config     *Config
	listener   *Listener
	connection *quic.Conn
	client     *http3.ClientConn
	active     atomic.Int64
}

func requireH3CapacityIPv6(t *testing.T) {
	t.Helper()
	listener, err := stdnet.Listen("tcp6", "[::1]:0")
	if err == nil {
		defer listener.Close()
		var probe stdnet.Conn
		probe, err = stdnet.DialTimeout("tcp6", listener.Addr().String(), time.Second)
		if err == nil {
			probe.Close()
			return
		}
	}
	if os.Getenv("REMNACUST_REQUIRE_IPV6_TESTS") == "1" {
		t.Fatalf("required IPv6 loopback unavailable before QUIC: %v", err)
	}
	t.Skipf("IPv6 loopback unavailable before QUIC: %v", err)
}

func newH3CapacityFixture(t *testing.T, address net.Address, config *Config, params *internet.QuicParams) *h3CapacityFixture {
	t.Helper()
	certificate, _ := cert.MustGenerate(nil, cert.CommonName("localhost"), cert.DNSNames("localhost"))
	certificatePEM, _ := certificate.ToPEM()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificatePEM) {
		t.Fatal("could not trust fixture certificate")
	}
	settings := &internet.MemoryStreamConfig{
		ProtocolName: protocolName, ProtocolSettings: config, SecurityType: "tls",
		SecuritySettings: &tls.Config{
			Certificate:  []*tls.Certificate{tls.ParseCertificate(certificate)},
			NextProtocol: []string{"h3"}, ServerName: "localhost",
		},
		QuicParams: params,
	}
	t.Cleanup(func() { settings.Close() })
	fixture := &h3CapacityFixture{config: config}
	listener, err := ListenXH(context.Background(), address, udp.PickPort(), settings, func(connection stat.Connection) {
		fixture.active.Add(1)
		go func() {
			defer fixture.active.Add(-1)
			defer connection.Close()
			io.Copy(connection, connection)
		}()
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.listener = listener.(*Listener)
	t.Cleanup(func() { listener.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	fixture.connection, err = quic.DialAddr(ctx, listener.Addr().String(), &stdtls.Config{
		RootCAs: roots, ServerName: "localhost", NextProtos: []string{"h3"}, MinVersion: stdtls.VersionTLS13,
	}, &quic.Config{MaxIncomingStreams: -1, MaxIdleTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	transport := &http3.Transport{MaxResponseHeaderBytes: 8192}
	fixture.client = transport.NewClientConn(fixture.connection)
	t.Cleanup(func() {
		fixture.connection.CloseWithError(0, "capacity fixture complete")
		deadline := time.Now().Add(3 * time.Second)
		for {
			sessions := 0
			fixture.listener.h3server.Handler.(*requestHandler).sessions.Range(func(_, _ any) bool { sessions++; return true })
			if fixture.active.Load() == 0 && sessions == 0 {

				return
			}
			if time.Now().After(deadline) {
				t.Errorf("HTTP/3 sessions survived connection close: active=%d sessions=%d", fixture.active.Load(), sessions)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	return fixture
}

func (f *h3CapacityFixture) openDownlinks(t *testing.T, count int) []*http3.RequestStream {
	t.Helper()
	streams := make([]*http3.RequestStream, 0, count)
	for index := range count {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		stream, err := f.client.OpenRequestStream(ctx)
		cancel()
		if err != nil {
			t.Fatalf("downlink %d/%d on a single QUIC connection: %v", index+1, count, err)
		}
		streams = append(streams, stream)
		t.Cleanup(func() {
			stream.CancelRead(quic.StreamErrorCode(http3.ErrCodeRequestCanceled))
			stream.CancelWrite(quic.StreamErrorCode(http3.ErrCodeRequestCanceled))
		})
		request, err := http.NewRequest(http.MethodGet, "https://localhost"+f.config.GetNormalizedPath(), nil)
		if err != nil {
			t.Fatal(err)
		}
		f.config.FillStreamRequest(request, fmt.Sprintf("capacity-%03d", index), "")
		if err := stream.SendRequestHeader(request); err != nil {
			t.Fatal(err)
		}
		if err := stream.Close(); err != nil {
			t.Fatal(err)
		}
		stream.SetReadDeadline(time.Now().Add(2 * time.Second))
		response, err := stream.ReadResponse()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK || response.ProtoMajor != 3 {
			t.Fatalf("downlink %d returned %s HTTP %d", index, response.Proto, response.StatusCode)
		}
		stream.SetReadDeadline(time.Time{})
	}
	deadline := time.Now().Add(time.Second)
	for f.active.Load() != int64(count) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d downlinks remained active", f.active.Load(), count)
		}
		time.Sleep(time.Millisecond)
	}
	return streams
}

func (f *h3CapacityFixture) upload(sequence int, payload []byte, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://localhost"+f.config.GetNormalizedPath(), nil)
	if err != nil {
		return err
	}
	if err := f.config.FillPacketRequest(request, "capacity-000", strconv.Itoa(sequence), buf.MergeBytes(nil, payload)); err != nil {
		return err
	}
	response, err := f.client.RoundTrip(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK || response.ProtoMajor != 3 {
		return fmt.Errorf("packet upload returned %s HTTP %d", response.Proto, response.StatusCode)
	}
	return nil
}

func assertH3CapacityEcho(t *testing.T, stream *http3.RequestStream, payload []byte) {
	t.Helper()
	stream.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("packet upload echo = %q, want %q", got, payload)
	}
	stream.SetReadDeadline(time.Time{})
}

func TestH3StreamCapacityAdvertisedBudget(t *testing.T) {
	fixture := newH3CapacityFixture(t, net.LocalHostIP, &Config{Path: "/h3-capacity/", Mode: "packet-up"}, nil)
	const want = 1024
	for index := range want {
		if _, err := fixture.connection.OpenStream(); err != nil {
			t.Fatalf("advertised stream budget ended at %d, want %d: %v", index, want, err)
		}
	}
	stream, err := fixture.connection.OpenStream()
	var limited *quic.StreamLimitReachedError
	if stream != nil || !stderrors.As(err, &limited) {
		t.Fatalf("advertised stream budget exceeds %d: %v", want, err)
	}
}

func TestH3StreamCapacityPacketUp(t *testing.T) {
	for _, address := range []net.Address{net.LocalHostIP, net.LocalHostIPv6} {
		for _, explicitZero := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/explicit-zero=%t", address, explicitZero), func(t *testing.T) {
				if address.Family().IsIPv6() {
					requireH3CapacityIPv6(t)
				}
				config := &Config{Path: "/h3-capacity/", Mode: "packet-up"}
				var params *internet.QuicParams
				if explicitZero {
					params = &internet.QuicParams{Congestion: "reno"}
				}
				fixture := newH3CapacityFixture(t, address, config, params)
				streams := fixture.openDownlinks(t, 110)
				for sequence := range 2 {
					payload := []byte(fmt.Sprintf("upload-after-110-downlinks-%d", sequence))
					if err := fixture.upload(sequence, payload, 3*time.Second); err != nil {
						t.Fatal(err)
					}
					assertH3CapacityEcho(t, streams[0], payload)
				}
				if fixture.active.Load() != 110 {
					t.Fatalf("uploads released downlinks: %d remain", fixture.active.Load())
				}
				t.Logf("110 live downlinks and packet uploads progressed on QUIC %s -> %s", fixture.connection.LocalAddr(), fixture.connection.RemoteAddr())
				if err := fixture.connection.CloseWithError(0, "completed 110 live downlinks"); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestH3StreamCapacityPreservesExplicitLimit(t *testing.T) {
	fixture := newH3CapacityFixture(t, net.LocalHostIP, &Config{Path: "/h3-capacity/", Mode: "packet-up"}, &internet.QuicParams{MaxIncomingStreams: 3, Congestion: "reno"})
	streams := fixture.openDownlinks(t, 3)
	if err := fixture.upload(0, []byte("blocked"), 150*time.Millisecond); !stderrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("explicit stream limit was not preserved: %v", err)
	}
	streams[2].CancelRead(quic.StreamErrorCode(http3.ErrCodeRequestCanceled))
	payload := []byte("upload-after-slot-release")
	if err := fixture.upload(0, payload, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	assertH3CapacityEcho(t, streams[0], payload)
}

func TestH3StreamCapacityPreservesDisabledStreams(t *testing.T) {
	fixture := newH3CapacityFixture(t, net.LocalHostIP, &Config{Path: "/h3-capacity/", Mode: "packet-up"}, &internet.QuicParams{MaxIncomingStreams: -1, Congestion: "reno"})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	stream, err := fixture.client.OpenRequestStream(ctx)
	if stream != nil {
		stream.CancelRead(0)
		stream.CancelWrite(0)
		t.Fatal("negative explicit stream limit admitted a request")
	}
	if !stderrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("disabled request stream: %v", err)
	}
}
