package xerahttp

import (
	"context"
	"crypto/sha256"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/tls"
)

func TestAuthenticatedRealityAllowsAbsentALPN(t *testing.T) {
	if err := validateRealityHTTPALPN("2", ""); err != nil {
		t.Fatal(err)
	}
	if err := validateHTTPALPN("2", ""); err == nil {
		t.Fatal("ordinary TLS must negotiate h2")
	}
	if err := validateRealityHTTPALPN("2", "http/1.1"); err == nil {
		t.Fatal("explicit incompatible ALPN must be rejected")
	}
}

func TestHTTP2RejectsServerWithoutH2ALPN(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("HTTP request reached server after ALPN mismatch")
	}))
	defer server.Close()
	hash := sha256.Sum256(server.Certificate().Raw)
	addr := server.Listener.Addr().(*net.TCPAddr)
	c := createHTTPClient(xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(addr.Port)), &internet.MemoryStreamConfig{
		ProtocolSettings: &Config{}, SecurityType: "tls",
		SecuritySettings: &tls.Config{PinnedPeerCertSha256: [][]byte{hash[:]}, NextProtocol: []string{"h2", "http/1.1"}},
	}).(*DefaultDialerClient)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, _, _, err := c.OpenStream(ctx, server.URL, "", nil, false)
	if r != nil {
		r.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "ALPN mismatch") {
		t.Fatalf("negotiation failure was not detected: %v", err)
	}
}
