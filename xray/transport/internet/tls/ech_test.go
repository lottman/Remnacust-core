package tls

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	gotls "crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	utls "github.com/refraction-networking/utls"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
	"golang.org/x/crypto/cryptobyte"
)

func TestECHDial(t *testing.T) {
	const publicName = "public.example.test"
	const secretName = "secret.example.test"
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var configBuilder cryptobyte.Builder
	configBuilder.AddUint16(0xfe0d)
	configBuilder.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
		b.AddUint8(1)
		b.AddUint16(0x20)
		b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) { b.AddBytes(key.PublicKey().Bytes()) })
		b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) { b.AddUint16(1); b.AddUint16(1) })
		b.AddUint8(0)
		b.AddUint8LengthPrefixed(func(b *cryptobyte.Builder) { b.AddBytes([]byte(publicName)) })
		b.AddUint16(0)
	})
	echConfig, err := configBuilder.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	configs := bytes.Repeat(echConfig, 8)
	echList := append(binary.BigEndian.AppendUint16(nil, uint16(len(configs))), configs...)
	certificate, err := cert.Generate(nil, cert.Authority(true), cert.KeyUsage(x509.KeyUsageCertSign|x509.KeyUsageDigitalSignature), cert.DNSNames(secretName, publicName))
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := certificate.ToPEM()
	pair, err := gotls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("test certificate could not be trusted")
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || !r.TLS.ECHAccepted {
			http.Error(w, "ECH was not accepted", http.StatusBadRequest)
			return
		}
		io.WriteString(w, "encrypted client hello accepted")
	}))
	server.TLS = &gotls.Config{Certificates: []gotls.Certificate{pair}, MinVersion: gotls.VersionTLS13, EncryptedClientHelloKeys: []gotls.EncryptedClientHelloKey{{Config: echConfig, PrivateKey: key.Bytes(), SendAsRetry: true}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var queries atomic.Int64
	dnsServer := &dns.Server{PacketConn: packet, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		queries.Add(1)
		reply := new(dns.Msg)
		reply.SetReply(r)
		reply.Answer = []dns.RR{&dns.HTTPS{SVCB: dns.SVCB{Hdr: dns.RR_Header{Name: dns.Fqdn(publicName), Rrtype: dns.TypeHTTPS, Class: dns.ClassINET, Ttl: 60}, Priority: 1, Target: ".", Value: []dns.SVCBKeyValue{&dns.SVCBECHConfig{ECH: echList}}}}}
		if err := w.WriteMsg(reply); err != nil {
			t.Error(err)
		}
	})}
	started := make(chan struct{})
	dnsServer.NotifyStartedFunc = func() { close(started) }
	go func() {
		if err := dnsServer.ActivateAndServe(); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		dnsServer.ShutdownContext(ctx)
	})
	<-started
	dnsURL := "udp://" + packet.LocalAddr().String()
	config := &Config{ServerName: secretName, EchConfigList: publicName + "+" + dnsURL}
	cacheKey := ECHCacheKey(dnsURL, publicName, nil)
	t.Cleanup(func() { GlobalECHConfigCache.Delete(cacheKey) })
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			tlsConfig := config.GetTLSConfig()
			tlsConfig.RootCAs = pool
			tlsConfig.NextProtos = []string{"http/1.1"}
			transport := &http.Transport{TLSClientConfig: tlsConfig}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
			response, err := client.Get(server.URL)
			if err != nil {
				t.Error(err)
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Error(err)
				return
			}
			if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "encrypted client hello accepted") {
				t.Errorf("ECH was not negotiated: %d %s", response.StatusCode, body)
			}
		})
	}
	wg.Wait()
	for _, bounds := range []struct {
		name    string
		minimum uint16
		maximum uint16
		reject  bool
	}{{"default", 0, 0, false}, {"only13", gotls.VersionTLS13, gotls.VersionTLS13, false}, {"maximum12", 0, gotls.VersionTLS12, true}} {
		t.Run(fmt.Sprintf("utls-versions-%s", bounds.name), func(t *testing.T) {
			tlsConfig := config.GetTLSConfig()
			tlsConfig.RootCAs = pool
			tlsConfig.NextProtos = []string{"http/1.1"}
			tlsConfig.MinVersion, tlsConfig.MaxVersion = bounds.minimum, bounds.maximum
			transport := &http.Transport{DialTLSContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				var dialer net.Dialer
				raw, err := dialer.DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				client := UClient(raw, tlsConfig, &utls.HelloChrome_155).(*UConn)
				if err := client.WebsocketHandshakeContext(ctx); err != nil {
					client.Close()
					return nil, err
				}
				return client, nil
			}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
			response, err := client.Get(server.URL)
			if bounds.reject {
				if response != nil {
					response.Body.Close()
				}
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), "version") {
					t.Fatalf("ECH ignored incompatible TLS version bound: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "encrypted client hello accepted") {
				t.Fatalf("uTLS ECH failed: status=%d error=%v", response.StatusCode, err)
			}
		})
	}
	cached, ok := GlobalECHConfigCache.Load(cacheKey)
	if !ok {
		t.Fatal("ECH config not cached")
	}
	if !cached.UpdateLock.TryLock() {
		t.Fatal("ECH cache update lock leaked")
	}
	cached.UpdateLock.Unlock()
	if cached.configRecord.Load() == nil || queries.Load() != 1 {
		t.Fatalf("concurrent first use should query DNS once; queries=%d", queries.Load())
	}
}

func TestECHDialFail(t *testing.T) {
	server := echStartUDPDNS(t, "127.0.0.1:0", func(w dns.ResponseWriter, query *dns.Msg) {
		if err := w.WriteMsg(new(dns.Msg).SetRcode(query, dns.RcodeNameError)); err != nil {
			t.Error(err)
		}
	})
	echClearDNSCache(t, server, "unavailable.example.test", nil)
	config := &Config{
		ServerName:    "unavailable.example.test",
		EchConfigList: server,
	}
	tlsConfig := config.GetTLSConfig()
	ApplyECH(config, tlsConfig)
	if !slices.Equal(tlsConfig.EncryptedClientHelloConfigList, []byte{1, 1, 4, 5, 1, 4}) {
		t.Error("ECH config should be invalid when query failed", " but got ", tlsConfig.EncryptedClientHelloConfigList)
	}
}
