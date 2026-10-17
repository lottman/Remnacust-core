//go:build !wasm

package dispatcher

import (
	"context"
	"errors"
	"testing"
	"time"

	appstats "github.com/xtls/xray-core/app/stats"
	"github.com/xtls/xray-core/common/quotausage"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/stats"
)

func TestManagedProfileCannotUnregisterLiveNodeAccountingSource(t *testing.T) {
	root, err := core.NewProfile(context.Background(), &core.Config{App: []*serial.TypedMessage{serial.ToTypedMessage(&appstats.Config{})}})
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.Start(); err != nil {
		t.Fatal(err)
	}
	profile, err := core.NewManagedProfile(root, &core.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer profile.Close()
	if err := profile.Start(); err != nil {
		t.Fatal(err)
	}
	tracker := newUnreservedUsage(profile.RuntimeContext())
	manager := root.GetFeature(stats.ManagerType()).(stats.Manager)
	if tracker.manager != manager {
		t.Fatal("profile selected a different financial source")
	}
	scope := core.NewIsolatedFeatureScope(profile)
	defer scope.Close()
	if tracker := newUnreservedUsage(scope.Context()); tracker.manager != manager {
		t.Fatal("generation lost node financial source")
	}
	if err := profile.Close(); err != nil {
		t.Fatal(err)
	}
	hostPolicies.Lock()
	registered := unreservedManagers[manager]
	hostPolicies.Unlock()
	if !registered {
		t.Fatal("closing a profile unregistered live node accounting")
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		hostPolicies.Lock()
		registered = unreservedManagers[manager]
		hostPolicies.Unlock()
		if !registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closed node retained accounting registration")
		}
		time.Sleep(time.Millisecond)
	}
}

const unreservedAccount = "ff29ee68-a625-4160-9573-cc17de317a5d"
const otherUnreservedAccount = "ff29ee68-a625-4160-9573-cc17de317a5e"

func usageTrackerFixture(t *testing.T) (*unreservedUsage, *appstats.Manager) {
	t.Helper()
	manager, err := appstats.NewManager(context.Background(), &appstats.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return &unreservedUsage{manager: manager}, manager
}

func privateUsageName(t *testing.T, account, owner, direction string, factor, scale int64) string {
	t.Helper()
	name, err := (quotausage.Counter{Account: account, Owner: owner, Direction: direction, Factor: factor, Scale: scale}).Name()
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func TestUnreservedUsageDoesNotChargeReservedBytesTwice(t *testing.T) {
	policy := userQuotaFixture(t, "100", 1500000000)
	group := policy.Groups["user:global"]
	group.UnreservedAccounts = map[string]string{"1": unreservedAccount, "2": otherUnreservedAccount}
	policy.Groups["user:global"] = group
	if !publishHostPolicy(policy) {
		t.Fatal("policy rejected")
	}
	tracker, manager := usageTrackerFixture(t)
	var gross int64
	for _, sample := range []struct {
		email, tag, direction string
		size                  int
	}{
		{"1", "ordinary", "up", 3},
		{hostEmail("1", hostA), "shared", "down", 2},
		{"2", "ordinary", "up", 7},
		{hostEmail("2", hostB), "shared", "down", 11},
	} {
		if err := waitHostPolicyAndAccount(policyContext(sample.tag), sample.email, sample.direction, sample.size, func() { gross += int64(sample.size) }, tracker); err != nil {
			t.Fatal(err)
		}
	}
	if gross != 23 {
		t.Fatal("raw accounting changed", gross)
	}
	if manager.GetCounter(privateUsageName(t, unreservedAccount, "1", "uplink", 1500000000, userQuotaScale)) != nil {
		t.Fatal("reserved bytes also charged as outside usage")
	}
	for direction, count := range map[string]int64{"uplink": 7, "downlink": 11} {
		counter := manager.GetCounter(privateUsageName(t, otherUnreservedAccount, "2", direction, 1500000000, userQuotaScale))
		if counter == nil || counter.Value() != count {
			t.Fatal("unlimited usage lost or weighted twice", direction, counter)
		}
	}
}

func TestUnreservedUsageFollowsCoefficientAndAccountAcrossPolicyChanges(t *testing.T) {
	policy := userQuotaFixture(t, "100", 1500000000)
	group := policy.Groups["user:global"]
	group.UnreservedAccounts = map[string]string{"2": unreservedAccount}
	policy.Groups["user:global"] = group
	if !publishHostPolicy(policy) {
		t.Fatal("policy rejected")
	}
	tracker, manager := usageTrackerFixture(t)
	admit := func(size int) {
		t.Helper()
		if err := waitHostPolicyAndAccount(policyContext("ordinary"), "2", "up", size, nil, tracker); err != nil {
			t.Fatal(err)
		}
	}
	admit(5)
	old := privateUsageName(t, unreservedAccount, "2", "uplink", 1500000000, userQuotaScale)
	// An outside-grant period can span a coefficient change and a quota reset.
	policy.Generation = "second"
	policy.LegacyQuotaMultipliers["user:global"] = 750000000
	if !publishHostPolicy(policy) {
		t.Fatal("changed price rejected")
	}
	admit(3)
	middle := privateUsageName(t, unreservedAccount, "2", "uplink", 750000000, userQuotaScale)
	policy.Generation = "third"
	group.UnreservedAccounts["2"] = otherUnreservedAccount
	policy.Groups["user:global"] = group
	if !publishHostPolicy(policy) {
		t.Fatal("changed account rejected")
	}
	admit(7)
	latest := privateUsageName(t, otherUnreservedAccount, "2", "uplink", 750000000, userQuotaScale)
	if manager.GetCounter(old).Value() != 5 || manager.GetCounter(middle).Value() != 3 || manager.GetCounter(latest).Value() != 7 {
		t.Fatal("historical price/account was overwritten")
	}
	hostPolicies.Lock()
	pruneUnreservedUsageLocked(policy, manager)
	hostPolicies.Unlock()
	if manager.GetCounter(old) == nil || manager.GetCounter(middle) == nil {
		t.Fatal("uncollected debt was pruned")
	}
	manager.GetCounter(old).Set(0)
	manager.GetCounter(middle).Set(0)
	manager.GetCounter(latest).Set(0)
	hostPolicies.Lock()
	pruneUnreservedUsageLocked(policy, manager)
	hostPolicies.Unlock()
	if manager.GetCounter(old) != nil || manager.GetCounter(middle) != nil || manager.GetCounter(latest) == nil {
		t.Fatal("obsolete debt not collected or live handle removed")
	}
	// Reusing an account/price in a fresh policy must register a new handle.
	policy.Generation = "fourth"
	group.UnreservedAccounts["2"] = unreservedAccount
	policy.Groups["user:global"] = group
	if !publishHostPolicy(policy) {
		t.Fatal("reused account rejected")
	}
	admit(13)
	if manager.GetCounter(middle).Value() != 13 {
		t.Fatal("connection retained an unregistered counter")
	}
}

func TestUnreservedRecoveryUsesGrossUserPriceWithoutSpendingGlobalGrant(t *testing.T) {
	policy := userQuotaFixture(t, "100", 1500000000)
	group := policy.Groups["user:global"]
	group.UnreservedAccounts = map[string]string{"1": unreservedAccount}
	policy.Groups["user:global"] = group
	access := policy.Hosts[hostA]
	access.UserQuotaExempt = true
	access.Groups = []string{"a"}
	policy.Hosts[hostA] = access
	if !publishHostPolicy(policy) {
		t.Fatal("recovery policy rejected")
	}
	tracker, manager := usageTrackerFixture(t)
	if err := waitHostPolicyAndAccount(policyContext("shared"), hostEmail("1", hostA), "down", 9, nil, tracker); err != nil {
		t.Fatal(err)
	}
	entry := localQuota.entries[group.Reserves["1"].ID]
	if entry.consumed != 0 {
		t.Fatal("recovery spent the normal grant")
	}
	name := privateUsageName(t, unreservedAccount, "1", "downlink", 1500000000, userQuotaScale)
	if manager.GetCounter(name) == nil || manager.GetCounter(name).Value() != 9 {
		t.Fatal("recovery debt missing")
	}
}

type failedUsageManager struct {
	stats.Manager
	failures int
}

func (manager *failedUsageManager) GetOrRegisterCounter(name string) (stats.Counter, error) {
	manager.failures++
	if manager.failures == 2 {
		return nil, errors.New("injected registry failure")
	}
	return manager.Manager.GetOrRegisterCounter(name)
}

func TestUnreservedMultipleScopesPrepareAllCountersBeforeCharging(t *testing.T) {
	tracker, manager := usageTrackerFixture(t)
	tracker.manager = &failedUsageManager{Manager: manager}
	policy := hostPolicyDocument{Generation: "multiple", LegacyQuotaMultipliers: map[string]int64{"host:a": 150, "tag:b": 200}}
	groups := map[string]hostLimitPolicy{
		"host:a": {UnreservedAccounts: map[string]string{"1": unreservedAccount}},
		"tag:b":  {UnreservedAccounts: map[string]string{"1": otherUnreservedAccount}},
	}
	if err := tracker.accountLocked(policy, "", "1", "up", groups, 17); err == nil {
		t.Fatal("failed registration accepted")
	}
	manager.VisitCounters(func(name string, counter stats.Counter) bool {
		if counter.Value() != 0 {
			t.Fatal("partial metadata increment", name)
		}
		return true
	})
	if err := tracker.accountLocked(policy, "", "1", "up", groups, 17); err != nil {
		t.Fatal(err)
	}
	count := 0
	manager.VisitCounters(func(name string, counter stats.Counter) bool {
		count++
		if counter.Value() != 17 {
			t.Fatal("retry duplicated or missed debt", name)
		}
		return true
	})
	if count != 2 {
		t.Fatal("scope missing")
	}
}

func TestUnreservedUsageRequiresRealManagerAndStrictPolicyBindings(t *testing.T) {
	policy := userQuotaFixture(t, "100", 1500000000)
	group := policy.Groups["user:global"]
	group.UnreservedAccounts = map[string]string{"2": unreservedAccount}
	policy.Groups["user:global"] = group
	if !publishHostPolicy(policy) {
		t.Fatal("valid policy rejected")
	}
	called := false
	if err := waitHostPolicyAndAccount(policyContext("ordinary"), "2", "up", 1, func() { called = true }); err == nil || called {
		t.Fatal("unmetered traffic admitted")
	}
	tracker := &unreservedUsage{manager: stats.NoopManager{}}
	if err := waitHostPolicyAndAccount(policyContext("ordinary"), "2", "up", 1, nil, tracker); err == nil {
		t.Fatal("noop manager proved usage")
	}
	for _, accounts := range []map[string]string{
		{"02": unreservedAccount}, {"9223372036854775808": unreservedAccount}, {"2": "00000000-0000-0000-0000-000000000000"}, {"1": unreservedAccount, "2": unreservedAccount},
	} {
		group.UnreservedAccounts = accounts
		policy.Groups["user:global"] = group
		if validateUnreservedUsage(policy) == nil {
			t.Fatal("invalid binding accepted", accounts)
		}
	}
}

func TestUnreservedUsageSteadyPacketsDoNotAllocateOrRegisterAgain(t *testing.T) {
	tracker, manager := usageTrackerFixture(t)
	groups := map[string]hostLimitPolicy{"tag:a": {UnreservedAccounts: map[string]string{"1": unreservedAccount}}}
	policy := hostPolicyDocument{Generation: "stable", LegacyQuotaMultipliers: map[string]int64{"tag:a": 150}, Groups: groups}
	if err := tracker.accountLocked(policy, "", "1", "up", groups, 1); err != nil {
		t.Fatal(err)
	}
	allocations := testing.AllocsPerRun(1000, func() {
		if err := tracker.accountLocked(policy, "", "1", "up", groups, 1); err != nil {
			t.Fatal(err)
		}
	})
	if allocations != 0 {
		t.Fatal("steady traffic allocates metadata per packet", allocations)
	}
	registered := 0
	manager.VisitCounters(func(_ string, _ stats.Counter) bool { registered++; return true })
	if registered != 1 {
		t.Fatal("steady packets register more counters", registered)
	}
}
