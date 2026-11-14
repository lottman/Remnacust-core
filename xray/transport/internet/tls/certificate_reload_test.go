package tls

import (
	"bytes"
	"context"
	"crypto"
	gotls "crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
	xocsp "golang.org/x/crypto/ocsp"
)

type certificateReloadFixture struct {
	config    *Config
	ca        *cert.Certificate
	root      *x509.Certificate
	first     *cert.Certificate
	second    *cert.Certificate
	requests  chan *xocsp.Request
	responses chan []byte
	pool      *x509.CertPool
}

func newCertificateReloadFixture(t *testing.T) *certificateReloadFixture {
	t.Helper()
	f := &certificateReloadFixture{requests: make(chan *xocsp.Request, 16), responses: make(chan []byte), pool: x509.NewCertPool()}
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		if err != nil {
			t.Error(err)
			return
		}
		request, err := xocsp.ParseRequest(data)
		if err != nil {
			t.Error(err)
			return
		}
		select {
		case f.requests <- request:
		case <-done:
			return
		}
		select {
		case response := <-f.responses:
			w.Header().Set("Content-Type", "application/ocsp-response")
			if _, err := w.Write(response); err != nil {
				t.Error(err)
			}
		case <-done:
		case <-r.Context().Done():
		}
	}))
	directory := t.TempDir()
	entry := &Certificate{CertificatePath: filepath.Join(directory, "cert.pem"), KeyPath: filepath.Join(directory, "key.pem"), OcspStapling: 1}
	t.Cleanup(func() {
		os.Remove(entry.CertificatePath)
		os.Remove(entry.KeyPath)
		close(done)
		server.Close()
	})
	var err error
	f.ca, err = cert.Generate(nil, cert.Authority(true), cert.KeyUsage(x509.KeyUsageCertSign|x509.KeyUsageDigitalSignature))
	if err != nil {
		t.Fatal(err)
	}
	f.root, err = x509.ParseCertificate(f.ca.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	f.pool.AddCert(f.root)
	for _, output := range []**cert.Certificate{&f.first, &f.second} {
		*output, err = cert.Generate(f.ca, cert.DNSNames("reload.example.test"), func(c *x509.Certificate) { c.OCSPServer = []string{server.URL} })
		if err != nil {
			t.Fatal(err)
		}
	}
	entry.Certificate, entry.Key = f.chain(f.first)
	f.config = &Config{Certificate: []*Certificate{entry}}
	f.writeFiles(t, f.first)
	return f
}

func (f *certificateReloadFixture) chain(leaf *cert.Certificate) ([]byte, []byte) {
	certificate, key := leaf.ToPEM()
	ca, _ := f.ca.ToPEM()
	return append(certificate, ca...), key
}

func (f *certificateReloadFixture) writeFiles(t *testing.T, leaf *cert.Certificate) {
	t.Helper()
	certificate, key := f.chain(leaf)
	entry := f.configEntry()
	if err := os.WriteFile(entry.CertificatePath, certificate, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry.KeyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
}

func (f *certificateReloadFixture) configEntry() *Certificate { return f.config.Certificate[0] }

func (f *certificateReloadFixture) awaitRequest(t *testing.T, leaf *cert.Certificate) {
	t.Helper()
	parsed, err := x509.ParseCertificate(leaf.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-f.requests:
		if request.SerialNumber.Cmp(parsed.SerialNumber) != 0 {
			t.Fatal("OCSP refresh requested a different certificate")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OCSP reload worker did not reach the local responder")
	}
}

func (f *certificateReloadFixture) respond(t *testing.T, leaf *cert.Certificate, hours int) []byte {
	t.Helper()
	parsed, err := x509.ParseCertificate(leaf.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.ParsePKCS8PrivateKey(f.ca.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	response, err := xocsp.CreateResponse(f.root, f.root, xocsp.Response{Status: xocsp.Good, SerialNumber: parsed.SerialNumber, ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(time.Duration(hours) * time.Hour)}, key.(crypto.Signer))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := xocsp.ParseResponseForCert(response, parsed, f.root); err != nil {
		t.Fatal(err)
	}
	select {
	case f.responses <- response:
	case <-time.After(5 * time.Second):
		t.Fatal("OCSP responder did not accept the fixture response")
	}
	return response
}

func certificateReloadGet(t *testing.T, config *gotls.Config) *gotls.Certificate {
	t.Helper()
	certificate, err := config.GetCertificate(&gotls.ClientHelloInfo{ServerName: "reload.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func certificateReloadAwait(t *testing.T, config *gotls.Config, expected []byte) *gotls.Certificate {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		certificate := certificateReloadGet(t, config)
		if bytes.Equal(certificate.OCSPStaple, expected) {
			return certificate
		}
		select {
		case <-deadline.C:
			t.Fatal("updated OCSP certificate was not published")
		case <-tick.C:
		}
	}
}

func certificateReloadHandshake(config *gotls.Config, roots *x509.CertPool) (gotls.ConnectionState, error) {
	return certificateReloadHandshakeForName(config, roots, "reload.example.test")
}

func certificateReloadHandshakeForName(config *gotls.Config, roots *x509.CertPool, name string) (gotls.ConnectionState, error) {
	serverRaw, clientRaw := net.Pipe()
	defer serverRaw.Close()
	defer clientRaw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := gotls.Server(serverRaw, config)
	client := gotls.Client(clientRaw, &gotls.Config{RootCAs: roots, ServerName: name})
	serverResult := make(chan error, 1)
	go func() { serverResult <- server.HandshakeContext(ctx) }()
	err := client.HandshakeContext(ctx)
	if serverErr := <-serverResult; err == nil {
		err = serverErr
	}
	return client.ConnectionState(), err
}

func TestCertificateReloadOCSPSnapshots(t *testing.T) {
	f := newCertificateReloadFixture(t)
	config := f.config.GetTLSConfig()
	f.awaitRequest(t, f.first)
	initial := certificateReloadGet(t, config)
	firstResponse := f.respond(t, f.first, 1)
	first := certificateReloadAwait(t, config, firstResponse)
	if len(initial.OCSPStaple) != 0 || initial == first {
		t.Error("OCSP refresh mutated a previously published certificate")
	}
	f.awaitRequest(t, f.first)
	var work sync.WaitGroup
	for range 8 {
		work.Go(func() {
			for range 4 {
				if _, err := certificateReloadHandshake(config, f.pool); err != nil {
					t.Error(err)
				}
			}
		})
	}
	secondResponse := f.respond(t, f.first, 2)
	second := certificateReloadAwait(t, config, secondResponse)
	work.Wait()
	if !bytes.Equal(first.OCSPStaple, firstResponse) || first == second {
		t.Error("subsequent OCSP refresh mutated an in-flight certificate snapshot")
	}
	state, err := certificateReloadHandshake(config, f.pool)
	if err != nil || !bytes.Equal(state.OCSPResponse, secondResponse) {
		t.Fatalf("verified TLS handshake did not receive the current OCSP response: %v", err)
	}
}

func TestCertificateReloadRotation(t *testing.T) {
	f := newCertificateReloadFixture(t)
	entry := f.configEntry()
	originalCertificate, originalKey := slices.Clone(entry.Certificate), slices.Clone(entry.Key)
	f.writeFiles(t, f.second)
	config := f.config.GetTLSConfig()
	f.awaitRequest(t, f.second)
	initial := certificateReloadGet(t, config)
	if !bytes.Equal(initial.Certificate[0], f.first.Certificate) {
		t.Fatal("reload published a certificate before OCSP processing finished")
	}
	response := f.respond(t, f.second, 1)
	updated := certificateReloadAwait(t, config, response)
	if !bytes.Equal(updated.Certificate[0], f.second.Certificate) || !bytes.Equal(initial.Certificate[0], f.first.Certificate) || len(initial.OCSPStaple) != 0 {
		t.Fatal("file rotation did not publish an independent, coherent certificate/key/staple snapshot")
	}
	if !bytes.Equal(entry.Certificate, originalCertificate) || !bytes.Equal(entry.Key, originalKey) {
		t.Error("reload mutated the source configuration shared by other TLS consumers")
	}
	state, err := certificateReloadHandshake(config, f.pool)
	if err != nil || len(state.PeerCertificates) == 0 || !bytes.Equal(state.PeerCertificates[0].Raw, f.second.Certificate) || !bytes.Equal(state.OCSPResponse, response) {
		t.Fatalf("verified TLS handshake did not use the rotated key/certificate/OCSP: %v", err)
	}
}

func TestCertificateReloadRejectsMismatchedKeyAndRecovers(t *testing.T) {
	f := newCertificateReloadFixture(t)
	_, wrongKey := f.chain(f.second)
	if err := os.WriteFile(f.configEntry().KeyPath, wrongKey, 0600); err != nil {
		t.Fatal(err)
	}
	config := f.config.GetTLSConfig()
	f.awaitRequest(t, f.first)
	initial := certificateReloadGet(t, config)
	if !bytes.Equal(initial.Certificate[0], f.first.Certificate) {
		t.Fatal("mismatched file reload replaced the last valid certificate")
	}
	f.writeFiles(t, f.second)
	firstResponse := f.respond(t, f.first, 1)
	first := certificateReloadAwait(t, config, firstResponse)
	f.awaitRequest(t, f.second)
	response := f.respond(t, f.second, 2)
	updated := certificateReloadAwait(t, config, response)
	if !bytes.Equal(updated.Certificate[0], f.second.Certificate) || !bytes.Equal(first.Certificate[0], f.first.Certificate) || !bytes.Equal(first.OCSPStaple, firstResponse) {
		t.Fatal("reload recovery did not retain independent valid snapshots")
	}
	if _, err := certificateReloadHandshake(config, f.pool); err != nil {
		t.Fatal(err)
	}
}

func TestCertificateReloadOneTimeLoading(t *testing.T) {
	f := newCertificateReloadFixture(t)
	f.configEntry().OneTimeLoading = true
	f.writeFiles(t, f.second)
	config := f.config.GetTLSConfig()
	selected := certificateReloadGet(t, config)
	if !bytes.Equal(selected.Certificate[0], f.first.Certificate) || len(selected.OCSPStaple) != 0 {
		t.Fatal("one-time loading reloaded files or stapled OCSP")
	}
	if _, err := certificateReloadHandshake(config, f.pool); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.requests:
		t.Fatal("one-time loading started an OCSP worker")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestCertificateReloadSelectionAndExportedBuilder(t *testing.T) {
	var entries []*Certificate
	for _, name := range []string{"reload.example.test", "*.wild.example.test"} {
		generated, err := cert.Generate(nil, cert.CommonName(name), cert.DNSNames(name))
		if err != nil {
			t.Fatal(err)
		}
		entry := ParseCertificate(generated)
		entry.OneTimeLoading = true
		entries = append(entries, entry)
	}
	config := &Config{Certificate: entries}
	var built []*gotls.Certificate = config.BuildCertificates()
	if len(built) != 2 {
		t.Fatalf("exported certificate builder lost valid certificates: %d", len(built))
	}
	for _, reject := range []bool{false, true} {
		config.RejectUnknownSni = reject
		tlsConfig := config.GetTLSConfig()
		for _, test := range []struct {
			name string
			want int
		}{{"RELOAD.EXAMPLE.TEST", 0}, {"a.wild.example.test", 1}, {"unrelated.example.test", -1}, {"", -1}} {
			selected, err := tlsConfig.GetCertificate(&gotls.ClientHelloInfo{ServerName: test.name})
			if test.want < 0 && reject {
				if selected != nil || err == nil {
					t.Errorf("unknown SNI was accepted: %q", test.name)
				}
				continue
			}
			index := test.want
			if index < 0 {
				index = 0
			}
			if err != nil || selected == nil || !bytes.Equal(selected.Certificate[0], built[index].Certificate[0]) {
				t.Errorf("SNI selection changed: name=%q, reject=%t, error=%v", test.name, reject, err)
			}
		}
	}
}

func TestCertificateReloadExpirationDoesNotMutateLeaf(t *testing.T) {
	for _, test := range []struct {
		name    string
		expires time.Time
		want    bool
	}{{"valid", time.Now().Add(time.Hour), false}, {"expired", time.Now().Add(-time.Minute), true}, {"near-expiry", time.Now().Add(time.Minute), true}} {
		t.Run(test.name, func(t *testing.T) {
			generated, err := cert.Generate(nil, cert.NotAfter(test.expires))
			if err != nil {
				t.Fatal(err)
			}
			published := &gotls.Certificate{Certificate: [][]byte{generated.Certificate}}
			var readers sync.WaitGroup
			for range 16 {
				readers.Go(func() {
					if actual := isCertificateExpired(published); actual != test.want {
						t.Errorf("expiration changed: got=%t, want=%t", actual, test.want)
					}
				})
			}
			readers.Wait()
			if published.Leaf != nil {
				t.Fatal("expiration check mutated the published certificate leaf")
			}
		})
	}
}

func TestCertificateReloadCAAndCacheExpiration(t *testing.T) {
	f := newCertificateReloadFixture(t)
	entry := f.configEntry()
	entry.Certificate, entry.Key = f.ca.ToPEM()
	entry.Usage = Certificate_AUTHORITY_ISSUE
	entry.BuildChain = true
	if err := os.WriteFile(entry.CertificatePath, entry.Certificate, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry.KeyPath, entry.Key, 0600); err != nil {
		t.Fatal(err)
	}
	stores := f.config.getCustomCA()
	if len(stores) != 1 {
		t.Fatal("CA builder did not retain the issuing authority")
	}
	initial := stores[0].Load()
	rotated, err := cert.Generate(nil, cert.Authority(true), cert.KeyUsage(x509.KeyUsageCertSign|x509.KeyUsageDigitalSignature))
	if err != nil {
		t.Fatal(err)
	}
	newCertificate, newKey := rotated.ToPEM()
	if err := os.WriteFile(entry.CertificatePath, newCertificate, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry.KeyPath, newKey, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !bytes.Equal(stores[0].Load().Key, newKey) {
		select {
		case <-deadline.C:
			t.Fatal("rotated CA was not published")
		case <-tick.C:
		}
	}
	if !bytes.Equal(initial.Certificate, entry.Certificate) || !bytes.Equal(initial.Key, entry.Key) || initial == stores[0].Load() {
		t.Fatal("CA reload mutated the source config or an existing snapshot")
	}
	expired, err := cert.Generate(f.ca, cert.DNSNames("reload.example.test"), cert.NotAfter(time.Now().Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM, keyPEM := expired.ToPEM()
	cached, err := gotls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cache := &gotls.Config{Certificates: []gotls.Certificate{cached}}
	cache.BuildNameToCertificate()
	cache.GetCertificate = getGetCertificateFunc(cache, stores)
	issued := certificateReloadGet(t, cache)
	if len(cache.Certificates) != 1 || len(issued.Certificate) != 2 || !bytes.Equal(issued.Certificate[1], rotated.Certificate) {
		t.Fatal("CA rotation did not preserve chain building and old-cache expiration")
	}
	if !bytes.Equal(cache.Certificates[0].Certificate[0], cached.Certificate[0]) || !bytes.Equal(cache.NameToCertificate["reload.example.test"].Certificate[0], cached.Certificate[0]) {
		t.Error("CA cache pruning changed the published TLS config or fallback map")
	}
	if again := certificateReloadGet(t, cache); again != issued {
		t.Fatal("valid issued certificate was not cached")
	}
	root, err := x509.ParseCertificate(rotated.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	if _, err := issued.Leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "reload.example.test"}); err != nil {
		t.Fatalf("issued certificate was not signed by the rotated CA: %v", err)
	}
}

func certificateReloadIssuingConfig(t *testing.T) (*gotls.Config, *x509.CertPool) {
	t.Helper()
	ca, err := cert.Generate(nil, cert.Authority(true), cert.KeyUsage(x509.KeyUsageCertSign|x509.KeyUsageDigitalSignature))
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(ca.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	entry := ParseCertificate(ca)
	entry.Usage = Certificate_AUTHORITY_ISSUE
	entry.BuildChain = true
	return (&Config{Certificate: []*Certificate{entry}}).GetTLSConfig(), pool
}

func TestCertificateReloadCAConcurrentHandshakesKeepConfigImmutable(t *testing.T) {
	config, roots := certificateReloadIssuingConfig(t)
	if _, err := certificateReloadHandshakeForName(config, roots, "warm.example.test"); err != nil {
		t.Fatal(err)
	}
	if len(config.Certificates) != 0 || config.NameToCertificate != nil {
		t.Error("CA handshake mutated the already-published TLS configuration")
	}
	stop := make(chan struct{})
	var observer sync.WaitGroup
	observer.Go(func() {
		for {
			snapshot := config.Clone()
			if len(snapshot.Certificates) != 0 || snapshot.NameToCertificate != nil {
				t.Error("CA issuance changed config fields read by crypto/tls outside the certificate-cache lock")
				return
			}
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Microsecond):
			}
		}
	})
	start := make(chan struct{})
	var handshakes sync.WaitGroup
	for worker := range 8 {
		handshakes.Go(func() {
			<-start
			for request := range 4 {
				name := fmt.Sprintf("ca-%d-%d.example.test", worker, request)
				if _, err := certificateReloadHandshakeForName(config, roots, name); err != nil {
					t.Error(err)
				}
			}
		})
	}
	close(start)
	handshakes.Wait()
	close(stop)
	observer.Wait()
	if len(config.Certificates) != 0 || config.NameToCertificate != nil {
		t.Error("concurrent CA handshakes published mutable config cache state")
	}
}

func TestCertificateReloadCANoSNIFallback(t *testing.T) {
	config, roots := certificateReloadIssuingConfig(t)
	first := certificateReloadGet(t, config)
	if _, err := config.GetCertificate(&gotls.ClientHelloInfo{ServerName: "other.example.test"}); err != nil {
		t.Fatal(err)
	}
	serverRaw, clientRaw := net.Pipe()
	defer serverRaw.Close()
	defer clientRaw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := gotls.Server(serverRaw, config)
	client := utls.UClient(clientRaw, &utls.Config{RootCAs: roots, ServerName: "reload.example.test"}, utls.HelloChrome_Auto)
	if err := client.RemoveSNIExtension(); err != nil {
		t.Fatal(err)
	}
	serverResult := make(chan error, 1)
	go func() { serverResult <- server.HandshakeContext(ctx) }()
	err := client.HandshakeContext(ctx)
	if serverErr := <-serverResult; err == nil {
		err = serverErr
	}
	if err != nil {
		t.Fatalf("verified CA TLS handshake without SNI failed: %v", err)
	}
	state := client.ConnectionState()
	if len(state.PeerCertificates) == 0 || !bytes.Equal(state.PeerCertificates[0].Raw, first.Certificate[0]) {
		t.Fatal("CA no-SNI fallback did not retain the first compatible cached certificate")
	}
}
