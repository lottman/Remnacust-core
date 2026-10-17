package dns

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/apernet/quic-go"
)

func TestRetirementClosesEstablishedLocalQUICConnection(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	data, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{data}, PrivateKey: key}}, NextProtos: []string{NextProtoDQ}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// The certificate is generated for this isolated loopback fixture only.
	client, err := quic.DialAddr(ctx, listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{NextProtoDQ}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseWithError(0, "")
	peer, err := listener.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	address, _ := url.Parse("quic+local://" + listener.Addr().String())
	server, err := NewQUICNameServer(address, false, false, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	server.connection = client
	defer server.Close()
	if reused, err := server.getConnection(ctx); err != nil || reused != client {
		t.Fatal("live pool did not reuse owned connection", err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("owned QUIC connection remained active")
	}
	select {
	case <-peer.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("QUIC peer did not observe retirement")
	}
	if _, err := server.getConnection(ctx); err == nil {
		t.Fatal("retired QUIC pool accepted reuse")
	}
}
