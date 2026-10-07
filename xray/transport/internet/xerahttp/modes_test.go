package xerahttp

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
	"github.com/xtls/xray-core/testing/servers/tcp"
	"github.com/xtls/xray-core/testing/servers/udp"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/internet/tls"
	"google.golang.org/protobuf/proto"
)

func TestModesRoundTripAndClose(t *testing.T) {
	for _, alpn := range []string{"http/1.1", "h2", "h3"} {
		for _, mode := range []string{ModePacketUp, ModeStreamUp, ModeStreamOne, ModeStreamAuto} {
			if alpn == "http/1.1" && mode != ModePacketUp && mode != ModeStreamAuto {
				continue
			}
			for _, padding := range []string{"", PlacementHeader, PlacementCookie, PlacementQuery, PlacementQueryInHeader, "custom", "duplex"} {
				t.Run(alpn+"/"+mode+"/"+padding, func(t *testing.T) {
					certificate, hash := cert.MustGenerate(nil, cert.CommonName("localhost"))
					settings := &internet.MemoryStreamConfig{
						ProtocolName: "xera-http",
						ProtocolSettings: &Config{
							Path: "/modes", Mode: mode,
							XPaddingObfsMode: padding != "", XPaddingPlacement: padding,
							XPaddingMethod: string(PaddingMethodTokenish),
							XPaddingHeader: "X-Response-Token", XPaddingKey: "token",
						},
						SecurityType:     "tls",
						SecuritySettings: &tls.Config{Certificate: []*tls.Certificate{tls.ParseCertificate(certificate)}, PinnedPeerCertSha256: [][]byte{hash[:]}, NextProtocol: []string{alpn}},
					}
					if padding != "" {
						config := settings.ProtocolSettings.(*Config)
						config.ScMaxEachPostBytes = &RangeConfig{From: 32768, To: 65536}
						config.ScMinPostsIntervalMs = &RangeConfig{From: 0, To: 8}
						if padding == "custom" || padding == "duplex" {
							config.XPaddingPlacement = PlacementHeader
							config.CustomDownlinkPadding = testDownlinkConfig()
							if padding == "duplex" {
								config.CustomDownlinkPadding.Uplink = true
								config.CustomDownlinkPadding.BudgetPercent = 1
							}
						}
					}
					port := tcp.PickPort()
					if alpn == "h3" {
						port = udp.PickPort()
					}
					closed := make(chan struct{}, 12)
					listener, err := ListenXH(context.Background(), net.LocalHostIP, port, settings, func(c stat.Connection) {
						go func() { io.Copy(c, c); c.Close(); closed <- struct{}{} }()
					})
					if err != nil {
						t.Fatal(err)
					}
					defer listener.Close()
					for index := range 12 {
						clientSettings := settings
						if (padding == "custom" || padding == "duplex") && index%2 == 1 {
							config := proto.Clone(settings.ProtocolSettings.(*Config)).(*Config)
							config.CustomDownlinkPadding = nil
							clientSettings = &internet.MemoryStreamConfig{ProtocolName: settings.ProtocolName, ProtocolSettings: config, SecurityType: settings.SecurityType, SecuritySettings: settings.SecuritySettings}
						}
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						c, err := Dial(ctx, net.TCPDestination(net.LocalHostIP, port), clientSettings)
						cancel()
						if err != nil {
							t.Fatal(err)
						}
						finished := make(chan error, 1)
						go func() {
							payload := bytes.Repeat([]byte("xera-http"), 13000)
							writeDone := make(chan error, 1)
							go func() { _, err := c.Write(payload); writeDone <- err }()
							got := make([]byte, len(payload))
							_, readErr := io.ReadFull(c, got)
							if readErr != nil {
								finished <- readErr
								return
							}
							if !bytes.Equal(payload, got) {
								finished <- io.ErrUnexpectedEOF
								return
							}
							finished <- <-writeDone
						}()
						select {
						case err := <-finished:
							c.Close()
							if err != nil {
								t.Fatal(err)
							}
						case <-time.After(5 * time.Second):
							c.Close()
							t.Fatal("round trip stalled")
						}
						select {
						case <-closed:
						case <-time.After(5 * time.Second):
							t.Fatal("server stream survived close")
						}
					}
					runtime.GC()
				})
			}
		}
	}
}
