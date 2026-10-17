package dns

import (
	"context"
	"io"
	stdnet "net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/task"
	featureDNS "github.com/xtls/xray-core/features/dns"
	"golang.org/x/net/dns/dnsmessage"
)

func TestRetirementStopsUDPPendingRequestTaskAndLateRetry(t *testing.T) {
	server := NewClassicNameServer(net.UDPDestination(net.LocalHostIP, 53), nil, false, false, 0, nil)
	defer server.Close()
	var runs atomic.Int64
	server.requestsCleanup = &task.Periodic{Interval: time.Hour, Execute: func() error { runs.Add(1); return nil }}
	request := func(id uint16) *udpDnsRequest {
		return &udpDnsRequest{dnsRequest: dnsRequest{msg: &dnsmessage.Message{Header: dnsmessage.Header{ID: id}}}}
	}
	if !server.addPendingRequest(request(1)) || runs.Load() != 1 {
		t.Fatal("UDP request cleanup did not start")
	}
	var workers sync.WaitGroup
	for n := range 32 {
		workers.Go(func() { server.addPendingRequest(request(uint16(n + 2))) })
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	before := runs.Load()
	if server.addPendingRequest(request(100)) {
		t.Fatal("late UDP retry was accepted")
	}
	if runs.Load() != before {
		t.Fatal("late UDP retry restarted cleanup task")
	}
	if len(server.requests) != 0 {
		t.Fatal("retired UDP server retained pending requests")
	}
}

type retirementPendingServer struct {
	cache   *CacheController
	started chan struct{}
	once    sync.Once
	calls   atomic.Int64
}

func TestRetirementLocalFallbackKeepsAUsableRequestDeadline(t *testing.T) {
	client := NewLocalDNSClient(featureDNS.IPOption{IPv4Enable: true})
	defer client.server.(*LocalNameServer).Close()
	ips, _, err := client.QueryIP(context.Background(), "127.0.0.1", featureDNS.IPOption{IPv4Enable: true})
	if err != nil || len(ips) != 1 || !ips[0].Equal(net.LocalHostIP.IP()) {
		t.Fatal("default localhost fallback is cancelled before lookup", ips, err)
	}
}

func (s *retirementPendingServer) getCacheController() *CacheController { return s.cache }
func (s *retirementPendingServer) sendQuery(context.Context, chan<- error, string, featureDNS.IPOption) {
	s.calls.Add(1)
	s.once.Do(func() { close(s.started) })
}

func TestRetirementCancelsPendingDNSAndRejectsLateCacheUpdates(t *testing.T) {
	cache := NewCacheController("retirement-test", false, false, 0)
	defer cache.Close()
	var runs atomic.Int64
	cache.cacheCleanup = &task.Periodic{Interval: time.Hour, Execute: func() error { runs.Add(1); return nil }}
	server := &retirementPendingServer{cache: cache, started: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, _, err := queryIP(context.Background(), server, "pending.invalid", featureDNS.IPOption{IPv4Enable: true})
		result <- err
	}()
	select {
	case <-server.started:
	case <-time.After(2 * time.Second):
		t.Fatal("query did not start")
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("retired query succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("retired query did not terminate")
	}
	var workers sync.WaitGroup
	for range 64 {
		workers.Go(func() {
			cache.updateRecord(&dnsRequest{domain: "late.invalid.", reqType: dnsmessage.TypeA, start: time.Now()}, &IPRecord{IP: []net.IP{net.LocalHostIP.IP()}, Expire: time.Now().Add(time.Hour)})
		})
	}
	workers.Wait()
	if runs.Load() != 0 || len(cache.ips) != 0 {
		t.Fatal("late response revived cache or cleanup timer")
	}
	if _, _, err := queryIP(context.Background(), server, "again.invalid", featureDNS.IPOption{IPv4Enable: true}); err == nil {
		t.Fatal("retired server started a new query")
	}
	if server.calls.Load() != 1 {
		t.Fatal("retired server sent another request")
	}
	if !cache.pub.Subscribe("after").IsClosed() {
		t.Fatal("retired cache retained subscriber")
	}
}

func TestRetirementClosesActiveDoHSocketsAndRejectsLateDial(t *testing.T) {
	address, _ := url.Parse("https+local://127.0.0.1/dns-query")
	server := NewDoHNameServer(address, nil, true, false, false, 0, nil)
	client, peer := stdnet.Pipe()
	defer peer.Close()
	tracked, err := server.trackConnection(client)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("retired active HTTP/2 socket remained open")
	}
	_ = tracked.Close()
	late, latePeer := stdnet.Pipe()
	defer latePeer.Close()
	if _, err := server.trackConnection(late); err == nil {
		t.Fatal("late HTTP/2 dial reopened retired server")
	}
	_ = latePeer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := latePeer.Read(make([]byte, 1)); err == nil {
		t.Fatal("late HTTP/2 socket was not closed")
	}
	server.connectionsAccess.Lock()
	count := len(server.connections)
	server.connectionsAccess.Unlock()
	if count != 0 {
		t.Fatal("retired server retained sockets")
	}
	_ = server.Close()
}

func TestRetirementCancelsQUICHandshakeAndCannotReopenPool(t *testing.T) {
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.LocalHostIP.IP()})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	address, _ := url.Parse("quic+local://" + socket.LocalAddr().String())
	server, err := NewQUICNameServer(address, false, false, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	result := make(chan error, 1)
	go func() { _, err := server.getConnection(context.Background()); result <- err }()
	_ = socket.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := socket.ReadFromUDP(make([]byte, 2048)); err != nil {
		t.Fatal("QUIC handshake did not start", err)
	}
	closed := make(chan struct{})
	go func() { _ = server.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("retirement waited for handshake timeout")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("retired handshake succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("handshake did not terminate")
	}
	if _, err := server.getConnection(context.Background()); err == nil {
		t.Fatal("retired QUIC pool reopened")
	}
}

func TestRetirementCancelsBlockedTCPQuery(t *testing.T) {
	client, peer := stdnet.Pipe()
	defer client.Close()
	defer peer.Close()
	destination := net.TCPDestination(net.LocalHostIP, 53)
	started := make(chan struct{})
	server := &TCPNameServer{cacheController: NewCacheController("tcp-retirement", false, false, 0), destination: &destination, dial: func(context.Context) (net.Conn, error) { close(started); return client, nil }}
	owner := &DNS{clients: []*Client{{server: server}}}
	result := make(chan error, 1)
	go func() {
		_, _, err := server.QueryIP(context.Background(), "pending.invalid", featureDNS.IPOption{IPv4Enable: true})
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("TCP query did not dial")
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("retired TCP query succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("TCP query did not terminate")
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	for {
		if _, err := peer.Read(make([]byte, 64)); err != nil {
			if err != io.EOF {
				t.Fatal("blocked TCP socket did not close", err)
			}
			break
		}
	}
	if _, _, err := owner.LookupIP("anything.invalid", featureDNS.IPOption{IPv4Enable: true}); err == nil {
		t.Fatal("closed DNS feature accepted lookup")
	}
	if err := owner.Start(); err == nil {
		t.Fatal("closed DNS feature restarted")
	}
	_ = owner.Close()
}
