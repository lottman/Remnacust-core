package reality

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	goreality "github.com/xtls/reality"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/core"
)

func TestClientWireVersionAndAuthentication(t *testing.T) {
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	target.EnableHTTP2 = true
	target.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	target.StartTLS()
	defer target.Close()
	defer target.CloseClientConnections()
	goreality.DetectPostHandshakeRecordsLens(&goreality.Config{Type: "tcp", Dest: target.Listener.Addr().String(), ServerNames: map[string]bool{"example.com": true}})
	for deadline := time.Now().Add(12 * time.Second); ; {
		if val, ok := goreality.GlobalPostHandshakeRecordsLens.Load(target.Listener.Addr().String() + " example.com 2"); ok {
			if _, ready := val.([]int); ready {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("target detection did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	short := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	for _, tc := range []struct {
		name                 string
		min, max             []byte
		wrongShort, wrongKey bool
		allow                bool
	}{
		{name: "official-minimum", min: []byte{26, 3, 27}, max: []byte{26, 11, 9}, allow: true},
		{name: "future-minimum", min: []byte{26, 11, 10}},
		{name: "older-maximum", max: []byte{26, 11, 8}},
		{name: "invalid-short-id", min: []byte{26, 3, 27}, wrongShort: true},
		{name: "invalid-key", min: []byte{26, 3, 27}, wrongKey: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			result := make(chan error, 1)
			go func() {
				raw, err := listener.Accept()
				if err != nil {
					result <- err
					return
				}
				defer raw.Close()
				raw.SetDeadline(time.Now().Add(3 * time.Second))
				conn, err := goreality.Server(context.Background(), raw, &goreality.Config{
					DialContext: (&net.Dialer{Timeout: time.Second}).DialContext,
					Type:        "tcp", Dest: target.Listener.Addr().String(), PrivateKey: private,
					ServerNames: map[string]bool{"example.com": true}, ShortIds: map[[8]byte]bool{short: true},
					MinClientVer: tc.min, MaxClientVer: tc.max, MaxTimeDiff: time.Minute,
					SessionTicketsDisabled: true,
				})
				if err != nil {
					result <- err
					return
				}
				defer conn.Close()
				expected := [3]byte{core.UpstreamVersion_x, core.UpstreamVersion_y, core.UpstreamVersion_z}
				if conn.ClientVer != expected {
					result <- io.ErrUnexpectedEOF
					return
				}
				var data [1]byte
				_, err = io.ReadFull(conn, data[:])
				if err == nil && data[0] != 42 {
					err = io.ErrUnexpectedEOF
				}
				result <- err
			}()
			raw, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			raw.SetDeadline(time.Now().Add(3 * time.Second))
			sid := append([]byte(nil), short[:]...)
			if tc.wrongShort {
				sid[0] ^= 1
			}
			pub := private.PublicKey().Bytes()
			if tc.wrongKey {
				other, _ := ecdh.X25519().GenerateKey(rand.Reader)
				pub = other.PublicKey().Bytes()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, err := UClient(raw, &Config{Fingerprint: "chrome", ServerName: "example.com", PublicKey: pub, ShortId: sid}, ctx, xnet.TCPDestination(xnet.DomainAddress("example.com"), 443))
			if tc.allow {
				if err != nil {
					t.Fatal(err)
				}
				if _, err = conn.Write([]byte{42}); err != nil {
					t.Fatal(err)
				}
				select {
				case err = <-result:
				case <-time.After(5 * time.Second):
					t.Fatal("server handshake did not finish")
				}
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					conn.Close()
					t.Fatal("invalid client accepted")
				}
				raw.Close()
				select {
				case err = <-result:
				case <-time.After(5 * time.Second):
					t.Fatal("server rejection did not finish")
				}
				if err == nil {
					t.Fatal("server accepted invalid client")
				}
			}
		})
	}
}
