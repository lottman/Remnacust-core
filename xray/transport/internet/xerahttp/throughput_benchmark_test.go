package xerahttp

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	gotls "crypto/tls"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/testing/servers/tcp"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/reality"
	"github.com/xtls/xray-core/transport/internet/stat"
)

// Loopback baseline only: this does not model Internet latency, loss, or TUN.
func BenchmarkRealityStreamThroughput(b *testing.B) {
	for _, mode := range []string{ModeStreamAuto, ModeStreamUp} {
		b.Run(mode, func(b *testing.B) {
			decoy := httptest.NewUnstartedServer(http.NotFoundHandler())
			decoy.EnableHTTP2 = true
			decoy.TLS = &gotls.Config{MinVersion: gotls.VersionTLS13, CurvePreferences: []gotls.CurveID{gotls.X25519}}
			decoy.Config.ErrorLog = log.New(io.Discard, "", 0)
			decoy.StartTLS()
			defer decoy.Close()
			key, err := ecdh.X25519().GenerateKey(rand.Reader)
			if err != nil {
				b.Fatal(err)
			}
			sid := []byte{1, 2, 3, 4, 5, 6, 7, 8}
			server := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: &Config{Path: "/bench", Mode: mode}, SecurityType: "reality", SecuritySettings: &reality.Config{Dest: decoy.Listener.Addr().String(), Type: "tcp", ServerNames: []string{"localhost"}, PrivateKey: key.Bytes(), ShortIds: [][]byte{sid}}}
			client := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: &Config{Path: "/bench", Mode: mode}, SecurityType: "reality", SecuritySettings: &reality.Config{ServerName: "localhost", Fingerprint: "chrome", PublicKey: key.PublicKey().Bytes(), ShortId: sid}}
			port := tcp.PickPort()
			listener, err := ListenXH(context.Background(), net.LocalHostIP, port, server, func(c stat.Connection) { go func() { defer c.Close(); io.Copy(c, c) }() })
			if err != nil {
				b.Fatal(err)
			}
			defer listener.Close()
			dest := net.TCPDestination(net.LocalHostIP, port)
			defer func() {
				globalDialerAccess.Lock()
				defer globalDialerAccess.Unlock()
				k := dialerConf{dest, client}
				if m := globalDialerMap[k]; m != nil {
					for _, c := range m.xmuxClients {
						c.NotUsed.Store(true)
						c.maybeClose()
					}
					delete(globalDialerMap, k)
				}
			}()
			c, err := Dial(context.Background(), dest, client)
			if err != nil {
				b.Fatal(err)
			}
			defer c.Close()
			if err := c.SetDeadline(time.Now().Add(60 * time.Second)); err != nil {
				b.Fatal(err)
			}
			// REALITY discovers the decoy's post-handshake records lazily. Measure
			// startup separately so a cold target does not distort steady throughput.
			started := time.Now()
			if _, err := c.Write([]byte{42}); err != nil {
				b.Fatal(err)
			}
			warm := make([]byte, 1)
			if _, err := io.ReadFull(c, warm); err != nil {
				b.Fatal(err)
			}
			if warm[0] != 42 {
				b.Fatal("warmup payload differs")
			}
			startup := time.Since(started)
			const size = 1024 * 1024
			data := make([]byte, size)
			if _, err = rand.Read(data); err != nil {
				b.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() { _, err := io.CopyN(io.Discard, c, int64(b.N)*size); finished <- err }()
			b.SetBytes(size)
			b.ResetTimer()
			for range b.N {
				if _, err := c.Write(data); err != nil {
					b.Fatal(err)
				}
			}
			if err := <-finished; err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			b.ReportMetric(float64(startup.Microseconds())/1000, "startup-ms")
		})
	}
}
