//go:build !wasm

package dispatcher

import (
	"context"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
)

func TestPacketPolicyPreservesHostScopeAndDestination(t *testing.T) {
	doc := v2Policy()
	email := hostEmail("1", hostA)
	host := doc.Hosts[hostA]
	host.DomainMode = "ALLOW_ONLY"
	host.Domains = []string{"192.0.2.0/24"}
	doc.Hosts[hostA] = host
	testHostPolicy(t, doc)
	if err := CheckUserPacket(context.Background(), "shared", email, net.ParseAddress("192.0.2.1"), "up", 100); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []struct{ tag, email, target string }{
		{"wrong-inbound", email, "192.0.2.1"}, {"shared", "1", "192.0.2.1"}, {"shared", email, "198.51.100.1"},
	} {
		if CheckUserPacket(context.Background(), candidate.tag, candidate.email, net.ParseAddress(candidate.target), "down", 100) == nil {
			t.Fatal("packet bypassed policy", candidate)
		}
	}
}

func TestPacketPolicyRevocationAndCancellation(t *testing.T) {
	email := "7301"
	testHostPolicy(t, hostPolicyDocument{Group: "peer", BytesPerSecond: 1, ExpiresAt: time.Now().Add(time.Minute).UnixMilli()})
	RevokeUserLinks(email)
	t.Cleanup(func() { AllowUserLinks(email) })
	if CheckUserPacket(context.Background(), "peer", email, net.ParseAddress("192.0.2.1"), "up", 1) == nil {
		t.Fatal("revoked user sent a packet")
	}
	AllowUserLinks(email)
	if err := CheckUserPacket(context.Background(), "peer", email, net.ParseAddress("192.0.2.1"), "up", policyBurst); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if CheckUserPacket(ctx, "peer", email, net.ParseAddress("192.0.2.1"), "down", 1) == nil {
		t.Fatal("cancelled packet wait continued")
	}
}
