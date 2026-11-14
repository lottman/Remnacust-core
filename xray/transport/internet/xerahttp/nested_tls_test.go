package xerahttp

import (
	"bytes"
	"context"
	"crypto/rand"
	stdtls "crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	stdnet "net"
	"os"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
	"github.com/xtls/xray-core/testing/servers/tcp"
	"github.com/xtls/xray-core/testing/servers/udp"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/internet/tls"
)

func TestNestedTLSRoundTripAcrossXeraModes(t *testing.T) {
	innerCertificate, _ := cert.MustGenerate(nil, cert.CommonName("nested.example.test"), cert.DNSNames("nested.example.test"))
	innerPEM, innerKey := innerCertificate.ToPEM()
	innerPair, err := stdtls.X509KeyPair(innerPEM, innerKey)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(innerPEM) {
		t.Fatal("inner test certificate could not be trusted")
	}
	payload := make([]byte, 256<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	for _, profile := range []struct {
		alpn, fingerprint string
		address           net.Address
	}{
		{"http/1.1", "golang", net.LocalHostIP}, {"h2", "golang", net.LocalHostIP}, {"h2", "hellochrome_133", net.LocalHostIP}, {"h2", "hellochrome_155", net.LocalHostIP}, {"h3", "golang", net.LocalHostIP},
		{"http/1.1", "golang", net.LocalHostIPv6}, {"h2", "golang", net.LocalHostIPv6}, {"h2", "hellochrome_133", net.LocalHostIPv6}, {"h2", "hellochrome_155", net.LocalHostIPv6}, {"h3", "golang", net.LocalHostIPv6},
	} {
		for _, mode := range []string{ModePacketUp, ModeStreamUp, ModeStreamOne, ModeStreamAuto} {
			for _, padded := range []bool{false, true} {
				for _, innerVersion := range []uint16{stdtls.VersionTLS12, stdtls.VersionTLS13} {
					name := fmt.Sprintf("%s/%s/%s/%s/padded=%t/inner=%x", profile.address, profile.alpn, profile.fingerprint, mode, padded, innerVersion)
					t.Run(name, func(t *testing.T) {
						if profile.address.Family().IsIPv6() {
							requireXeraIPv6Loopback(t)
						}
						outerCertificate, outerHash := cert.MustGenerate(nil, cert.CommonName("localhost"), cert.DNSNames("localhost"))
						config := &Config{Path: "/nested", Mode: mode}
						if padded {
							config.CustomDownlinkPadding = testDownlinkConfig()
							config.CustomDownlinkPadding.Uplink = true
							config.CustomDownlinkPadding.BudgetPercent = 5
							config.CustomDownlinkPadding.BurstBytes = 4096
						}
						settings := &internet.MemoryStreamConfig{
							ProtocolName: protocolName, ProtocolSettings: config, SecurityType: "tls",
							SecuritySettings: &tls.Config{Certificate: []*tls.Certificate{tls.ParseCertificate(outerCertificate)},
								ServerName: "localhost", PinnedPeerCertSha256: [][]byte{outerHash[:]},
								NextProtocol: []string{profile.alpn}, Fingerprint: profile.fingerprint},
						}
						t.Cleanup(func() { settings.Close() })
						port := tcp.PickPort()
						if profile.alpn == "h3" {
							port = udp.PickPort()
						}
						serverStopped := make(chan struct{})
						serverResult := make(chan error, 1)
						listener, err := ListenXH(context.Background(), profile.address, port, settings, func(c stat.Connection) {
							go func() {
								defer close(serverStopped)
								defer c.Close()
								secured := stdtls.Server(c, &stdtls.Config{Certificates: []stdtls.Certificate{innerPair}, MinVersion: innerVersion, MaxVersion: innerVersion})
								defer secured.Close()
								ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
								defer cancel()
								if err := secured.HandshakeContext(ctx); err != nil {
									serverResult <- err
									return
								}
								_, err := io.CopyN(secured, secured, int64(len(payload)))
								serverResult <- err
							}()
						})
						if err != nil {
							t.Fatal(err)
						}
						defer listener.Close()
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						connection, err := Dial(ctx, net.TCPDestination(profile.address, port), settings)
						if err != nil {
							t.Fatal(err)
						}
						defer connection.Close()
						if err := connection.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
							t.Fatal(err)
						}
						secured := stdtls.Client(connection, &stdtls.Config{RootCAs: roots, ServerName: "nested.example.test", MinVersion: innerVersion, MaxVersion: innerVersion})
						defer secured.Close()
						if err := secured.HandshakeContext(ctx); err != nil {
							t.Fatal(err)
						}
						writeResult := make(chan error, 1)
						go func() { _, err := secured.Write(payload); writeResult <- err }()
						got := make([]byte, len(payload))
						if _, err := io.ReadFull(secured, got); err != nil {
							t.Fatal(err)
						}
						if err := <-writeResult; err != nil || !bytes.Equal(got, payload) {
							t.Fatalf("nested TLS payload differs: %v", err)
						}
						if err := <-serverResult; err != nil {
							t.Fatal(err)
						}
						secured.Close()
						select {
						case <-serverStopped:
						case <-ctx.Done():
							t.Fatal("nested TLS server survived close")
						}
					})
				}
			}
		}
	}
}

func requireXeraIPv6Loopback(t *testing.T) {
	t.Helper()
	listener, err := stdnet.Listen("tcp6", "[::1]:0")
	if err != nil {
		if os.Getenv("REMNACUST_REQUIRE_IPV6_TESTS") == "1" {
			t.Fatal(err)
		}
		t.Skipf("IPv6 loopback is unavailable: %v", err)
	}
	defer listener.Close()
	probe, err := stdnet.DialTimeout("tcp6", listener.Addr().String(), time.Second)
	if err != nil {
		if os.Getenv("REMNACUST_REQUIRE_IPV6_TESTS") == "1" {
			t.Fatal(err)
		}
		t.Skipf("IPv6 loopback cannot connect before Xera starts: %v", err)
	}
	probe.Close()
}
