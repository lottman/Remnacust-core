package reality

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	stdtls "crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
)

type realityOwnedConn struct {
	net.Conn
	closes atomic.Int32
}

func (c *realityOwnedConn) Close() error {
	c.closes.Add(1)
	return c.Conn.Close()
}

func TestRealityClientOwnsEarlyFailureConnection(t *testing.T) {
	for _, sized := range []bool{false, true} {
		for _, fingerprint := range []string{"nonexistent-fingerprint", "chrome", "hellochrome_58"} {
			t.Run(fmt.Sprintf("%v/%s", sized, fingerprint), func(t *testing.T) {
				raw, peer := net.Pipe()
				defer raw.Close()
				defer peer.Close()
				tracked := &realityOwnedConn{Conn: raw}
				config := &Config{Fingerprint: fingerprint, ServerName: "localhost"}
				if fingerprint == "hellochrome_58" {
					private, err := ecdh.X25519().GenerateKey(rand.Reader)
					if err != nil {
						t.Fatal(err)
					}
					config.PublicKey = private.PublicKey().Bytes()
				}
				var conn net.Conn
				var err error
				if sized {
					conn, err = UClientWithRecordSizing(tracked, config, context.Background(), xnet.TCPDestination(xnet.LocalHostIP, 443), true)
				} else {
					conn, err = UClient(tracked, config, context.Background(), xnet.TCPDestination(xnet.LocalHostIP, 443))
				}
				if conn != nil || err == nil {
					t.Fatal("early failure returned a connection or no error")
				}
				if count := tracked.closes.Load(); count != 1 {
					t.Fatalf("helper-owned raw Close calls=%d", count)
				}
				peer.SetReadDeadline(time.Now().Add(time.Second))
				if n, err := peer.Read(make([]byte, 1)); n != 0 || err != io.EOF {
					t.Fatal("early failure did not close raw before TLS bytes", n, err)
				}
			})
		}
	}
}

func TestRealityClientTransfersFallbackConnectionToSpider(t *testing.T) {
	if os.Getenv("REMNACUST_REALITY_OWNERSHIP_CHILD") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRealityClientTransfersFallbackConnectionToSpider$", "-test.v")
		command.Env = append(os.Environ(), "REMNACUST_REALITY_OWNERSHIP_CHILD=1", "GODEBUG="+os.Getenv("GODEBUG")+",x509usefallbackroots=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated fallback test failed: %v\n%s", err, output)
		}
		return
	}
	ca, _ := cert.MustGenerate(nil, cert.Authority(true), cert.CommonName("loopback fixture CA"), cert.KeyUsage(x509.KeyUsageCertSign|x509.KeyUsageDigitalSignature))
	root, err := x509.ParseCertificate(ca.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	x509.SetFallbackRoots(roots)
	leaf, _ := cert.MustGenerate(ca, cert.CommonName("localhost"), cert.DNSNames("localhost"))
	pem, key := leaf.ToPEM()
	pair, err := stdtls.X509KeyPair(pem, key)
	if err != nil {
		t.Fatal(err)
	}
	started, release, returned := make(chan struct{}, 1), make(chan struct{}), make(chan error, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			returned <- fmt.Errorf("spider raw connection closed before handoff: %w", r.Context().Err())
			return
		}
		_, err := io.WriteString(w, "ok")
		if err == nil {
			err = http.NewResponseController(w).Flush()
		}
		returned <- err
	}))
	server.EnableHTTP2 = true
	server.TLS = &stdtls.Config{Certificates: []stdtls.Certificate{pair}, MinVersion: stdtls.VersionTLS13}
	server.StartTLS()
	defer server.Close()
	raw, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	tracked := &realityOwnedConn{Conn: raw}
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := UClient(tracked, &Config{Fingerprint: "chrome", ServerName: "localhost", PublicKey: private.PublicKey().Bytes(), ShortId: []byte{1, 2, 3, 4, 5, 6, 7, 8}, SpiderX: "/", SpiderY: make([]int64, 10)}, ctx, xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(server.Listener.Addr().(*net.TCPAddr).Port)))
	if conn != nil || err == nil || !strings.Contains(err.Error(), "processed invalid connection") {
		t.Fatalf("wrong fallback result: conn=%v err=%v", conn, err)
	}
	if count := tracked.closes.Load(); count != 0 {
		t.Fatalf("transferred spider raw closed %d times", count)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("spider request did not arrive", ctx.Err())
	}
	close(release)
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("spider response did not finish", ctx.Err())
	}
}
