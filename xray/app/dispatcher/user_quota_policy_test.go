//go:build !wasm

package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func TestRecoveryAccessWaitsForUserDebtFenceWithoutSpendingItsFiniteGrant(t *testing.T) {
	policy := userQuotaFixture(t, "100", 1000000000)
	access := policy.Hosts[hostA]
	access.UserQuotaExempt = true
	access.Groups = []string{"a"}
	policy.Hosts[hostA] = access
	group := policy.Groups["user:global"]
	group.FencedOwners = map[string]bool{"1": true}
	policy.Groups["user:global"] = group
	if !publishHostPolicy(policy) {
		t.Fatal("recovery fence rejected")
	}
	if err := admitHostPolicy(policyContext("shared"), hostEmail("1", hostA), "up", 1, nil); !errors.Is(err, errHostPolicyFenced) {
		t.Fatal("recovery bypassed the gross debt barrier", err)
	}
	if err := admitHostPolicy(policyContext("shared"), hostEmail("2", hostA), "up", 1, nil); err != nil {
		t.Fatal("unrelated recovery owner was fenced", err)
	}
	ctx, cancel := context.WithCancel(policyContext("shared"))
	cancel()
	if err := waitHostPolicy(ctx, hostEmail("1", hostA), "up", 1); !errors.Is(err, context.Canceled) {
		t.Fatal("fenced recovery stream ignored cancellation", err)
	}
	policy.Generation = "released-recovery"
	group.FencedOwners = nil
	policy.Groups["user:global"] = group
	if !publishHostPolicy(policy) {
		t.Fatal("recovery fence release rejected")
	}
	if err := waitHostPolicy(policyContext("shared"), hostEmail("1", hostA), "up", 101); err != nil {
		t.Fatal("recovery lost its grant exemption after the barrier", err)
	}
	if localQuota.entries[group.Reserves["1"].ID].consumed != 0 {
		t.Fatal("recovery spent the normal finite reserve")
	}
}

func TestRecoveryAccountingChangesDrainAdmissionsBeforePolicyAcknowledgement(t *testing.T) {
	previous := userQuotaFixture(t, "100", 1000000000)
	access := previous.Hosts[hostA]
	access.UserQuotaExempt = true
	access.Groups = []string{"a"}
	previous.Hosts[hostA] = access
	encoded, err := json.Marshal(previous)
	if err != nil {
		t.Fatal(err)
	}
	var next hostPolicyDocument
	if err = json.Unmarshal(encoded, &next); err != nil {
		t.Fatal(err)
	}
	group := next.Groups["user:global"]
	group.FenceAll = true
	next.Groups["user:global"] = group
	affected := changedHostAdmissions(previous, next)
	if !affected["host:"+hostA] || !affected["host:"+hostB] || !affected["legacy"] {
		t.Fatal("global fence ACK could miss admitted recovery bytes", affected)
	}
	group.FenceAll = false
	next.Groups["user:global"] = group
	next.LegacyQuotaMultipliers["user:global"] = 1500000000
	affected = changedHostAdmissions(previous, next)
	if !affected["host:"+hostA] || !affected["legacy"] || affected["host:"+hostB] {
		t.Fatal("recovery price change froze the wrong admissions", affected)
	}
}

func userQuotaFixture(t *testing.T, ceiling string, factor int64) hostPolicyDocument {
	t.Helper()
	previous := localQuota
	localQuota = quotaTestStore(t)
	t.Cleanup(func() { localQuota = previous })
	testHostPolicy(t, hostPolicyDocument{})
	policy := v2Policy()
	policy.Generation = "user-quota-first"
	policy.OfflineReserves = quotaReserveCapability
	policy.ExpiresAt = 1
	policy.UserQuota = userQuotaCapability
	policy.LegacyGroups = []string{"user:global"}
	policy.LegacyQuotaMultipliers = map[string]int64{"user:global": factor}
	policy.LegacyOwners = map[string]legacyOwnerPolicy{"1": {Allowed: true}, "2": {Allowed: true}}
	policy.Groups["user:global"] = hostLimitPolicy{MultiplierScale: userQuotaScale, ReserveRequired: true, UnlimitedOwners: map[string]bool{"2": true}, Reserves: map[string]quotaGrant{
		"1": {ID: "aaaaaaaa-1111-4111-8111-111111111111", Instance: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Ceiling: ceiling, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
	}}
	for host, access := range policy.Hosts {
		access.Groups = append(access.Groups, "user:global")
		access.QuotaMultipliers = map[string]int64{"user:global": factor}
		access.IdentityExpiresAt = map[string]int64{hostEmail("1", host): 0, hostEmail("2", host): 0}
		policy.Hosts[host] = access
	}
	return policy
}

func TestUserQuotaSharesOneBudgetAcrossHostsLegacyDevicesAndDirections(t *testing.T) {
	policy := userQuotaFixture(t, "12", 1500000000)
	if !publishHostPolicy(policy) {
		t.Fatal("user quota policy was not accepted")
	}
	for _, sample := range []struct {
		email, tag, direction string
		size                  int
	}{
		{hostEmail("1", hostA), "shared", "up", 3}, {"1", "ordinary", "down", 2}, {"1~0123456789abcdef01234567", "ordinary", "up", 2}, {hostEmail("1", hostB), "shared", "down", 1},
	} {
		if err := waitHostPolicy(policyContext(sample.tag), sample.email, sample.direction, sample.size); err != nil {
			t.Fatal(err)
		}
	}
	if err := waitHostPolicy(policyContext("ordinary"), "1", "down", 1); err == nil {
		t.Fatal("different host/device obtained another global quota")
	}
	if err := waitHostPolicy(policyContext("ordinary"), "2", "up", 4096); err != nil {
		t.Fatal("another unlimited user stopped", err)
	}
	entry := localQuota.entries[policy.Groups["user:global"].Reserves["1"].ID]
	if entry.consumed != 12 || entry.Scale != userQuotaScale {
		t.Fatalf("incorrect global fractional accounting: %+v", entry)
	}
}

func TestUserQuotaNanofractionsAccumulateWithoutPerPacketRounding(t *testing.T) {
	store := quotaTestStore(t)
	debit := quotaTestDebit("11111111-1111-4111-8111-111111111111", 1, 0)
	debit.Group = "user:global"
	debit.Scale = userQuotaScale
	debit.Fraction = 1
	for range 1000 {
		if err := store.debit([]quotaDebit{debit}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if entry := store.entries[debit.Grant.ID]; entry.consumed != 1 || entry.fraction != 1000 {
		t.Fatalf("small packets rounded separately: %+v", entry)
	}
	debit.Fraction = userQuotaScale - 1000
	if err := store.debit([]quotaDebit{debit}, time.Now()); err != nil {
		t.Fatal("exact final fraction did not fit", err)
	}
	debit.Fraction = 1
	if err := store.debit([]quotaDebit{debit}, time.Now()); err == nil {
		t.Fatal("nanofraction exceeded the whole budget")
	}
	loaded := &quotaStore{path: store.path}
	if err := loaded.debit([]quotaDebit{debit}, time.Now()); err == nil {
		t.Fatal("restart refunded a prepaid fractional tail")
	}
}

func TestUserQuotaMixedPrecisionsAreAtomicAndCannotChangeExistingGrantScale(t *testing.T) {
	store := quotaTestStore(t)
	host := quotaTestDebit("11111111-1111-4111-8111-111111111111", 100, 1)
	global := quotaTestDebit("22222222-2222-4222-8222-222222222222", 1, 1)
	global.Group = "user:global"
	global.Scale = userQuotaScale
	global.Fraction = userQuotaScale / 2
	if err := store.debit([]quotaDebit{host, global}, time.Now()); err == nil {
		t.Fatal("insufficient global budget accepted")
	}
	if store.entries[host.Grant.ID].consumed != 0 {
		t.Fatal("a rejected global charge consumed host quota")
	}
	global.Grant.Ceiling = "2"
	if err := store.debit([]quotaDebit{host, global}, time.Now()); err != nil {
		t.Fatal(err)
	}
	global.Scale = 100
	global.Fraction = 0
	if err := store.debit([]quotaDebit{global}, time.Now()); err == nil {
		t.Fatal("the same grant changed precision")
	}
	loaded := &quotaStore{path: store.path}
	if err := loaded.debit([]quotaDebit{global}, time.Now()); err == nil {
		t.Fatal("restart forgot the grant precision binding")
	}
}

func TestUserQuotaKnownEntitlementsAndProtectedInboundRemainMandatory(t *testing.T) {
	policy := userQuotaFixture(t, "100", 1000000000)
	policy.LegacyOwners["3"] = legacyOwnerPolicy{Allowed: false}
	policy.LegacyOwners["4"] = legacyOwnerPolicy{Allowed: true, ExpiresAt: time.Now().Add(-time.Second).UnixMilli()}
	if !publishHostPolicy(policy) {
		t.Fatal("policy was rejected")
	}
	for _, email := range []string{"3", "4", "5", "invalid", "1~bad"} {
		if err := waitHostPolicy(policyContext("ordinary"), email, "up", 1); err == nil {
			t.Fatalf("unauthorized legacy identity passed: %s", email)
		}
	}
	if err := waitHostPolicy(policyContext("shared"), "1", "up", 1); err == nil {
		t.Fatal("global quota bypassed host isolation")
	}
	if err := waitHostPolicy(policyContext("ordinary"), "1", "up", 1); err != nil {
		t.Fatal("stale panel TTL blocked a valid finite global reserve", err)
	}
}

func TestUserQuotaLegacyFenceRespectsOwnerExpiryAndOtherUsers(t *testing.T) {
	policy := userQuotaFixture(t, "100", 1000000000)
	owner := policy.LegacyOwners["1"]
	owner.ExpiresAt = time.Now().Add(150 * time.Millisecond).UnixMilli()
	policy.LegacyOwners["1"] = owner
	group := policy.Groups["user:global"]
	group.FencedOwners = map[string]bool{"1": true}
	policy.Groups["user:global"] = group
	if !publishHostPolicy(policy) {
		t.Fatal("fenced policy was rejected")
	}
	ctx, cancel := context.WithTimeout(policyContext("ordinary"), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- waitHostPolicy(ctx, "1", "up", 1) }()
	if err := waitHostPolicy(policyContext("ordinary"), "2", "up", 1); err != nil {
		t.Fatal("unaffected owner paused", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expired owner passed its fence")
		}
	case <-time.After(time.Second):
		t.Fatal("legacy global fence ignored local expiry")
	}
}

func TestUserQuotaRequirementSurvivesPolicyRemovalAndCorruption(t *testing.T) {
	policy := userQuotaFixture(t, "100", 1000000000)
	if !publishHostPolicy(policy) {
		t.Fatal("policy was rejected")
	}
	if required, err := userQuotaRequired(); err != nil || !required {
		t.Fatal("durable user quota requirement was not committed", err)
	}
	legacy := v2Policy()
	legacy.Generation = "unsafe-downgrade"
	if publishHostPolicy(legacy) {
		t.Fatal("legacy policy removed the global quota requirement")
	}
	hostPolicies.Lock()
	hostPolicies.document = hostPolicyDocument{}
	hostPolicies.Unlock()
	if err := validateUserQuotaStartup(); err == nil {
		t.Fatal("missing saved policy permitted an unmetered startup")
	}
	if err := os.WriteFile(userQuotaRequirementPath(), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := userQuotaRequired(); err == nil {
		t.Fatal("corrupt requirement was accepted")
	}
}

func TestUserQuotaValidationRejectsUnboundZeroFactorsAndUnsupportedPrecision(t *testing.T) {
	for _, mutate := range []func(*hostPolicyDocument){
		func(p *hostPolicyDocument) { p.UserQuota = "" }, func(p *hostPolicyDocument) { delete(p.LegacyQuotaMultipliers, "user:global") },
		func(p *hostPolicyDocument) {
			g := p.Groups["user:global"]
			g.MultiplierScale = 101
			p.Groups["user:global"] = g
		},
		func(p *hostPolicyDocument) { p.LegacyGroups = append(p.LegacyGroups, "user:global") }, func(p *hostPolicyDocument) { p.LegacyOwners["01"] = legacyOwnerPolicy{Allowed: true} },
		func(p *hostPolicyDocument) { h := p.Hosts[hostA]; h.Groups = nil; p.Hosts[hostA] = h },
		func(p *hostPolicyDocument) { h := p.Hosts[hostA]; h.UserQuotaExempt = true; p.Hosts[hostA] = h },
	} {
		policy := userQuotaFixture(t, "100", 0)
		mutate(&policy)
		if err := validateUserQuotaPolicy(policy); err == nil {
			t.Fatal("invalid user quota policy was accepted")
		}
	}
}

func TestUserQuotaRecoveryExemptionMustBeExplicitAndPreservesOtherHostChecks(t *testing.T) {
	policy := userQuotaFixture(t, "0", 1000000000)
	host := policy.Hosts[hostA]
	host.Groups = nil
	if err := validateUserQuotaPolicy(policy); err != nil {
		t.Fatal("original binding rejected", err)
	}
	policy.Hosts[hostA] = host
	if err := validateUserQuotaPolicy(policy); err == nil {
		t.Fatal("missing global binding silently exempted a host")
	}
	host.UserQuotaExempt = true
	policy.Hosts[hostA] = host
	if !publishHostPolicy(policy) {
		t.Fatal("explicit recovery policy rejected")
	}
	if err := waitHostPolicy(policyContext("shared"), hostEmail("1", hostA), "up", 4); err != nil {
		t.Fatal("explicit recovery host was blocked by the exhausted global reserve", err)
	}
	if err := waitHostPolicy(policyContext("shared"), hostEmail("1", hostB), "up", 4); err == nil {
		t.Fatal("ordinary host bypassed global exhaustion")
	}
	if err := waitHostPolicy(policyContext("other"), hostEmail("1", hostA), "up", 4); err == nil {
		t.Fatal("recovery exemption bypassed the host's inbound binding")
	}
}
