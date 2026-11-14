package xerahttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"syscall"
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

func adaptiveLoopbackError(t *testing.T, err error) {
	t.Helper()
	if runtime.GOOS == "windows" && (errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.Errno(10013))) {
		t.Skipf("Windows denied isolated loopback socket access: %v", err)
	}
	t.Fatal(err)
}

func TestAdaptiveHTTPTransportMatrix(t *testing.T) {
	for _, alpn := range []string{"http/1.1", "h2", "h3"} {
		fingerprints := []string{"golang"}
		if alpn == "h2" {
			fingerprints = append(fingerprints, "chrome")
		}
		for _, fingerprint := range fingerprints {
			for _, mode := range []string{ModePacketUp, ModeStreamUp, ModeStreamOne, ModeStreamAuto} {
				if alpn == "http/1.1" && mode != ModePacketUp && mode != ModeStreamAuto {
					continue
				}
				for _, padded := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/padding=%t", alpn, fingerprint, mode, padded), func(t *testing.T) {
						certificate, hash := cert.MustGenerate(nil, cert.CommonName("localhost"))
						server := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: &Config{Path: "/adaptive", ScMaxEachPostBytes: &RangeConfig{From: 65536, To: 65536}}, SecurityType: "tls", SecuritySettings: &tls.Config{Certificate: []*tls.Certificate{tls.ParseCertificate(certificate)}, NextProtocol: []string{alpn}}}
						settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: &Config{Path: "/adaptive", Mode: mode, Adaptive: &AdaptiveConfig{Mode: AdaptiveBalanced, MinStreams: 1, MaxStreams: 1}, UplinkDataPlacement: PlacementBody, ScMinPostsIntervalMs: &RangeConfig{From: -1, To: -1}, ScMaxEachPostBytes: &RangeConfig{From: 16384, To: 32768}}, SecurityType: "tls", SecuritySettings: &tls.Config{ServerName: "localhost", PinnedPeerCertSha256: [][]byte{hash[:]}, NextProtocol: []string{alpn}, Fingerprint: fingerprint}}
						if padded {
							for _, config := range []*Config{server.ProtocolSettings.(*Config), settings.ProtocolSettings.(*Config)} {
								config.CustomDownlinkPadding = testDownlinkConfig()
								config.CustomDownlinkPadding.Uplink = true
							}
						}
						t.Cleanup(func() { settings.Close(); server.Close() })
						port := tcp.PickPort()
						if alpn == "h3" {
							port = udp.PickPort()
						}
						listener, err := ListenXH(context.Background(), net.LocalHostIP, port, server, func(connection stat.Connection) {
							go func() { defer connection.Close(); io.Copy(connection, connection) }()
						})
						if err != nil {
							adaptiveLoopbackError(t, err)
						}
						defer listener.Close()
						dest := net.TCPDestination(net.LocalHostIP, port)
						var group sync.WaitGroup
						failures := make(chan error, 4)
						for index := range 4 {
							group.Add(1)
							go func() {
								defer group.Done()
								ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
								defer cancel()
								connection, err := Dial(ctx, dest, settings)
								if err != nil {
									failures <- err
									return
								}
								defer connection.Close()
								connection.SetDeadline(time.Now().Add(5 * time.Second))
								payload := bytes.Repeat([]byte{byte(index + 1)}, 65536)
								written := make(chan error, 1)
								go func() { _, err := connection.Write(payload); written <- err }()
								got := make([]byte, len(payload))
								if _, err := io.ReadFull(connection, got); err != nil {
									failures <- err
									connection.Close()
									<-written
									return
								}
								if err := <-written; err != nil {
									failures <- err
									return
								}
								if !bytes.Equal(payload, got) {
									failures <- fmt.Errorf("%s changed payload", mode)
								}
							}()
						}
						group.Wait()
						close(failures)
						for err := range failures {
							adaptiveLoopbackError(t, err)
						}
						if alpn == "h3" {
							dest.Network = net.Network_UDP
						}
						globalDialerAccess.Lock()
						manager := globalDialerMap[dialerConf{dest, settings}]
						globalDialerAccess.Unlock()
						if manager == nil || manager.adaptive == nil {
							t.Fatal("native path omitted adaptive admission")
						}
						leaseEventually(t, func() bool {
							_, active, queued := adaptiveSnapshot(manager.adaptive)
							return active == [2]int{} && queued == 0
						})
					})
				}
			}
		}
	}
}
