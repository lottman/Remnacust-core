package core

import (
	"context"
	"sort"
	"strconv"

	"github.com/xtls/xray-core/features/stats"
)

// This namespace is never part of user/quota counters or legacy inbound tags.
const BindingTrafficPrefix = "binding-traffic>>>"

type BindingTrafficSample struct {
	BindingID          string
	Uplink, Downlink   string
	Available, Retired bool
}

// BindingTrafficRecords includes bindings whose old transports still drain.
// Values are observations only; they never issue, release or settle credit.
func (s *Instance) BindingTrafficRecords() ([]BindingTrafficSample, bool) {
	p := s.profilePlan
	if p == nil {
		return nil, false
	}
	p.mu.Lock()
	ids := make(map[string]bool, len(p.bindings)+len(p.retiring))
	for _, binding := range p.bindings {
		ids[binding.BindingID] = false
	}
	for instance := range p.retiring {
		if _, exists := ids[instance.profileBindingID]; !exists {
			ids[instance.profileBindingID] = true
		}
	}
	p.mu.Unlock()
	keys := make([]string, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	records := make([]BindingTrafficSample, 0, len(keys))
	for _, id := range keys {
		up, down, available := s.BindingTrafficSnapshot(id)
		records = append(records, BindingTrafficSample{BindingID: id, Uplink: up, Downlink: down, Available: available, Retired: ids[id]})
	}
	return records, true
}

type bindingTrafficCounters struct {
	up, down stats.Counter
}

type bindingTrafficCounter struct {
	aggregate, binding stats.Counter
}

func (c *bindingTrafficCounter) Add(value int64) int64 {
	c.binding.Add(value)
	return c.aggregate.Add(value)
}
func (c *bindingTrafficCounter) Value() int64 { return c.aggregate.Value() }

// Legacy aggregate resets do not reset cumulative binding measurements.
func (c *bindingTrafficCounter) Set(value int64) int64 { return c.aggregate.Set(value) }

// ProfileTrafficCounters wraps only native inbound counters. The shared stats
// manager and user-accounting counters retain their original identity.
func ProfileTrafficCounters(ctx context.Context, up, down stats.Counter) (stats.Counter, stats.Counter) {
	if up == nil && down == nil {
		return up, down
	}
	instance := FromContext(ctx)
	if instance == nil {
		return up, down
	}
	for instance.scopeParent != nil {
		instance = instance.scopeParent
	}
	instance.bindingTrafficOnce.Do(func() {
		owner := instance.AccountingOwner()
		owner.lifecycleAccess.Lock()
		id := instance.profileBindingID
		owner.lifecycleAccess.Unlock()
		if id == "" {
			return
		}
		manager, ok := owner.GetFeature(stats.ManagerType()).(stats.Manager)
		if !ok {
			return
		}
		u, err := manager.GetOrRegisterCounter(BindingTrafficPrefix + id + ">>>uplink")
		if err != nil {
			return
		}
		d, err := manager.GetOrRegisterCounter(BindingTrafficPrefix + id + ">>>downlink")
		if err != nil {
			return
		}
		instance.bindingTraffic = bindingTrafficCounters{up: u, down: d}
	})
	if up != nil && instance.bindingTraffic.up != nil {
		up = &bindingTrafficCounter{aggregate: up, binding: instance.bindingTraffic.up}
	}
	if down != nil && instance.bindingTraffic.down != nil {
		down = &bindingTrafficCounter{aggregate: down, binding: instance.bindingTraffic.down}
	}
	return up, down
}

func (s *Instance) BindingTrafficSnapshot(id string) (uplink, downlink string, available bool) {
	if !canonicalProfileUUID(id) {
		return "0", "0", false
	}
	manager, ok := s.AccountingOwner().GetFeature(stats.ManagerType()).(stats.Manager)
	if !ok {
		return "0", "0", false
	}
	up, down := manager.GetCounter(BindingTrafficPrefix+id+">>>uplink"), manager.GetCounter(BindingTrafficPrefix+id+">>>downlink")
	if up == nil || down == nil {
		return "0", "0", false
	}
	u, d := up.Value(), down.Value()
	if u < 0 || d < 0 {
		return "0", "0", false
	}
	return strconv.FormatInt(u, 10), strconv.FormatInt(d, 10), true
}

// Called under the owner's lifecycle lock after the closing graph unregisters.
func (s *Instance) releaseBindingTrafficLocked(id string) {
	if s.closing || id == "" {
		return
	}
	for instance := range s.managedProfiles {
		if instance.profileBindingID == id {
			return
		}
	}
	if manager, ok := s.GetFeature(stats.ManagerType()).(stats.Manager); ok {
		_ = manager.UnregisterCounter(BindingTrafficPrefix + id + ">>>uplink")
		_ = manager.UnregisterCounter(BindingTrafficPrefix + id + ">>>downlink")
	}
}
