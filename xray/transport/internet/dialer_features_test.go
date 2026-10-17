package internet

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/transport"
)

type scopeDNS struct {
	lookup func(string, dns.IPOption) ([]net.IP, uint32, error)
}

func (*scopeDNS) Type() interface{} { return dns.ClientType() }
func (*scopeDNS) Start() error      { return nil }
func (*scopeDNS) Close() error      { return nil }
func (d *scopeDNS) LookupIP(domain string, options dns.IPOption) ([]net.IP, uint32, error) {
	return d.lookup(domain, options)
}

type scopeDialerFeatures struct {
	dns     dns.Client
	manager outbound.Manager
}

func (s scopeDialerFeatures) DialerDNSClient() dns.Client             { return s.dns }
func (s scopeDialerFeatures) DialerOutboundManager() outbound.Manager { return s.manager }

type scopeProxyManager struct {
	outbound.Manager
	handler outbound.Handler
}

func (m scopeProxyManager) GetHandler(tag string) outbound.Handler {
	if tag == "chain" {
		return m.handler
	}
	return nil
}

type scopeProxy struct {
	onDispatch func(context.Context)
}

func (*scopeProxy) Start() error                         { return nil }
func (*scopeProxy) Close() error                         { return nil }
func (*scopeProxy) Tag() string                          { return "chain" }
func (*scopeProxy) SenderSettings() *serial.TypedMessage { return nil }
func (*scopeProxy) ProxySettings() *serial.TypedMessage  { return nil }
func (h *scopeProxy) Dispatch(ctx context.Context, link *transport.Link) {
	defer common.Close(link.Writer)
	defer common.Close(link.Reader)
	h.onDispatch(ctx)
}

func TestDialSystemKeepsConcurrentProfileDNSAndProxyChains(t *testing.T) {
	// Both profiles intentionally use the same domain and proxy tag. No system
	// sockets or external DNS are involved: the selected proxy records its target.
	var workers sync.WaitGroup
	for profile := 1; profile <= 2; profile++ {
		ip := net.ParseIP(fmt.Sprintf("192.0.2.%d", profile))
		client := &scopeDNS{lookup: func(domain string, options dns.IPOption) ([]net.IP, uint32, error) {
			if domain != "same.example" || !options.IPv4Enable || options.IPv6Enable {
				return nil, 0, fmt.Errorf("unexpected lookup: %s %+v", domain, options)
			}
			return []net.IP{ip}, 60, nil
		}}
		for range 32 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				dispatched := make(chan error, 1)
				handler := &scopeProxy{onDispatch: func(ctx context.Context) {
					outbounds := session.OutboundsFromContext(ctx)
					target := outbounds[len(outbounds)-1].Target
					if !target.Address.Family().IsIP() || !target.Address.IP().Equal(ip) {
						dispatched <- fmt.Errorf("profile %s reached %s", ip, target)
						return
					}
					dispatched <- nil
				}}
				ctx := ContextWithDialerFeatures(context.Background(), scopeDialerFeatures{client, scopeProxyManager{handler: handler}})
				conn, err := DialSystem(ctx, net.TCPDestination(net.DomainAddress("same.example"), 443), &SocketConfig{DomainStrategy: DomainStrategy_FORCE_IP4, DialerProxy: "chain"})
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				select {
				case err := <-dispatched:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(3 * time.Second):
					t.Error("profile proxy was not dispatched")
				}
			}()
		}
	}
	workers.Wait()
}

func TestScopedDNSFallbackDoesNotSwitchAfterGlobalInitialization(t *testing.T) {
	systemFeaturesMu.RLock()
	oldDNS, oldManager := dnsClient, obm
	systemFeaturesMu.RUnlock()
	t.Cleanup(func() { InitSystemDialer(oldDNS, oldManager) })
	ip := net.ParseIP("2001:db8::1")
	calls := 0
	client := &scopeDNS{lookup: func(_ string, option dns.IPOption) ([]net.IP, uint32, error) {
		calls++
		if calls == 1 {
			if !option.IPv4Enable || option.IPv6Enable {
				t.Fatal("preferred query was not IPv4")
			}
			InitSystemDialer(nil, nil)
			return nil, 0, dns.ErrEmptyResponse
		}
		if option.IPv4Enable || !option.IPv6Enable {
			t.Fatal("fallback query was not IPv6")
		}
		return []net.IP{ip}, 60, nil
	}}
	ctx := ContextWithDialerFeatures(context.Background(), scopeDialerFeatures{dns: client})
	ips, err := LookupForIPContext(ctx, "same.example", DomainStrategy_FORCE_IP46, nil)
	if err != nil || len(ips) != 1 || !ips[0].Equal(ip) || calls != 2 {
		t.Fatalf("fallback left its owning DNS client: ips=%v err=%v calls=%d", ips, err, calls)
	}
}

func TestIncompleteScopeCannotBorrowGlobalFeatures(t *testing.T) {
	systemFeaturesMu.RLock()
	oldDNS, oldManager := dnsClient, obm
	systemFeaturesMu.RUnlock()
	t.Cleanup(func() { InitSystemDialer(oldDNS, oldManager) })
	client := &scopeDNS{lookup: func(string, dns.IPOption) ([]net.IP, uint32, error) {
		t.Error("private scope borrowed global DNS")
		return []net.IP{net.ParseIP("192.0.2.3")}, 60, nil
	}}
	InitSystemDialer(client, scopeProxyManager{handler: &scopeProxy{onDispatch: func(context.Context) { t.Error("private scope borrowed global proxy") }}})
	ctx := ContextWithDialerFeatures(context.Background(), scopeDialerFeatures{})
	if _, err := LookupForIPContext(ctx, "same.example", DomainStrategy_FORCE_IP4, nil); err == nil {
		t.Fatal("scope without DNS was accepted")
	}
	if _, err := DialSystem(ctx, net.TCPDestination(net.LocalHostIP, 443), &SocketConfig{DialerProxy: "chain"}); err == nil {
		t.Fatal("scope without outbound manager was accepted")
	}
	// Unscoped callers retain the historical exported API.
	client.lookup = func(string, dns.IPOption) ([]net.IP, uint32, error) { return []net.IP{net.LocalHostIP.IP()}, 60, nil }
	if _, err := LookupForIP("same.example", DomainStrategy_FORCE_IP4, nil); err != nil {
		t.Fatal(err)
	}
}
