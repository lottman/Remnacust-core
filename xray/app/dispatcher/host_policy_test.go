//go:build !wasm

package dispatcher

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/transport"
)

func testHostPolicy(t *testing.T, doc hostPolicyDocument) {
	t.Helper()
	hostPolicies.Lock()
	before := hostPolicies.document
	hostPolicies.document = doc
	hostPolicies.buckets = make(map[string]*hostBucket)
	hostPolicies.Unlock()
	t.Cleanup(func() {
		hostPolicies.Lock()
		hostPolicies.document = before
		hostPolicies.buckets = make(map[string]*hostBucket)
		hostPolicies.Unlock()
	})
}

type hostCountingWriter struct{ bytes atomic.Int64 }

func (w *hostCountingWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	w.bytes.Add(int64(mb.Len()))
	buf.ReleaseMulti(mb)
	return nil
}

func TestHostPolicySharesSpeedAcrossConnectionsAndDevices(t *testing.T) {
	testHostPolicy(t, hostPolicyDocument{Group: "tag:test", BytesPerSecond: 524288, ExpiresAt: time.Now().Add(time.Minute).UnixMilli()})
	sink := &hostCountingWriter{}
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			email := "123"
			if i%2 == 1 {
				email += "~0123456789abcdef01234567"
			}
			writer := hostPolicyWriter{sink, context.Background(), email, "down"}
			for j := 0; j < 16; j++ {
				if err := writer.WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes(make([]byte, 8192))}); err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	if sink.bytes.Load() != 1048576 {
		t.Fatalf("traffic lost: %d", sink.bytes.Load())
	}
	if elapsed := time.Since(start); elapsed < 1750*time.Millisecond {
		t.Fatalf("parallel flows bypassed cap: %v", elapsed)
	}
}

func TestHostPolicyOwnerAndRevocation(t *testing.T) {
	for input, want := range map[string]string{"123": "123", "123~0123456789abcdef01234567": "123", "123~bad": "", "someone@example.com": "", "123~0123456789abcdef01234567~x": ""} {
		if got := hostPolicyOwner(input); got != want {
			t.Fatalf("owner %q: %q", input, got)
		}
	}
	testHostPolicy(t, hostPolicyDocument{Group: "host:one", ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), BlockedOwners: map[string]bool{"123": true}})
	for _, email := range []string{"123", "123~0123456789abcdef01234567"} {
		if waitHostPolicy(context.Background(), email, "up", 1) == nil {
			t.Fatalf("blocked identity allowed: %s", email)
		}
	}
	if err := waitHostPolicy(context.Background(), "124", "up", 1); err != nil {
		t.Fatalf("unrelated user blocked: %v", err)
	}
	hostPolicies.Lock()
	hostPolicies.document.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
	hostPolicies.Unlock()
	if waitHostPolicy(context.Background(), "124", "down", 1) == nil {
		t.Fatal("stale policy allowed traffic")
	}
}

func TestHostPolicyExistingWriterObservesUpdates(t *testing.T) {
	testHostPolicy(t, hostPolicyDocument{})
	sink := &hostCountingWriter{}
	ctx := session.ContextWithInbound(context.Background(), &session.Inbound{User: &protocol.MemoryUser{Email: "123"}, CanSpliceCopy: 2})
	in, out := &transport.Link{Writer: sink}, &transport.Link{Writer: sink}
	applyHostPolicy(ctx, in, out)
	if session.InboundFromContext(ctx).CanSpliceCopy != 3 {
		t.Fatal("splice can bypass limits")
	}
	if err := in.Writer.WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes([]byte("ok"))}); err != nil {
		t.Fatal(err)
	}
	hostPolicies.Lock()
	hostPolicies.document = hostPolicyDocument{Group: "host:one", BlockAll: true}
	hostPolicies.Unlock()
	if out.Writer.WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes([]byte("blocked"))}) == nil {
		t.Fatal("existing stream ignored policy")
	}
	if sink.bytes.Load() != 2 {
		t.Fatal("blocked bytes reached destination")
	}
}

func TestHostPolicyCancellation(t *testing.T) {
	testHostPolicy(t, hostPolicyDocument{Group: "test", BytesPerSecond: 1, ExpiresAt: time.Now().Add(time.Minute).UnixMilli()})
	if err := waitHostPolicy(context.Background(), "123", "down", policyBurst); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitHostPolicy(ctx, "123", "down", 1) == nil {
		t.Fatal("cancelled wait succeeded")
	}
}

const hostA = "11111111111141118111111111111111"
const hostB = "22222222222242228222222222222222"

func hostEmail(owner, host string) string { return owner + "~0123456789abcdef01234567~h" + host }
func policyContext(tag string) context.Context {
	return session.ContextWithInbound(context.Background(), &session.Inbound{Tag: tag})
}
func v2Policy() hostPolicyDocument {
	return hostPolicyDocument{Version: hostPolicyCapability, Generation: "test", ExpiresAt: time.Now().Add(time.Minute).UnixMilli(),
		ProtectedInbounds: []string{"shared"},
		Hosts: map[string]hostAccessPolicy{
			hostA: {InboundTag: "shared", AllowedIdentities: map[string]bool{hostEmail("1", hostA): true, hostEmail("2", hostA): true}, Groups: []string{"a"}},
			hostB: {InboundTag: "shared", AllowedIdentities: map[string]bool{hostEmail("1", hostB): true, hostEmail("2", hostB): true}, Groups: []string{"b"}},
		}, Groups: map[string]hostLimitPolicy{"a": {}, "b": {}},
	}
}
func TestHostPolicyV2IsolationOnSameInbound(t *testing.T) {
	doc := v2Policy()
	doc.Groups["a"] = hostLimitPolicy{BlockAll: true}
	testHostPolicy(t, doc)
	ctx := policyContext("shared")
	for _, email := range []string{hostEmail("1", hostA), "1", "1~0123456789abcdef01234567", hostEmail("3", hostB)} {
		if waitHostPolicy(ctx, email, "up", 1) == nil {
			t.Fatalf("unauthorized identity passed: %s", email)
		}
	}
	if err := waitHostPolicy(ctx, hostEmail("1", hostB), "up", 1); err != nil {
		t.Fatalf("neighbor host affected: %v", err)
	}
	if err := waitHostPolicy(policyContext("unrelated"), "1", "up", 1); err != nil {
		t.Fatalf("node-wide restriction: %v", err)
	}
	if waitHostPolicy(policyContext("other"), hostEmail("1", hostB), "up", 1) == nil {
		t.Fatal("host key used on wrong inbound")
	}
}

func TestHostPolicyV2TagUserSpeedIsIndependentPerHost(t *testing.T) {
	doc := v2Policy()
	doc.Groups["a"] = hostLimitPolicy{BytesPerSecond: policyBurst}
	doc.Groups["b"] = hostLimitPolicy{BytesPerSecond: policyBurst}
	testHostPolicy(t, doc)
	ctx, cancel := context.WithTimeout(policyContext("shared"), 50*time.Millisecond)
	defer cancel()
	if err := waitHostPolicy(ctx, hostEmail("1", hostA), "up", policyBurst); err != nil {
		t.Fatal(err)
	}
	if err := waitHostPolicy(ctx, hostEmail("1", hostB), "up", policyBurst); err != nil {
		t.Fatal("host A consumed host B user budget:", err)
	}
	if err := waitHostPolicy(ctx, hostEmail("1", hostA), "down", policyBurst); err == nil {
		t.Fatal("same user's connections did not share their host budget")
	}
}
func TestHostPolicyV2AllGroupsAndPerUserQuota(t *testing.T) {
	doc := v2Policy()
	a := doc.Hosts[hostA]
	a.Groups = []string{"a", "shared-group"}
	doc.Hosts[hostA] = a
	doc.Groups["shared-group"] = hostLimitPolicy{BlockedOwners: map[string]bool{"1": true}}
	testHostPolicy(t, doc)
	ctx := policyContext("shared")
	if waitHostPolicy(ctx, hostEmail("1", hostA), "up", 1) == nil {
		t.Fatal("second group bypassed")
	}
	if err := waitHostPolicy(ctx, hostEmail("2", hostA), "up", 1); err != nil {
		t.Fatal("user quota blocked another user")
	}
	if err := waitHostPolicy(ctx, hostEmail("1", hostB), "up", 1); err != nil {
		t.Fatal("non-member inherited group")
	}
	hostPolicies.Lock()
	g := hostPolicies.document.Groups["a"]
	g.BlockAll = true
	hostPolicies.document.Groups["a"] = g
	hostPolicies.Unlock()
	if waitHostPolicy(ctx, hostEmail("2", hostA), "up", 1) == nil {
		t.Fatal("whole-host quota not applied")
	}
}
func TestHostPolicyV2WholeHostSpeedAcrossUsersAndDirections(t *testing.T) {
	doc := v2Policy()
	doc.Groups["a"] = hostLimitPolicy{TotalBytesPerSecond: 524288}
	testHostPolicy(t, doc)
	start := time.Now()
	var wg sync.WaitGroup
	for _, owner := range []string{"1", "2"} {
		for _, direction := range []string{"up", "down"} {
			wg.Add(1)
			go func(owner, direction string) {
				defer wg.Done()
				if err := waitHostPolicy(policyContext("shared"), hostEmail(owner, hostA), direction, 262144); err != nil {
					t.Error(err)
				}
			}(owner, direction)
		}
	}
	wg.Wait()
	if time.Since(start) < 1750*time.Millisecond {
		t.Fatal("aggregate speed multiplied by users or directions")
	}
	start = time.Now()
	if err := waitHostPolicy(policyContext("shared"), hostEmail("1", hostB), "down", 1048576); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("unlimited neighbor inherited limiter")
	}
}
func TestHostPolicyV2GroupSpeedAcrossHosts(t *testing.T) {
	doc := v2Policy()
	for key, host := range doc.Hosts {
		host.Groups = append(host.Groups, "tag:both")
		doc.Hosts[key] = host
	}
	doc.Groups["tag:both"] = hostLimitPolicy{BytesPerSecond: 524288}
	testHostPolicy(t, doc)
	start := time.Now()
	var wg sync.WaitGroup
	for _, host := range []string{hostA, hostB} {
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			if err := waitHostPolicy(policyContext("shared"), hostEmail("1", host), "down", 524288); err != nil {
				t.Error(err)
			}
		}(host)
	}
	wg.Wait()
	if time.Since(start) < 1750*time.Millisecond {
		t.Fatal("tag speed multiplied by hosts")
	}
}
func TestHostPolicyV2DomainsRevocationAndExpiry(t *testing.T) {
	doc := v2Policy()
	a := doc.Hosts[hostA]
	a.DomainMode = "ALLOW_ONLY"
	a.Domains = []string{"example.org"}
	doc.Hosts[hostA] = a
	testHostPolicy(t, doc)
	ctx := session.ContextWithOutbounds(policyContext("shared"), []*session.Outbound{{Target: xnet.TCPDestination(xnet.DomainAddress("sub.example.org"), 443)}})
	email := hostEmail("1", hostA)
	if err := waitHostPolicy(ctx, email, "up", 1); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"example.org.evil.test", "evil-example.org", "1.2.3.4"} {
		other := session.ContextWithOutbounds(policyContext("shared"), []*session.Outbound{{Target: xnet.TCPDestination(xnet.ParseAddress(target), 443)}})
		if waitHostPolicy(other, email, "up", 1) == nil {
			t.Fatalf("domain bypass: %s", target)
		}
	}
	hostPolicies.Lock()
	host := hostPolicies.document.Hosts[hostA]
	delete(host.AllowedIdentities, email)
	hostPolicies.document.Hosts[hostA] = host
	hostPolicies.Unlock()
	if waitHostPolicy(ctx, email, "up", 1) == nil {
		t.Fatal("revocation ignored")
	}
	hostPolicies.Lock()
	hostPolicies.document.ExpiresAt = 1
	hostPolicies.Unlock()
	if waitHostPolicy(policyContext("shared"), hostEmail("1", hostB), "up", 1) == nil {
		t.Fatal("stale host policy failed open")
	}
}

func TestHostDestinationRules(t *testing.T) {
	rules := compileHostDestinations([]string{"telegram.org", "xn--e1afmkfd.xn--p1ai", "149.154.160.0/20", "2001:db8::/32", "1.2.3.4"})
	for _, target := range []string{"telegram.org", "api.telegram.org", "ПРИМЕР.РФ", "149.154.175.255", "2001:db8::1", "1.2.3.4", "::ffff:1.2.3.4"} {
		if !rules.matches(target) {
			t.Errorf("allowed destination rejected: %s", target)
		}
	}
	for _, target := range []string{"eviltelegram.org", "telegram.org.attacker.test", "149.154.176.1", "2001:db9::1", "evil.1.2.3.4", "evil.149.154.160.0", "127.0.0.1"} {
		if rules.matches(target) {
			t.Errorf("forbidden destination allowed: %s", target)
		}
	}
}
