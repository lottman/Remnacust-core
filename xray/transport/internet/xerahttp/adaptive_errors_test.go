package xerahttp

import (
	"context"
	gotls "crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	stdnet "net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/apernet/quic-go"
	"github.com/gorilla/websocket"
	utls "github.com/refraction-networking/utls"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
	"github.com/xtls/xray-core/transport/internet"
	xtls "github.com/xtls/xray-core/transport/internet/tls"
)

func TestAdaptiveNativeTLSRejectionDoesNotShrink(t *testing.T) {
	good, hash := cert.MustGenerate(nil, cert.CommonName("localhost"), cert.DNSNames("localhost"))
	goodPEM, goodKey := good.ToPEM()
	goodPair, err := gotls.X509KeyPair(goodPEM, goodKey)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := cert.MustGenerate(nil, cert.CommonName("localhost"), cert.DNSNames("localhost"))
	otherPEM, otherKey := other.ToPEM()
	otherPair, err := gotls.X509KeyPair(otherPEM, otherKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{AdaptiveBalanced, AdaptiveLowLatency, AdaptiveUnstableNetwork} {
		for _, fingerprint := range []string{"golang", "chrome"} {
			t.Run(mode+"/"+fingerprint, func(t *testing.T) {
				var bad atomic.Bool
				var requests, handshakes atomic.Int32
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					w.Write([]byte("x"))
				}))
				server.Config.ErrorLog = log.New(io.Discard, "", 0)
				server.TLS = &gotls.Config{Certificates: []gotls.Certificate{goodPair}, NextProtos: []string{"http/1.1"}, SessionTicketsDisabled: true, GetCertificate: func(*gotls.ClientHelloInfo) (*gotls.Certificate, error) {
					handshakes.Add(1)
					if bad.Load() {
						return &otherPair, nil
					}
					return &goodPair, nil
				}}
				server.StartTLS()
				defer server.Close()
				dest := xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(server.Listener.Addr().(*stdnet.TCPAddr).Port))
				settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: &Config{Path: "/tls-rejection", Mode: ModePacketUp, Adaptive: &AdaptiveConfig{Mode: mode, MinStreams: 4, MaxStreams: 32}, Xmux: &XmuxConfig{CMaxReuseTimes: &RangeConfig{From: 1, To: 1}}}, SecurityType: "tls", SecuritySettings: &xtls.Config{ServerName: "localhost", PinnedPeerCertSha256: [][]byte{hash[:]}, NextProtocol: []string{"http/1.1"}, Fingerprint: fingerprint}}
				defer settings.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				first, err := Dial(ctx, dest, settings)
				if err != nil {
					adaptiveLoopbackError(t, err)
				}
				data, err := io.ReadAll(first)
				first.Close()
				if err != nil || string(data) != "x" {
					t.Fatalf("trusted certificate failed: %v", err)
				}
				globalDialerAccess.Lock()
				controller := globalDialerMap[dialerConf{dest, settings}].adaptive
				globalDialerAccess.Unlock()
				clock := new(adaptiveTestClock)
				controller.mu.Lock()
				clock.value.Store(controller.windowAt.UnixNano())
				controller.now = func() time.Time { return time.Unix(0, clock.value.Load()) }
				controller.mu.Unlock()
				bad.Store(true)
				beforeRequests, beforeHandshakes := requests.Load(), handshakes.Load()
				failed, rejectErr := Dial(ctx, dest, settings)
				if failed != nil {
					_, rejectErr = io.ReadAll(failed)
					failed.Close()
				}
				if rejectErr == nil {
					t.Fatal("untrusted certificate was accepted")
				}
				if requests.Load() != beforeRequests || handshakes.Load()-beforeHandshakes != 1 {
					t.Fatal("TLS rejection reached HTTP or was replayed")
				}
				controller.mu.Lock()
				failures, transient := controller.directions[0].window.failures, controller.directions[0].window.transient
				controller.mu.Unlock()
				adaptiveTick(controller, clock)
				limits, active, queued := adaptiveSnapshot(controller)
				if failures != 1 || transient != 0 || limits[0] != 8 || active != [2]int{} || queued != 0 {
					t.Fatalf("certificate rejection treated as congestion: failures=%d transient=%d limits=%v active=%v queued=%d", failures, transient, limits, active, queued)
				}
			})
		}
	}
}

func TestAdaptiveErrorClassification(t *testing.T) {
	wrapped := func(err error) error { return &url.Error{Op: "Post", URL: "https://example.test/", Err: err} }
	for _, test := range []struct {
		name       string
		err        error
		congestion bool
	}{
		{"certificate pin rejection", errors.New("peer cert is unrecognized"), false},
		{"x509 authority", x509.UnknownAuthorityError{}, false},
		{"x509 hostname", x509.HostnameError{}, false},
		{"certificate verifier timeout", &gotls.CertificateVerificationError{Err: context.DeadlineExceeded}, false},
		{"utls certificate verifier timeout", &utls.CertificateVerificationError{Err: context.DeadlineExceeded}, false},
		{"system roots error", x509.SystemRootsError{Err: syscall.ETIMEDOUT}, false},
		{"tls record protocol", gotls.RecordHeaderError{Msg: "invalid record"}, false},
		{"tls authentication alert", gotls.AlertError(42), false},
		{"http protocol", &http.ProtocolError{ErrorString: "invalid protocol"}, false},
		{"quic crypto alert", &quic.TransportError{ErrorCode: quic.TransportErrorCode(0x12a)}, false},
		{"quic protocol", &quic.TransportError{ErrorCode: quic.ProtocolViolation}, false},
		{"browser opaque authentication", &websocket.CloseError{Code: 4000, Text: "Browser task failed"}, false},
		{"browser opaque protocol", &websocket.CloseError{Code: websocket.CloseProtocolError, Text: "Browser task failed"}, false},
		{"unknown error payload", errors.New("timeout reset tls authentication opaque-payload"), false},
		{"deadline", context.DeadlineExceeded, true},
		{"socket deadline", os.ErrDeadlineExceeded, true},
		{"tcp timeout", &stdnet.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, true},
		{"tcp reset", &stdnet.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "recv", Err: syscall.ECONNRESET}}, true},
		{"broken pipe", syscall.EPIPE, true},
		{"network timeout", syscall.ETIMEDOUT, true},
		{"quic idle timeout", &quic.IdleTimeoutError{}, true},
		{"quic handshake timeout", &quic.HandshakeTimeoutError{}, true},
		{"quic stateless reset", &quic.StatelessResetError{}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			controller, clock := adaptiveTestController(AdaptiveBalanced, 4, 32)
			lease := adaptiveAcquire(t, controller, adaptiveDown)
			lease.finish(wrapped(test.err))
			controller.mu.Lock()
			failures, transient := controller.directions[0].window.failures, controller.directions[0].window.transient
			controller.mu.Unlock()
			adaptiveTick(controller, clock)
			limits, active, _ := adaptiveSnapshot(controller)
			wantTransient, wantLimit := 0, 8
			if test.congestion {
				wantTransient, wantLimit = 1, 6
			}
			if failures != 1 || transient != wantTransient || limits[0] != wantLimit || active != [2]int{} {
				t.Fatalf("failure class: failures=%d transient=%d budget=%d active=%v", failures, transient, limits[0], active)
			}
		})
	}
	if runtime.GOOS == "windows" {
		for _, code := range []syscall.Errno{10053, 10054, 10060} {
			controller, clock := adaptiveTestController(AdaptiveBalanced, 4, 32)
			lease := adaptiveAcquire(t, controller, adaptiveDown)
			lease.finish(wrapped(&stdnet.OpError{Op: "read", Net: "tcp", Err: code}))
			adaptiveTick(controller, clock)
			limits, _, _ := adaptiveSnapshot(controller)
			if limits[0] != 6 {
				t.Fatalf("Windows network error %d stopped reducing congestion", code)
			}
		}
	}
}

func TestAdaptiveOpaqueFailureBlocksGrowthWithoutShrinking(t *testing.T) {
	controller, clock := adaptiveTestController(AdaptiveBalanced, 4, 32)
	var held []*adaptiveLease
	for range 8 {
		held = append(held, adaptiveAcquire(t, controller, adaptiveDown))
	}
	clock.advance(time.Millisecond)
	for _, lease := range held {
		lease.confirm()
	}
	adaptivePressure(t, controller, adaptiveDown, 16)
	for range 2 {
		for _, lease := range held {
			lease.progress(adaptiveDown, 1000)
		}
		adaptiveTick(controller, clock)
	}
	held[0].finish(&websocket.CloseError{Code: 4000, Text: "Browser task failed"})
	for _, lease := range held {
		lease.progress(adaptiveDown, 1000)
	}
	adaptiveTick(controller, clock)
	limits, _, _ := adaptiveSnapshot(controller)
	if limits[0] != 8 {
		t.Fatalf("opaque failure grew or shrank the budget: %v", limits)
	}
}

func TestAdaptiveHTTPStatusClassificationPreserved(t *testing.T) {
	for _, status := range []int{401, 403, 429, 503} {
		controller, clock := adaptiveTestController(AdaptiveBalanced, 4, 32)
		lease := adaptiveAcquire(t, controller, adaptiveUp)
		lease.reject(status)
		lease.finish(errors.New("HTTP rejection"))
		adaptiveTick(controller, clock)
		limits, active, _ := adaptiveSnapshot(controller)
		want := 8
		if status == 429 || status == 503 {
			want = 6
		}
		if limits[1] != want || active != [2]int{} {
			t.Fatalf("HTTP %d budget=%v active=%v", status, limits, active)
		}
	}
}
