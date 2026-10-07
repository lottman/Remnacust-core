package xerahttp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	stdnet "net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/testing/servers/tcp"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/internet/tls"
)

// HTTP/2 on the client/CDN leg does not imply streaming request forwarding.
// A buffering reverse proxy must receive a finite upload before the origin can reply.
func TestStreamAutoThroughBufferingCDN(t *testing.T) {
	port := tcp.PickPort()
	originConfig := &internet.MemoryStreamConfig{ProtocolName: "xera-http", ProtocolSettings: &Config{Path: "/cdn", Mode: ModeAuto}}
	origin, err := ListenXH(context.Background(), net.LocalHostIP, port, originConfig, func(c stat.Connection) {
		go func() { defer c.Close(); io.Copy(c, c) }()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	originURL, _ := url.Parse("http://127.0.0.1:" + port.String())
	proxy := httputil.NewSingleHostReverseProxy(originURL)
	proxy.FlushInterval = -1
	edge := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			r.Body.Close()
			if err != nil {
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		proxy.ServeHTTP(w, r)
	}))
	edge.EnableHTTP2 = true
	edge.StartTLS()
	defer edge.Close()
	_, rawPort, _ := stdnet.SplitHostPort(edge.Listener.Addr().String())
	edgePort, _ := strconv.Atoi(rawPort)
	certificateHash := sha256.Sum256(edge.Certificate().Raw)
	settings := &internet.MemoryStreamConfig{
		ProtocolName:     "xera-http",
		ProtocolSettings: &Config{Path: "/cdn", Mode: ModeStreamAuto},
		SecurityType:     "tls",
		SecuritySettings: &tls.Config{PinnedPeerCertSha256: [][]byte{certificateHash[:]}, NextProtocol: []string{"h2"}}, // loopback test certificate only
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := Dial(ctx, net.TCPDestination(net.LocalHostIP, net.Port(edgePort)), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	done := make(chan error, 1)
	go func() {
		payload := []byte("buffered-cdn-round-trip")
		if _, err := conn.Write(payload); err != nil {
			done <- err
			return
		}
		got := make([]byte, len(payload))
		_, err := io.ReadFull(conn, got)
		if err == nil && !bytes.Equal(got, payload) {
			err = io.ErrUnexpectedEOF
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("stream-auto stalled behind a request-buffering HTTP/2 CDN")
	}
}
