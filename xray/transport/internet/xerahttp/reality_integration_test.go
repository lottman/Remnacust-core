package xerahttp

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	gotls "crypto/tls"
	"fmt"
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

func TestRealityModesRoundTripAndClose(t *testing.T) {
	decoy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	decoy.EnableHTTP2 = true
	decoy.TLS = &gotls.Config{MinVersion: gotls.VersionTLS13, CurvePreferences: []gotls.CurveID{gotls.X25519}}
	decoy.Config.ErrorLog = log.New(io.Discard, "", 0)
	decoy.StartTLS()
	defer decoy.Close()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	shortID := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	for _, mode := range []string{ModeStreamAuto, ModeStreamOne, ModeStreamUp} {
		t.Run(mode, func(t *testing.T) {
			serverSettings := &internet.MemoryStreamConfig{
				ProtocolName: protocolName, ProtocolSettings: &Config{Path: "/reality-check", Mode: mode},
				SecurityType: "reality", SecuritySettings: &reality.Config{
					Dest: decoy.Listener.Addr().String(), Type: "tcp", ServerNames: []string{"localhost"},
					PrivateKey: key.Bytes(), ShortIds: [][]byte{shortID},
				},
			}
			clientSettings := &internet.MemoryStreamConfig{
				ProtocolName: protocolName, ProtocolSettings: &Config{Path: "/reality-check", Mode: mode},
				SecurityType: "reality", SecuritySettings: &reality.Config{
					ServerName: "localhost", Fingerprint: "chrome", PublicKey: key.PublicKey().Bytes(), ShortId: shortID,
				},
			}
			port := tcp.PickPort()
			const sessions = 24
			closed := make(chan struct{}, sessions)
			listener, err := ListenXH(context.Background(), net.LocalHostIP, port, serverSettings, func(c stat.Connection) {
				go func() { io.Copy(c, c); c.Close(); closed <- struct{}{} }()
			})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			dest := net.TCPDestination(net.LocalHostIP, port)
			defer func() {
				globalDialerAccess.Lock()
				defer globalDialerAccess.Unlock()
				key := dialerConf{dest, clientSettings}
				if manager := globalDialerMap[key]; manager != nil {
					for _, client := range manager.xmuxClients {
						client.NotUsed.Store(true)
						client.maybeClose()
					}
					delete(globalDialerMap, key)
				}
			}()
			results := make(chan error, sessions)
			for index := range sessions {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					c, err := Dial(ctx, dest, clientSettings)
					if err != nil {
						results <- err
						return
					}
					defer c.Close()
					if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
						results <- err
						return
					}
					payload := bytes.Repeat([]byte{byte(index), 0x5a}, 65536)
					if _, err = c.Write(payload); err == nil {
						got := make([]byte, len(payload))
						_, err = io.ReadFull(c, got)
						if err == nil && !bytes.Equal(payload, got) {
							err = fmt.Errorf("session %d payload differs", index)
						}
					}
					results <- err
				}()
			}
			for range sessions {
				if err := <-results; err != nil {
					t.Error(err)
				}
			}
			deadline := time.After(10 * time.Second)
			for range sessions {
				select {
				case <-closed:
				case <-deadline:
					t.Fatal("REALITY server streams survived client close")
				}
			}
		})
	}
}
