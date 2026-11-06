package xerahttp

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	stdnet "net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/apernet/quic-go/http3"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
)

type packetHTTPBody struct {
	io.ReadCloser
	entered chan struct{}
	once    sync.Once
}

type packetHTTPDeadlineWriter struct {
	http.ResponseWriter
	mu        sync.Mutex
	deadlines []time.Time
}

func (w *packetHTTPDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	w.mu.Lock()
	w.deadlines = append(w.deadlines, deadline)
	w.mu.Unlock()
	return http.NewResponseController(w.ResponseWriter).SetReadDeadline(deadline)
}

func TestSuccessfulPacketBodyClearsNativeHTTP1DeadlineAndReusesConnection(t *testing.T) {
	h := testAdmissionHandler(t, "POST")
	peers := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writer := &packetHTTPDeadlineWriter{ResponseWriter: w}
		h.ServeHTTP(writer, r)
		if len(writer.deadlines) != 2 || writer.deadlines[0].IsZero() || !writer.deadlines[1].IsZero() {
			t.Error("native packet read deadline was not set and cleared")
		}
		peers <- r.RemoteAddr
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	client.Transport.(*http.Transport).MaxConnsPerHost = 1
	for i := range 2 {
		request, _ := http.NewRequest("POST", server.URL+"/api/", nil)
		request.Host = "example.com"
		if err := h.config.FillPacketRequest(request, "reuse", string(rune('0'+i)), buf.MergeBytes(nil, []byte("x"))); err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("packet returned %d", response.StatusCode)
		}
	}
	if first, second := <-peers, <-peers; first != second {
		t.Fatalf("HTTP/1 connection was not reused: %s, %s", first, second)
	}
}

func (b *packetHTTPBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	return b.ReadCloser.Read(p)
}

func TestPacketCancellationUsesRequestLocalHTTPDeadlines(t *testing.T) {
	for _, protocol := range []string{"http1", "h2", "h3"} {
		t.Run(protocol, func(t *testing.T) {
			h := testAdmissionHandler(t, "POST")
			entered, returned, finishLive := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var finishOnce sync.Once
			finish := func() { finishOnce.Do(func() { close(finishLive) }) }
			t.Cleanup(finish)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/live" {
					w.Write([]byte("start"))
					w.(http.Flusher).Flush()
					select {
					case <-finishLive:
						w.Write([]byte("end"))
					case <-r.Context().Done():
					}
					return
				}
				r.Body = &packetHTTPBody{ReadCloser: r.Body, entered: entered}
				h.ServeHTTP(w, r)
				close(returned)
			})
			var client *http.Client
			var origin string
			if protocol != "h3" {
				server := httptest.NewUnstartedServer(handler)
				if protocol == "h2" {
					server.EnableHTTP2 = true
					server.StartTLS()
				} else {
					server.Start()
				}
				t.Cleanup(server.Close)
				client, origin = server.Client(), server.URL
				if protocol == "h2" {
					client.Transport.(*http.Transport).MaxConnsPerHost = 1
				}
			} else {
				certificate, _ := cert.MustGenerate(nil, cert.CommonName("localhost"), cert.DNSNames("localhost"))
				pem, key := certificate.ToPEM()
				pair, err := tls.X509KeyPair(pem, key)
				if err != nil {
					t.Fatal(err)
				}
				roots := x509.NewCertPool()
				roots.AppendCertsFromPEM(pem)
				listener, err := stdnet.ListenPacket("udp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				server := &http3.Server{Handler: handler, TLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}, NextProtos: []string{"h3"}}}
				served := make(chan struct{})
				go func() { defer close(served); server.Serve(listener) }()
				t.Cleanup(func() {
					server.Close()
					listener.Close()
					select {
					case <-served:
					case <-time.After(2 * time.Second):
						t.Error("HTTP/3 server did not stop")
					}
				})
				transport := &http3.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13}}
				t.Cleanup(func() { transport.Close() })
				client = &http.Client{Transport: transport}
				origin = "https://" + listener.LocalAddr().String()
			}
			client.Timeout = 4 * time.Second
			var live *http.Response
			if protocol != "http1" {
				var err error
				live, err = client.Get(origin + "/live")
				if err != nil {
					t.Fatal(err)
				}
				defer live.Body.Close()
				want := 2
				if protocol == "h3" {
					want = 3
				}
				if live.ProtoMajor != want {
					t.Fatalf("wrong HTTP protocol: %s", live.Proto)
				}
				if _, err := io.ReadFull(live.Body, make([]byte, 5)); err != nil {
					t.Fatal(err)
				}
			}
			request, err := http.NewRequest("POST", origin+"/api/", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Host = "example.com"
			if err := h.config.FillPacketRequest(request, "stalled", "0", buf.MergeBytes(nil, []byte("x"))); err != nil {
				t.Fatal(err)
			}
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			request.Body, request.ContentLength = reader, 1
			clientDone := make(chan struct{})
			go func() {
				defer close(clientDone)
				response, _ := client.Do(request)
				if response != nil {
					response.Body.Close()
				}
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("packet body was not read")
			}
			h.upsertSession("stalled").uploadQueue.Close()
			select {
			case <-returned:
			case <-time.After(2 * time.Second):
				t.Fatal("cancelled native body read remained blocked")
			}
			writer.Close()
			select {
			case <-clientDone:
			case <-time.After(2 * time.Second):
				t.Fatal("upload client did not stop")
			}
			if h.budget.used.Load() != 0 || h.pendingRequests.Load() != 0 {
				t.Fatal("cancelled request retained admission")
			}
			finish()
			if live != nil {
				remaining, err := io.ReadAll(live.Body)
				if err != nil || string(remaining) != "end" {
					t.Fatalf("sibling stream was interrupted: %q %v", remaining, err)
				}
			}
		})
	}
}
