package xerahttp

import (
	"context"
	gotls "crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/tls"
	"golang.org/x/net/http2"
)

func TestBrowserH2VerifiedChrome155TLSWire(t *testing.T) {
	data, err := os.ReadFile("testdata/chrome155-http2.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Settings []struct {
			ID    uint16 `json:"id"`
			Value uint32 `json:"value"`
		} `json:"settings"`
		Window uint32   `json:"connectionWindowIncrement"`
		Pseudo []string `json:"pseudoHeaderOrder"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	var expectedSettings []http2.Setting
	for _, setting := range fixture.Settings {
		expectedSettings = append(expectedSettings, http2.Setting{ID: http2.SettingID(setting.ID), Val: setting.Value})
	}
	for _, fingerprint := range []string{"", "chrome", "hellochrome_155"} {
		t.Run("fingerprint="+fingerprint, func(t *testing.T) {
			certificate, hash := cert.MustGenerate(nil, cert.CommonName("localhost"), cert.DNSNames("localhost"))
			pem, key := certificate.ToPEM()
			pair, err := gotls.X509KeyPair(pem, key)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := gotls.Listen("tcp", "127.0.0.1:0", &gotls.Config{
				Certificates: []gotls.Certificate{pair}, NextProtos: []string{"h2"},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			observed := make(chan browserH2Observation, 1)
			go func() {
				peer, err := listener.Accept()
				if err != nil {
					observed <- browserH2Observation{err: err}
					return
				}
				observed <- observeBrowserH2(peer)
			}()
			settings := &internet.MemoryStreamConfig{
				ProtocolSettings: &Config{}, SecurityType: "tls",
				SecuritySettings: &tls.Config{Fingerprint: fingerprint, ServerName: "localhost", PinnedPeerCertSha256: [][]byte{hash[:]}},
			}
			client := createHTTPClient(xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(listener.Addr().(*net.TCPAddr).Port)), settings).(*DefaultDialerClient)
			defer client.Close()
			for range 2 {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				request, _ := http.NewRequestWithContext(ctx, "GET", "https://localhost/api/", nil)
				client.transportConfig.FillStreamRequest(request, "session", "")
				response, err := client.client.Do(request)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				cancel()
				if err != nil || string(body) != "ok" || response.ProtoMajor != 2 {
					t.Fatalf("unexpected response: protocol=%s body=%q err=%v", response.Proto, body, err)
				}
			}
			result := <-observed
			if result.err != nil {
				t.Fatal(result.err)
			}
			if !reflect.DeepEqual(result.settings, expectedSettings) || result.window != fixture.Window || !reflect.DeepEqual(result.pseudo, fixture.Pseudo) {
				t.Fatalf("Chrome 155 HTTP/2 capture differs: %+v", result)
			}
		})
	}
}
