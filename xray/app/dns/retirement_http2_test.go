package dns

import (
	"context"
	"io"
	stdnet "net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	featureDNS "github.com/xtls/xray-core/features/dns"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/http2"
)

func TestRetirementClosesLiveHTTP2DNSQueryAndPool(t *testing.T) {
	listener, err := stdnet.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var access sync.Mutex
	connections := make(map[stdnet.Conn]bool)
	t.Cleanup(func() {
		_ = listener.Close()
		access.Lock()
		defer access.Unlock()
		for conn := range connections {
			_ = conn.Close()
		}
	})
	started, cancelled := make(chan struct{}), make(chan struct{})
	var requests atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		var query dnsmessage.Message
		if query.Unpack(data) != nil || len(query.Questions) != 1 {
			w.WriteHeader(400)
			return
		}
		if query.Questions[0].Name.String() == "pending.invalid." {
			close(started)
			<-r.Context().Done()
			close(cancelled)
			return
		}
		query.Header.Response = true
		query.Header.RecursionAvailable = true
		query.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: query.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 41}}}}
		answer, err := query.Pack()
		if err != nil {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(answer)
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			access.Lock()
			connections[conn] = true
			access.Unlock()
			go func() {
				defer func() { access.Lock(); delete(connections, conn); access.Unlock() }()
				(&http2.Server{}).ServeConn(conn, &http2.ServeConnOpts{Handler: handler})
			}()
		}
	}()
	address, _ := url.Parse("https+local://" + listener.Addr().String() + "/dns-query")
	server := NewDoHNameServer(address, nil, true, false, false, 0, nil)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, _, err := server.QueryIP(ctx, "ready.invalid", featureDNS.IPOption{IPv4Enable: true})
	if err != nil || len(ips) != 1 || ips[0].String() != "192.0.2.41" {
		t.Fatal("local HTTP/2 DNS did not answer", ips, err)
	}
	result := make(chan error, 1)
	go func() {
		_, _, err := server.QueryIP(ctx, "pending.invalid", featureDNS.IPOption{IPv4Enable: true})
		result <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("pending HTTP/2 query did not start")
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("retired HTTP/2 query succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP/2 query did not terminate")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("HTTP/2 server stream remained active")
	}
	before := requests.Load()
	if _, _, err := server.QueryIP(ctx, "after.invalid", featureDNS.IPOption{IPv4Enable: true}); err == nil {
		t.Fatal("retired HTTP/2 server accepted query")
	}
	if requests.Load() != before {
		t.Fatal("retired HTTP/2 pool sent another request")
	}
	server.connectionsAccess.Lock()
	count := len(server.connections)
	server.connectionsAccess.Unlock()
	if count != 0 {
		t.Fatal("retired HTTP/2 pool retained sockets")
	}
}
