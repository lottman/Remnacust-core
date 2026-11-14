package tls

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	stdtls "crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
)

type tlsVersionObservation struct {
	version uint16
	offered []uint16
	err     error
}

func testVersionPolicy(t *testing.T, fingerprint utls.ClientHelloID, minimum, maximum, serverMin, serverMax uint16, entry string) tlsVersionObservation {
	t.Helper()
	certificate, _ := cert.MustGenerate(nil, cert.CommonName("localhost"), cert.DNSNames("localhost"))
	pem, key := certificate.ToPEM()
	pair, err := stdtls.X509KeyPair(pem, key)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	var ciphers []uint16
	if serverMin < stdtls.VersionTLS12 {
		privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		template, err := x509.ParseCertificate(certificate.Certificate)
		if err != nil {
			t.Fatal(err)
		}
		template.SignatureAlgorithm = x509.SHA256WithRSA
		template.PublicKey = &privateKey.PublicKey
		template.PublicKeyAlgorithm = x509.RSA
		der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
		if err != nil {
			t.Fatal(err)
		}
		root, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		roots.AddCert(root)
		pair = stdtls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey}
		ciphers = []uint16{stdtls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA}
	} else {
		roots.AppendCertsFromPEM(pem)
	}
	var offered []uint16
	listener, err := stdtls.Listen("tcp", "127.0.0.1:0", &stdtls.Config{
		Certificates: []stdtls.Certificate{pair}, MinVersion: serverMin, MaxVersion: serverMax,
		CipherSuites: ciphers,
		GetConfigForClient: func(info *stdtls.ClientHelloInfo) (*stdtls.Config, error) {
			offered = append([]uint16(nil), info.SupportedVersions...)
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		peer.SetDeadline(time.Now().Add(3 * time.Second))
		if err := peer.(*stdtls.Conn).Handshake(); err == nil {
			io.Copy(io.Discard, peer)
		}
	}()
	raw, err := net.DialTimeout("tcp", listener.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(3 * time.Second))
	config := &stdtls.Config{ServerName: "localhost", RootCAs: roots, MinVersion: minimum, MaxVersion: maximum}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var version uint16
	if entry == "generic" {
		client := GeneraticUClient(raw, config)
		err = client.HandshakeContext(ctx)
		version = client.ConnectionState().Version
		client.Close()
	} else {
		client := UClient(raw, config, &fingerprint).(*UConn)
		switch entry {
		case "websocket":
			err = client.WebsocketHandshakeContext(ctx)
		case "implicit":
			_, err = client.Write([]byte("payload"))
		default:
			err = client.HandshakeContext(ctx)
		}
		version = client.ConnectionState().Version
		client.Close()
	}
	raw.Close()
	<-done
	return tlsVersionObservation{version: version, offered: offered, err: err}
}

func TestUTLSVersionPolicyMatchesAdvertisedAndNegotiatedVersions(t *testing.T) {
	for _, fingerprint := range []utls.ClientHelloID{utls.HelloChrome_155, utls.HelloFirefox_148, utls.HelloChrome_58, utls.HelloGolang} {
		for _, scenario := range []struct {
			name      string
			minimum   uint16
			maximum   uint16
			serverMin uint16
			serverMax uint16
			want      uint16
		}{
			{name: "default", serverMin: stdtls.VersionTLS12, serverMax: stdtls.VersionTLS12, want: stdtls.VersionTLS12},
			{name: "minimum13-reject12", minimum: stdtls.VersionTLS13, serverMin: stdtls.VersionTLS12, serverMax: stdtls.VersionTLS12},
			{name: "minimum12-reject11", minimum: stdtls.VersionTLS12, serverMin: stdtls.VersionTLS11, serverMax: stdtls.VersionTLS11},
			{name: "maximum12", maximum: stdtls.VersionTLS12, serverMin: stdtls.VersionTLS12, serverMax: stdtls.VersionTLS13, want: stdtls.VersionTLS12},
			{name: "only12-reject13", minimum: stdtls.VersionTLS12, maximum: stdtls.VersionTLS12, serverMin: stdtls.VersionTLS13, serverMax: stdtls.VersionTLS13},
			{name: "inverted", minimum: stdtls.VersionTLS13, maximum: stdtls.VersionTLS12, serverMin: stdtls.VersionTLS12, serverMax: stdtls.VersionTLS13},
		} {
			t.Run(fingerprint.Client+fingerprint.Version+"/"+scenario.name, func(t *testing.T) {
				result := testVersionPolicy(t, fingerprint, scenario.minimum, scenario.maximum, scenario.serverMin, scenario.serverMax, "explicit")
				if scenario.want == 0 {
					if result.err == nil || errors.Is(result.err, context.DeadlineExceeded) {
						t.Fatalf("forbidden version accepted or timed out: %+v", result)
					}
				} else if result.err != nil || result.version != scenario.want {
					t.Fatalf("allowed TLS version failed: %+v", result)
				}
				for _, version := range result.offered {
					if version&0x0f0f == 0x0a0a && byte(version>>8) == byte(version) {
						continue
					}
					if fingerprint != utls.HelloChrome_58 && scenario.minimum != 0 && version < scenario.minimum || scenario.maximum != 0 && version > scenario.maximum {
						t.Fatalf("ClientHello offered version outside policy: %#x", version)
					}
				}
			})
		}
	}
}

func TestUTLSVersionPolicyCoversImplicitWebsocketAndGenericEntryPoints(t *testing.T) {
	for _, entry := range []string{"implicit", "websocket", "generic"} {
		for _, version := range []uint16{stdtls.VersionTLS12, stdtls.VersionTLS13} {
			t.Run(fmt.Sprintf("%s/0x%04x", entry, version), func(t *testing.T) {
				result := testVersionPolicy(t, utls.HelloChrome_155, version, version, stdtls.VersionTLS12, stdtls.VersionTLS13, entry)
				if result.err != nil || result.version != version {
					t.Fatalf("allowed version failed: %+v", result)
				}
			})
		}
	}
}

func TestUTLSVersionPolicyPreservesExplicitLegacyMaximum(t *testing.T) {
	result := testVersionPolicy(t, utls.HelloChrome_58, stdtls.VersionTLS11, stdtls.VersionTLS11, stdtls.VersionTLS11, stdtls.VersionTLS12, "explicit")
	if result.err != nil || result.version != stdtls.VersionTLS11 {
		t.Fatalf("legacy maximum version lost: %+v, error=%v", result, result.err)
	}
}

func TestUTLSVersionPolicyPreservesRequestedWebsocketALPN(t *testing.T) {
	for _, fingerprint := range []utls.ClientHelloID{utls.HelloChrome_58, utls.HelloChrome_155} {
		for _, websocket := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", fingerprint.Version, websocket), func(t *testing.T) {
				certificate, _ := cert.MustGenerate(nil, cert.CommonName("localhost"), cert.DNSNames("localhost"))
				pem, key := certificate.ToPEM()
				pair, err := stdtls.X509KeyPair(pem, key)
				if err != nil {
					t.Fatal(err)
				}
				roots := x509.NewCertPool()
				roots.AppendCertsFromPEM(pem)
				listener, err := stdtls.Listen("tcp", "127.0.0.1:0", &stdtls.Config{Certificates: []stdtls.Certificate{pair}, MinVersion: stdtls.VersionTLS12, MaxVersion: stdtls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}})
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				result := make(chan string, 1)
				go func() {
					peer, err := listener.Accept()
					if err != nil {
						result <- err.Error()
						return
					}
					defer peer.Close()
					peer.SetDeadline(time.Now().Add(3 * time.Second))
					secured := peer.(*stdtls.Conn)
					if err := secured.Handshake(); err != nil {
						result <- err.Error()
						return
					}
					result <- secured.ConnectionState().NegotiatedProtocol
				}()
				raw, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				config := &stdtls.Config{ServerName: "localhost", RootCAs: roots, MinVersion: stdtls.VersionTLS12, MaxVersion: stdtls.VersionTLS12, NextProtos: []string{"http/1.1"}}
				client := UClient(raw, config, &fingerprint).(*UConn)
				defer client.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if websocket {
					err = client.WebsocketHandshakeContext(ctx)
				} else {
					err = client.HandshakeContext(ctx)
				}
				if err != nil {
					t.Fatal(err)
				}
				want := "h2"
				if websocket {
					want = "http/1.1"
				}
				if got := <-result; got != want || client.NegotiatedProtocol() != want {
					t.Fatalf("ALPN=%q/%q, want %q", got, client.NegotiatedProtocol(), want)
				}
				if len(config.NextProtos) != 1 || config.NextProtos[0] != "http/1.1" {
					t.Fatal("source config ALPN mutated")
				}
			})
		}
	}
}
