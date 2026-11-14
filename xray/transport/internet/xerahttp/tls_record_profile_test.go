package xerahttp

import (
	"bytes"
	"context"
	gotls "crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/tls"
)

type xeraRecordCapture struct {
	mu      sync.Mutex
	pending []byte
	sizes   []int
}

func (c *xeraRecordCapture) add(data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, data...)
	for len(c.pending) >= 5 {
		size := int(binary.BigEndian.Uint16(c.pending[3:5]))
		if len(c.pending) < 5+size {
			return
		}
		if c.pending[0] == 23 {
			c.sizes = append(c.sizes, size)
		}
		c.pending = c.pending[5+size:]
	}
}

type xeraRecordListener struct {
	net.Listener
	capture *xeraRecordCapture
}

func (l *xeraRecordListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &xeraRecordReadConn{Conn: conn, capture: l.capture}, nil
}

type xeraRecordReadConn struct {
	net.Conn
	capture *xeraRecordCapture
}

func (c *xeraRecordReadConn) Read(data []byte) (int, error) {
	n, err := c.Conn.Read(data)
	c.capture.add(data[:n])
	return n, err
}

func TestVerifiedXeraTLSProfilesDoNotUseRecordSizeRamp(t *testing.T) {
	for _, fingerprint := range []string{"hellochrome_133", "hellochrome_155", "hellofirefox_148"} {
		t.Run(fingerprint, func(t *testing.T) {
			certificate, hash := cert.MustGenerate(nil, cert.CommonName("localhost"), cert.DNSNames("localhost"))
			pem, key := certificate.ToPEM()
			pair, err := gotls.X509KeyPair(pem, key)
			if err != nil {
				t.Fatal(err)
			}
			capture := &xeraRecordCapture{}
			payload := bytes.Repeat([]byte{42}, 64<<10)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(data, payload) || r.ProtoMajor != 2 {
					t.Errorf("incomplete H2 payload: size=%d protocol=%s error=%v", len(data), r.Proto, err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			server.Listener = &xeraRecordListener{Listener: server.Listener, capture: capture}
			server.EnableHTTP2 = true
			server.TLS = &gotls.Config{Certificates: []gotls.Certificate{pair}}
			server.StartTLS()
			defer server.Close()
			settings := &internet.MemoryStreamConfig{
				ProtocolSettings: &Config{Mode: ModePacketUp}, SecurityType: "tls",
				SecuritySettings: &tls.Config{Fingerprint: fingerprint, ServerName: "localhost", PinnedPeerCertSha256: [][]byte{hash[:]}, NextProtocol: []string{"h2"}},
			}
			port := xnet.Port(server.Listener.Addr().(*net.TCPAddr).Port)
			client := createHTTPClient(xnet.TCPDestination(xnet.LocalHostIP, port), settings).(*DefaultDialerClient)
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := client.PostPacket(ctx, server.URL+"/api/", "session", "0", buf.MergeBytes(nil, payload)); err != nil {
				t.Fatal(err)
			}
			capture.mu.Lock()
			sizes := append([]int(nil), capture.sizes...)
			capture.mu.Unlock()
			fullRecords := 0
			for _, size := range sizes {
				if size == 16384+17 {
					fullRecords++
				}
			}
			if fingerprint != "hellofirefox_148" {
				if fullRecords < 4 {
					t.Fatalf("verified H2 profile retains TLS record ramp: %v", sizes)
				}
			} else if fullRecords >= 4 {
				t.Fatalf("record policy unexpectedly changed for unsupported profile: %v", sizes)
			}
		})
	}
}
