package xerahttp

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
	"github.com/xtls/xray-core/testing/servers/tcp"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/internet/tls"
)

func TestStreamingHTTPMethodsAndServerParameters(t *testing.T) {
	for _, alpn := range []string{"http/1.1", "h2"} {
		for _, mode := range []string{ModePacketUp, ModeStreamUp, ModeStreamOne} {
			for _, method := range []string{"POST", "OPTIONS"} {
				t.Run(alpn+"/"+mode+"/"+method, func(t *testing.T) {
					certificate, hash := cert.MustGenerate(nil, cert.CommonName("localhost"))
					settings := &internet.MemoryStreamConfig{
						ProtocolName:     protocolName,
						ProtocolSettings: &Config{Path: "/stream", Mode: mode, UplinkHTTPMethod: method, H2Params: []byte(`{"MaxConcurrentStreams":128,"MaxReadFrameSize":65536}`)},
						SecurityType:     "tls",
						SecuritySettings: &tls.Config{Certificate: []*tls.Certificate{tls.ParseCertificate(certificate)}, PinnedPeerCertSha256: [][]byte{hash[:]}, NextProtocol: []string{alpn}},
					}
					port := tcp.PickPort()
					listener, err := ListenXH(context.Background(), net.LocalHostIP, port, settings, func(conn stat.Connection) {
						go func() { defer conn.Close(); io.Copy(conn, conn) }()
					})
					if err != nil {
						t.Fatal(err)
					}
					defer listener.Close()
					if listener.(*Listener).server.HTTP2.MaxConcurrentStreams != 128 {
						t.Fatal("HTTP/2 parameters did not reach the server")
					}
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					conn, err := Dial(ctx, net.TCPDestination(net.LocalHostIP, port), settings)
					if err != nil {
						t.Fatal(err)
					}
					defer conn.Close()
					conn.SetDeadline(time.Now().Add(5 * time.Second))
					payload := bytes.Repeat([]byte("duplex"), 32768)
					written := make(chan error, 1)
					go func() { _, err := conn.Write(payload); written <- err }()
					got := make([]byte, len(payload))
					if _, err := io.ReadFull(conn, got); err != nil {
						t.Fatal(err)
					}
					if err := <-written; err != nil || !bytes.Equal(payload, got) {
						t.Fatal("streamed upload was truncated", err)
					}
				})
			}
		}
	}
}
