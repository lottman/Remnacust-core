package core

import (
	"context"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/policy"
	"github.com/xtls/xray-core/transport/internet"
)

type generationPolicy struct {
	policy.DefaultManager
	handshake time.Duration
}

func (p *generationPolicy) ForLevel(level uint32) policy.Session {
	value := p.DefaultManager.ForLevel(level)
	value.Timeouts.Handshake = p.handshake
	return value
}

func TestRuntimePolicyComesFromPinnedConnectionGeneration(t *testing.T) {
	parent := &Instance{ctx: context.Background()}
	initial := &generationPolicy{handshake: time.Second}
	if err := parent.AddFeature(initial); err != nil {
		t.Fatal(err)
	}
	old := NewIsolatedFeatureScope(parent, policy.ManagerType())
	defer old.Close()
	previous := &generationPolicy{handshake: 2 * time.Second}
	if err := old.Add(previous); err != nil {
		t.Fatal(err)
	}
	next := NewIsolatedFeatureScope(parent, policy.ManagerType())
	defer next.Close()
	newer := &generationPolicy{handshake: 3 * time.Second}
	if err := next.Add(newer); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		ctx  context.Context
		want policy.Manager
	}{
		{context.Background(), initial}, {nil, initial}, {parent.RuntimeContext(), initial},
		{RuntimeContext(parent.RuntimeContext(), old.Context()), previous},
		{ToBackgroundDetachedContext(old.Context()), previous},
		{RuntimeContext(parent.RuntimeContext(), next.Context()), newer},
	} {
		if manager := PolicyForContext(check.ctx, initial); manager != check.want || manager.ForLevel(0).Timeouts.Handshake != check.want.ForLevel(0).Timeouts.Handshake {
			t.Fatal("connection policy escaped its generation")
		}
	}
}

type generationDNS struct{ address net.IP }

func (*generationDNS) Type() interface{} { return dns.ClientType() }
func (*generationDNS) Start() error      { return nil }
func (*generationDNS) Close() error      { return nil }
func (d *generationDNS) LookupIP(string, dns.IPOption) ([]net.IP, uint32, error) {
	return []net.IP{d.address}, 60, nil
}

func TestRuntimeAndDetachedContextsKeepTheirGenerationDNS(t *testing.T) {
	parent := &Instance{ctx: context.Background()}
	if err := parent.AddFeature(&generationDNS{net.ParseIP("192.0.2.1")}); err != nil {
		t.Fatal(err)
	}
	old := NewFeatureScope(parent, dns.ClientType())
	defer old.Close()
	if err := old.Add(&generationDNS{net.ParseIP("192.0.2.2")}); err != nil {
		t.Fatal(err)
	}
	next := NewFeatureScope(parent, dns.ClientType())
	defer next.Close()
	if err := next.Add(&generationDNS{net.ParseIP("192.0.2.3")}); err != nil {
		t.Fatal(err)
	}
	// RuntimeContext overlays the generation without losing connection values;
	// background transport jobs detach cancellation but must keep that generation.
	type connectionKey struct{}
	connection := context.WithValue(parent.RuntimeContext(), connectionKey{}, "connection")
	oldContext := RuntimeContext(connection, old.Context())
	nextContext := RuntimeContext(connection, next.Context())
	for _, test := range []struct {
		ctx context.Context
		ip  string
	}{
		{parent.RuntimeContext(), "192.0.2.1"},
		{oldContext, "192.0.2.2"},
		{ToBackgroundDetachedContext(oldContext), "192.0.2.2"},
		{nextContext, "192.0.2.3"},
		{ToBackgroundDetachedContext(nextContext), "192.0.2.3"},
	} {
		ips, err := internet.LookupForIPContext(test.ctx, "same.example", internet.DomainStrategy_FORCE_IP4, nil)
		if err != nil || len(ips) != 1 || !ips[0].Equal(net.ParseIP(test.ip)) {
			t.Fatalf("generation DNS escaped: want=%s ips=%v err=%v", test.ip, ips, err)
		}
	}
	if oldContext.Value(connectionKey{}) != "connection" || nextContext.Value(connectionKey{}) != "connection" {
		t.Fatal("generation switch dropped session context")
	}
}
