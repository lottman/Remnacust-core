package xerahttp

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/testing/servers/tcp"
	"github.com/xtls/xray-core/testing/servers/udp"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/internet/tls"
)

func TestMixedIPv4IPv6UploadAndDownload(t *testing.T) {
	for _, alpn := range []string{"http/1.1", "h2", "h3"} {
		for _, mode := range []string{ModePacketUp, ModeStreamUp} {
			for _, pair := range []struct{ upload, download net.Address }{
				{net.LocalHostIP, net.LocalHostIP}, {net.LocalHostIP, net.LocalHostIPv6}, {net.LocalHostIPv6, net.LocalHostIP},
			} {
				t.Run(fmt.Sprintf("%s/%s/upload=%s/download=%s", alpn, mode, pair.upload, pair.download), func(t *testing.T) {
					upload, download := pair.upload, pair.download
					listenAddress := net.LocalHostIP
					if upload.Family().IsIPv6() || download.Family().IsIPv6() {
						requireXeraIPv6Loopback(t)
						listenAddress = net.AnyIPv6
					}
					certificate, hash := cert.MustGenerate(nil, cert.CommonName("localhost"), cert.DNSNames("localhost"))
					serverSettings := &internet.MemoryStreamConfig{
						ProtocolName: protocolName, ProtocolSettings: &Config{Path: "/mixed"}, SecurityType: "tls",
						SecuritySettings: &tls.Config{Certificate: []*tls.Certificate{tls.ParseCertificate(certificate)}, NextProtocol: []string{alpn}},
					}
					t.Cleanup(func() { serverSettings.Close() })
					port := tcp.PickPort()
					if alpn == "h3" {
						port = udp.PickPort()
					}
					listener, err := ListenXH(context.Background(), listenAddress, port, serverSettings, func(connection stat.Connection) {
						go func() {
							defer connection.Close()
							io.Copy(connection, connection)
						}()
					})
					if err != nil {
						t.Fatal(err)
					}
					defer listener.Close()
					clientTLS := &tls.Config{ServerName: "localhost", PinnedPeerCertSha256: [][]byte{hash[:]}, NextProtocol: []string{alpn}, Fingerprint: "golang"}
					config := &Config{Path: "/mixed", Mode: mode, DownloadSettings: &internet.StreamConfig{
						Address: net.NewIPOrDomain(download), Port: uint32(port), ProtocolName: protocolName,
						TransportSettings: []*internet.TransportConfig{{ProtocolName: protocolName, Settings: serial.ToTypedMessage(&Config{Path: "/mixed", Mode: mode})}},
						SecurityType:      serial.GetMessageType(clientTLS), SecuritySettings: []*serial.TypedMessage{serial.ToTypedMessage(clientTLS)},
					}}
					settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: config, SecurityType: "tls", SecuritySettings: clientTLS}
					t.Cleanup(func() { settings.Close() })
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					connection, err := Dial(ctx, net.TCPDestination(upload, port), settings)
					if err != nil {
						t.Fatal(err)
					}
					defer connection.Close()
					connection.SetDeadline(time.Now().Add(10 * time.Second))
					payload := make([]byte, 64<<10)
					if _, err := rand.Read(payload); err != nil {
						t.Fatal(err)
					}
					written := make(chan error, 1)
					go func() { _, err := connection.Write(payload); written <- err }()
					got := make([]byte, len(payload))
					if _, err := io.ReadFull(connection, got); err != nil {
						t.Fatal(err)
					}
					if err := <-written; err != nil || !bytes.Equal(got, payload) {
						t.Fatalf("mixed address family payload differs: %v", err)
					}
				})
			}
		}
	}
}
