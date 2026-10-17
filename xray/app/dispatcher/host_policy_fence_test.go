//go:build !wasm

package dispatcher

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/transport"
)

func TestHostPolicySoftFencePreservesWaiterAndOtherOwners(t *testing.T) {
	doc := v2Policy()
	doc.Groups["a"] = hostLimitPolicy{FencedOwners: map[string]bool{"1": true}}
	testHostPolicy(t, doc)
	ctx, cancel := context.WithTimeout(policyContext("shared"), 3*time.Second)
	defer cancel()
	var accounted atomic.Int64
	done := make(chan error, 1)
	go func() {
		done <- waitHostPolicyAndAccount(ctx, hostEmail("1", hostA), "up", 1, func() { accounted.Add(1) })
	}()
	select {
	case err := <-done:
		t.Fatalf("transition closed the waiter: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if accounted.Load() != 0 {
		t.Fatal("fenced bytes were admitted")
	}
	for _, email := range []string{hostEmail("2", hostA), hostEmail("1", hostB)} {
		if err := waitHostPolicy(ctx, email, "up", 1); err != nil {
			t.Fatal("unaffected owner or host stopped", err)
		}
	}
	next := v2Policy()
	next.Generation = "fence-finished"
	if !publishHostPolicy(next) {
		t.Fatal("resume policy was rejected")
	}
	if err := <-done; err != nil {
		t.Fatal("waiter could not resume", err)
	}
	if accounted.Load() != 1 {
		t.Fatal("waiter accounted more than once")
	}
}

func TestHostPolicyFenceReleasesOnExplicitDeviceRevocation(t *testing.T) {
	doc := v2Policy()
	doc.Groups["a"] = hostLimitPolicy{FenceAll: true}
	testHostPolicy(t, doc)
	email := hostEmail("1", hostA)
	ctx, cancel := context.WithCancel(session.ContextWithInbound(context.Background(), &session.Inbound{Tag: "shared", User: &protocol.MemoryUser{Email: email}}))
	defer cancel()
	t.Cleanup(func() {
		activeUserLinks.Lock()
		delete(activeUserLinks.revoked, email)
		delete(activeUserLinks.byEmail, email)
		activeUserLinks.Unlock()
	})
	link, peer := &transport.Link{Writer: buf.Discard}, &transport.Link{Writer: buf.Discard}
	trackUserLink(ctx, email, link, peer)
	applyHostPolicy(ctx, link, peer)
	done := make(chan error, 1)
	go func() { done <- link.Writer.WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes([]byte("pending"))}) }()
	select {
	case err := <-done:
		t.Fatalf("fence did not retain the connection: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	RevokeUserLinks(email)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("revoked data was forwarded")
		}
	case <-time.After(200 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("device revocation did not release the quota waiter")
	}
}

func TestHostPolicyGracefulHalfCloseKeepsTheReplyDirectionAlive(t *testing.T) {
	testHostPolicy(t, hostPolicyDocument{})
	ctx := session.ContextWithInbound(context.Background(), &session.Inbound{User: &protocol.MemoryUser{Email: "half-close"}})
	link, peer := &transport.Link{Writer: buf.Discard}, &transport.Link{Writer: buf.Discard}
	applyHostPolicy(ctx, link, peer)
	if err := link.Writer.(*hostPolicyWriter).Close(); err != nil {
		t.Fatal(err)
	}
	if err := peer.Writer.WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes([]byte("reply"))}); err != nil {
		t.Fatal("upload EOF cancelled the reply", err)
	}
}

func TestHostPolicySoftFenceReleasesOnCoreShutdown(t *testing.T) {
	doc := v2Policy()
	doc.Groups["a"] = hostLimitPolicy{FenceAll: true}
	testHostPolicy(t, doc)
	instance := &core.Instance{}
	ctx, cancel := context.WithTimeout(context.WithValue(policyContext("shared"), core.XrayKey(1), instance), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- waitHostPolicy(ctx, hostEmail("1", hostA), "up", 1) }()
	select {
	case err := <-done:
		t.Fatalf("transition did not retain traffic: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed core admitted data")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("closed core left a quota waiter")
	}
}

func TestHostPolicySoftFenceStopsOnCancellationOrIdentityDeadline(t *testing.T) {
	for _, expired := range []bool{false, true} {
		doc := v2Policy()
		doc.Groups["a"] = hostLimitPolicy{FenceAll: true}
		if expired {
			doc.ExpiresAt = time.Now().Add(40 * time.Millisecond).UnixMilli()
		}
		testHostPolicy(t, doc)
		ctx, cancel := context.WithTimeout(policyContext("shared"), 80*time.Millisecond)
		start := time.Now()
		err := waitHostPolicy(ctx, hostEmail("1", hostA), "up", 1)
		cancel()
		if err == nil || time.Since(start) > time.Second {
			t.Fatal("fence ignored cancellation/expiry", err)
		}
	}
}

func TestHostPolicyPublicationDrainsAdmissionsWithoutBlockingNeighborPorts(t *testing.T) {
	testPublicationAdmissionDrain(t, false)
}

func TestRecoveryGlobalFenceDrainsItsUnreservedAccountingBeforeAcknowledgement(t *testing.T) {
	testPublicationAdmissionDrain(t, true)
}

func testPublicationAdmissionDrain(t *testing.T, recovery bool) {
	store := quotaTestStore(t)
	before := localQuota
	localQuota = store
	t.Cleanup(func() { localQuota = before })
	debit := quotaTestDebit("ffffffff-ffff-4fff-8fff-ffffffffffff", 100, 0)
	debit.Owner, debit.Group = "1", "a"
	doc := v2Policy()
	doc.OfflineReserves = quotaReserveCapability
	for id, access := range doc.Hosts {
		access.IdentityExpiresAt = map[string]int64{}
		for email := range access.AllowedIdentities {
			access.IdentityExpiresAt[email] = 0
		}
		access.QuotaMultipliers = map[string]int64{"a": 100, "b": 100}
		doc.Hosts[id] = access
	}
	doc.Groups["a"] = hostLimitPolicy{ReserveRequired: true, Reserves: map[string]quotaGrant{"1": debit.Grant}}
	tracker, manager := usageTrackerFixture(t)
	if recovery {
		doc.UserQuota = userQuotaCapability
		doc.LegacyGroups = []string{"user:global"}
		doc.LegacyOwners = map[string]legacyOwnerPolicy{"1": {Allowed: true}, "2": {Allowed: true}}
		doc.LegacyQuotaMultipliers = map[string]int64{"user:global": userQuotaScale}
		doc.Groups["user:global"] = hostLimitPolicy{ReserveRequired: true, MultiplierScale: userQuotaScale, UnlimitedOwners: map[string]bool{"2": true}, UnreservedAccounts: map[string]string{"1": unreservedAccount}}
		access := doc.Hosts[hostA]
		access.UserQuotaExempt = true
		doc.Hosts[hostA] = access
		access = doc.Hosts[hostB]
		access.Groups = append(access.Groups, "user:global")
		access.QuotaMultipliers["user:global"] = userQuotaScale
		doc.Hosts[hostB] = access
	}
	if err := store.reconcile(doc, time.Now()); err != nil {
		t.Fatal(err)
	}
	testHostPolicy(t, doc)
	ctx, cancel := context.WithTimeout(policyContext("shared"), 3*time.Second)
	var release sync.Once
	store.Lock()
	releaseStore := func() { release.Do(store.Unlock) }
	admitted, published := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	publication := make(chan bool, 1)
	var accounted atomic.Int64
	go func() {
		defer close(admitted)
		result <- waitHostPolicyAndAccount(ctx, hostEmail("1", hostA), "up", 1, func() { accounted.Add(1) }, tracker)
	}()
	launched := false
	t.Cleanup(func() {
		releaseStore()
		cancel()
		select {
		case <-admitted:
		case <-time.After(time.Second):
			t.Error("admission leaked")
		}
		if launched {
			select {
			case <-published:
			case <-time.After(time.Second):
				t.Error("policy publication leaked")
			}
		}
	})
	wait := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			hostPolicies.Lock()
			ok := check()
			hostPolicies.Unlock()
			if ok {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("policy transition did not reach its expected barrier")
	}
	wait(func() bool { return hostPolicies.admissions["host:"+hostA] == 1 })
	next := v2Policy()
	next.Generation = "quota-fence"
	next.OfflineReserves = quotaReserveCapability
	for id, access := range doc.Hosts {
		next.Hosts[id] = access
	}
	next.Groups["a"] = hostLimitPolicy{ReserveRequired: true, FenceAll: true}
	if recovery {
		encoded, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(encoded, &next); err != nil {
			t.Fatal(err)
		}
		next.Generation = "recovery-global-fence"
		group := next.Groups["user:global"]
		group.FencedOwners = map[string]bool{"1": true}
		next.Groups["user:global"] = group
	}
	launched = true
	go func() { defer close(published); publication <- publishHostPolicy(next) }()
	wait(func() bool { return hostPolicies.frozen["host:"+hostA] })
	select {
	case <-published:
		t.Fatal("policy acknowledged before its admitted buffer")
	default:
	}
	neighborOwner := "1"
	if recovery {
		neighborOwner = "2"
	}
	if !recovery {
		if err := waitHostPolicy(ctx, hostEmail(neighborOwner, hostB), "up", 1); err != nil {
			t.Fatal("quota disk IO blocked an unrelated host", err)
		}
	}
	releaseStore()
	if err := <-result; err != nil {
		t.Fatal("pre-fence admission was dropped", err)
	}
	if !<-publication {
		t.Fatal("policy publication failed")
	}
	if recovery {
		if err := waitHostPolicy(ctx, hostEmail(neighborOwner, hostB), "up", 1); err != nil {
			t.Fatal("published owner fence blocked another user", err)
		}
	}
	if accounted.Load() != 1 {
		t.Fatal("pre-fence usage escaped its baseline")
	}
	entry := store.entries[debit.Grant.ID]
	if entry.Closed == recovery || entry.consumed != 1 || (!recovery && entry.Reserved != 1) {
		t.Fatal("sealed grant lost an admitted charge", entry)
	}
	if recovery {
		counter := manager.GetCounter(privateUsageName(t, unreservedAccount, "1", "uplink", userQuotaScale, userQuotaScale))
		if counter == nil || counter.Value() != 1 {
			t.Fatal("recovery debt escaped the acknowledged admission fence")
		}
	}
}
