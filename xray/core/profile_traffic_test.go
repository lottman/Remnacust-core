package core

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xtls/xray-core/features"
	"github.com/xtls/xray-core/features/stats"
)

type profileTrafficTestCounter struct{ value atomic.Int64 }

func (c *profileTrafficTestCounter) Add(value int64) int64 { return c.value.Add(value) - value }
func (c *profileTrafficTestCounter) Set(value int64) int64 { return c.value.Swap(value) }
func (c *profileTrafficTestCounter) Value() int64          { return c.value.Load() }

type profileTrafficTestManager struct {
	stats.NoopManager
	mu       sync.Mutex
	counters map[string]stats.Counter
}

func (m *profileTrafficTestManager) GetOrRegisterCounter(name string) (stats.Counter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if counter := m.counters[name]; counter != nil {
		return counter, nil
	}
	counter := new(profileTrafficTestCounter)
	m.counters[name] = counter
	return counter, nil
}
func (m *profileTrafficTestManager) GetCounter(name string) stats.Counter {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counters[name]
}
func (m *profileTrafficTestManager) UnregisterCounter(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.counters, name)
	return nil
}

func TestProfileTrafficKeepsSharedUserAccountingAndSeparatesIdenticalInboundTags(t *testing.T) {
	manager := &profileTrafficTestManager{counters: make(map[string]stats.Counter)}
	owner := &Instance{ctx: context.Background(), features: []features.Feature{manager}, managedProfiles: make(map[*Instance]struct{})}
	const a = "11111111-1111-4111-8111-111111111111"
	const b = "22222222-2222-4222-8222-222222222222"
	profileA := &Instance{ctx: context.Background(), accountingParent: owner, profileBindingID: a, features: []features.Feature{manager}}
	profileB := &Instance{ctx: context.Background(), accountingParent: owner, profileBindingID: b, features: []features.Feature{manager}}
	aggregateUp, _ := manager.GetOrRegisterCounter("inbound>>>SAME>>>traffic>>>uplink")
	aggregateDown, _ := manager.GetOrRegisterCounter("inbound>>>SAME>>>traffic>>>downlink")
	user, _ := manager.GetOrRegisterCounter("user>>>identity>>>traffic>>>uplink")
	user.Add(101)
	upA, downA := ProfileTrafficCounters(profileA.RuntimeContext(), aggregateUp, aggregateDown)
	upB, downB := ProfileTrafficCounters(profileB.RuntimeContext(), aggregateUp, aggregateDown)
	upA.Add(7)
	downA.Add(11)
	upB.Add(13)
	downB.Add(17)
	if aggregateUp.Value() != 20 || aggregateDown.Value() != 28 {
		t.Fatal("legacy aggregate traffic was lost or counted twice")
	}
	if user.Value() != 101 {
		t.Fatal("port traffic changed user accounting")
	}
	if profileA.GetFeature(stats.ManagerType()) != profileB.GetFeature(stats.ManagerType()) {
		t.Fatal("authoritative accounting manager was replaced")
	}
	u, d, available := owner.BindingTrafficSnapshot(a)
	if !available || u != "7" || d != "11" {
		t.Fatal("first binding traffic was mixed", u, d, available)
	}
	u, d, available = owner.BindingTrafficSnapshot(b)
	if !available || u != "13" || d != "17" {
		t.Fatal("second binding traffic was mixed", u, d, available)
	}
	// Existing aggregate readers may reset their samples without resetting the
	// cumulative binding observations or the user's credit ledger.
	upA.Set(0)
	u, _, _ = owner.BindingTrafficSnapshot(a)
	if u != "7" || user.Value() != 101 {
		t.Fatal("aggregate reset changed binding/user totals")
	}
	// A later graph with the same binding continues the same cumulative stream.
	replacement := &Instance{ctx: context.Background(), accountingParent: owner, profileBindingID: a}
	upNew, _ := ProfileTrafficCounters(replacement.RuntimeContext(), aggregateUp, nil)
	upNew.Add(19)
	u, _, _ = owner.BindingTrafficSnapshot(a)
	if u != "26" {
		t.Fatal("replacement graph reset the binding's old traffic")
	}
}

func TestProfileTrafficCleanupPreservesOtherGenerationsAndUserCounters(t *testing.T) {
	manager := &profileTrafficTestManager{counters: make(map[string]stats.Counter)}
	const id = "11111111-1111-4111-8111-111111111111"
	owner := &Instance{ctx: context.Background(), features: []features.Feature{manager}, managedProfiles: make(map[*Instance]struct{})}
	profile := &Instance{ctx: context.Background(), accountingParent: owner, profileBindingID: id}
	base, _ := manager.GetOrRegisterCounter("inbound>>>SAME>>>traffic>>>uplink")
	up, _ := ProfileTrafficCounters(profile.RuntimeContext(), base, nil)
	up.Add(5)
	owner.managedProfiles[profile] = struct{}{}
	owner.lifecycleAccess.Lock()
	owner.releaseBindingTrafficLocked(id)
	owner.lifecycleAccess.Unlock()
	if _, _, available := owner.BindingTrafficSnapshot(id); !available {
		t.Fatal("a live generation lost its binding counters")
	}
	owner.lifecycleAccess.Lock()
	delete(owner.managedProfiles, profile)
	owner.releaseBindingTrafficLocked(id)
	owner.lifecycleAccess.Unlock()
	if _, _, available := owner.BindingTrafficSnapshot(id); available {
		t.Fatal("closed binding metrics survived without a generation")
	}
	if manager.GetCounter("inbound>>>SAME>>>traffic>>>uplink") != base || base.Value() != 5 {
		t.Fatal("binding cleanup touched legacy accounting")
	}
}
