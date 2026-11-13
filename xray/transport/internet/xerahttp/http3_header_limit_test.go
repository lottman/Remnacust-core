package xerahttp

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
	"github.com/xtls/xray-core/testing/servers/udp"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/internet/tls"
)

func TestHTTP3EnforcesConfiguredHeaderLimit(t *testing.T) {
	for _, limit := range []int32{0, 4096} {
		t.Run(strconv.Itoa(int(limit)), func(t *testing.T) {
			certificate, hash := cert.MustGenerate(nil, cert.CommonName("localhost"), cert.DNSNames("localhost"))
			config := &Config{Path: "/header-limit/", Mode: ModePacketUp, Host: "localhost", ServerMaxHeaderBytes: limit}
			settings := &internet.MemoryStreamConfig{
				ProtocolName: protocolName, ProtocolSettings: config, SecurityType: "tls",
				SecuritySettings: &tls.Config{
					ServerName: "localhost", Certificate: []*tls.Certificate{tls.ParseCertificate(certificate)},
					PinnedPeerCertSha256: [][]byte{hash[:]}, NextProtocol: []string{"h3"},
				},
			}
			port := udp.PickPort()
			listener, err := ListenXH(context.Background(), net.LocalHostIP, port, settings, func(c stat.Connection) { c.Close() })
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			client := createHTTPClient(net.UDPDestination(net.LocalHostIP, port), settings).(*DefaultDialerClient)
			defer client.Close()
			for _, size := range []int{128, 16384, 128} {
				request, err := http.NewRequest(http.MethodOptions, "https://localhost/header-limit/", nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Origin", "https://browser.example")
				request.Header.Set("Access-Control-Request-Method", "POST")
				request.Header.Set("X-Large", strings.Repeat("a", size))
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				response, err := client.client.Do(request.WithContext(ctx))
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				_, readErr := io.Copy(io.Discard, response.Body)
				response.Body.Close()
				cancel()
				if response.ProtoMajor != 3 {
					t.Fatalf("unexpected protocol: %s", response.Proto)
				}
				if size == 16384 {
					if response.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
						t.Fatalf("oversized header returned HTTP %d, want 431", response.StatusCode)
					}
				} else if response.StatusCode != http.StatusOK || readErr != nil {
					t.Fatalf("valid request failed: status=%d, read=%v", response.StatusCode, readErr)
				}
			}
			if got := listener.(*Listener).h3server.MaxHeaderBytes; got != config.GetNormalizedServerMaxHeaderBytes() {
				t.Fatalf("HTTP/3 header limit = %d, want %d", got, config.GetNormalizedServerMaxHeaderBytes())
			}
		})
	}
}
