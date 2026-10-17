//go:build !wasm

package dispatcher

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/xtls/xray-core/common/quotausage"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/stats"
)

// A connection only caches the current policy generation. Old epochs and
// coefficients cannot grow its cache for the lifetime of a long-lived stream.
// Access, including pruning, is serialized by hostPolicies.Mutex.
type unreservedUsage struct {
	manager                 stats.Manager
	generation, host, owner string
	ready                   [2]bool
	counters                [2][]stats.Counter
}

var unreservedManagers = make(map[stats.Manager]bool)
var unreservedLastPrune time.Time

func newUnreservedUsage(ctx context.Context) *unreservedUsage {
	tracker := &unreservedUsage{}
	if instance := core.FromContext(ctx); instance != nil {
		owner := instance.AccountingOwner()
		tracker.manager, _ = owner.GetFeature(stats.ManagerType()).(stats.Manager)
		// A managed profile can end while the shared financial source stays live.
		if tracker.manager != nil && reflect.TypeOf(tracker.manager).Comparable() && owner.RuntimeDone() != nil {
			hostPolicies.Lock()
			if !unreservedManagers[tracker.manager] {
				unreservedManagers[tracker.manager] = true
				go func(manager stats.Manager, done <-chan struct{}) {
					<-done
					hostPolicies.Lock()
					delete(unreservedManagers, manager)
					hostPolicies.Unlock()
				}(tracker.manager, owner.RuntimeDone())
			}
			hostPolicies.Unlock()
		}
	}
	return tracker
}

// Recovery access deliberately bypasses the global grant. Its gross traffic is
// still billed, so the global account must record this outside-grant liability.
func unreservedBindings(policy hostPolicyDocument, host, owner, direction string, groups map[string]hostLimitPolicy, size int) ([]quotausage.Counter, error) {
	if size == 0 {
		return nil, nil
	}
	if size < 0 {
		return nil, errors.New("invalid unreserved usage size")
	}
	if direction == "up" {
		direction = "uplink"
	} else if direction == "down" {
		direction = "downlink"
	} else {
		return nil, errors.New("invalid unreserved usage direction")
	}
	var result []quotausage.Counter
	appendGroup := func(key string, group hostLimitPolicy, pricingHost string, exempt bool) error {
		account := group.UnreservedAccounts[owner]
		if account == "" || (!exempt && group.ReserveRequired && !group.UnlimitedOwners[owner]) {
			return nil
		}
		factor, scale, err := quotaChargeFactor(policy, pricingHost, key, group, size)
		if err != nil {
			return err
		}
		if factor == 0 {
			return nil
		}
		counter := quotausage.Counter{Account: account, Owner: owner, Direction: direction, Factor: factor, Scale: scale}
		if _, err := counter.Name(); err != nil {
			return err
		}
		result = append(result, counter)
		return nil
	}
	for key, group := range groups {
		if err := appendGroup(key, group, host, false); err != nil {
			return nil, err
		}
	}
	if host != "" && policy.Hosts[host].UserQuotaExempt {
		for _, key := range policy.LegacyGroups {
			if _, exists := groups[key]; exists {
				return nil, errors.New("ambiguous recovery usage binding")
			}
			if err := appendGroup(key, policy.Groups[key], "", true); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

func (tracker *unreservedUsage) accountLocked(policy hostPolicyDocument, host, owner, direction string, groups map[string]hostLimitPolicy, size int) error {
	if size == 0 {
		return nil
	}
	if size < 0 {
		return errors.New("invalid unreserved usage size")
	}
	var index int
	switch direction {
	case "up":
		index = 0
	case "down":
		index = 1
	default:
		return errors.New("invalid unreserved usage direction")
	}
	// Authenticated identity and group membership are immutable within a policy
	// generation. Avoid names, registry lookups and allocations for every packet.
	if tracker != nil && tracker.generation == policy.Generation && tracker.host == host && tracker.owner == owner && tracker.ready[index] {
		for _, counter := range tracker.counters[index] {
			counter.Add(int64(size))
		}
		return nil
	}
	bindings, err := unreservedBindings(policy, host, owner, direction, groups, size)
	if err != nil {
		return err
	}
	if len(bindings) > 0 && (tracker == nil || tracker.manager == nil) {
		return errors.New("unreserved usage statistics unavailable")
	}
	if tracker == nil {
		return nil
	}
	if tracker.generation != policy.Generation || tracker.host != host || tracker.owner != owner {
		tracker.generation, tracker.host, tracker.owner = policy.Generation, host, owner
		tracker.ready = [2]bool{}
		tracker.counters = [2][]stats.Counter{}
	}
	var prepared []stats.Counter
	for _, binding := range bindings {
		name, err := binding.Name()
		if err != nil {
			return err
		}
		counter, err := tracker.manager.GetOrRegisterCounter(name)
		if err != nil || counter == nil {
			return errors.New("unreserved usage counter registration failed")
		}
		prepared = append(prepared, counter)
	}
	// Prepare every counter before modifying any, so a failed group cannot leave
	// half a metadata charge. These are raw bytes; the name fixes their price.
	tracker.counters[index], tracker.ready[index] = prepared, true
	for _, counter := range prepared {
		counter.Add(int64(size))
	}
	return nil
}

func validateUnreservedUsage(policy hostPolicyDocument) error {
	seen := make(map[string]bool)
	for _, group := range policy.Groups {
		if len(group.UnreservedAccounts) == 0 {
			continue
		}
		if policy.OfflineReserves != quotaReserveCapability || len(group.UnreservedAccounts) > 200000 {
			return errors.New("invalid unreserved usage policy")
		}
		for owner, account := range group.UnreservedAccounts {
			binding := quotausage.Counter{Account: account, Owner: owner, Direction: "uplink", Factor: 1, Scale: quotaScale(group.MultiplierScale)}
			if _, err := binding.Name(); err != nil || seen[account] {
				return errors.New("invalid or duplicate unreserved usage account")
			}
			seen[account] = true
			if len(seen) > 200000 {
				return errors.New("too many unreserved usage accounts")
			}
		}
	}
	_, err := unreservedGroupPrices(policy)
	return err
}

// Index every actual host/group reference once. Policy validation and pruning
// must not perform a groups-by-hosts Cartesian scan under the admission lock.
func unreservedGroupPrices(policy hostPolicyDocument) (map[string]map[[2]int64]bool, error) {
	prices := make(map[string]map[[2]int64]bool)
	for key, group := range policy.Groups {
		if len(group.UnreservedAccounts) > 0 {
			prices[key] = make(map[[2]int64]bool)
		}
	}
	add := func(key, host string) error {
		factors, tracked := prices[key]
		if !tracked {
			return nil
		}
		factor, scale, err := quotaChargeFactor(policy, host, key, policy.Groups[key], 0)
		if err != nil {
			return err
		}
		factors[[2]int64{factor, scale}] = true
		return nil
	}
	for _, key := range policy.LegacyGroups {
		if err := add(key, ""); err != nil {
			return nil, err
		}
	}
	for host, access := range policy.Hosts {
		for _, key := range access.Groups {
			if err := add(key, host); err != nil {
				return nil, err
			}
		}
	}
	return prices, nil
}

// Keep current zero counters: active streams may cache their handles. Obsolete
// nonzero counters survive until the durable receipt collector captures them.
func pruneUnreservedUsageLocked(policy hostPolicyDocument, manager stats.Manager) {
	type binding struct {
		owner   string
		factors map[[2]int64]bool
	}
	prices, err := unreservedGroupPrices(policy)
	if err != nil {
		return
	}
	accounts := make(map[string]binding)
	for key, group := range policy.Groups {
		for owner, account := range group.UnreservedAccounts {
			accounts[account] = binding{owner: owner, factors: prices[key]}
		}
	}

	var stale []string
	manager.VisitCounters(func(name string, counter stats.Counter) bool {
		if !strings.HasPrefix(name, quotausage.Prefix) || counter.Value() != 0 {
			return true
		}
		value, err := quotausage.Parse(name)
		if err != nil {
			return true
		} // malformed names remain visible to receipt validation
		current, exists := accounts[value.Account]
		if !exists || current.owner != value.Owner || !current.factors[[2]int64{value.Factor, value.Scale}] {
			stale = append(stale, name)
		}
		return true
	})
	for _, name := range stale {
		_ = manager.UnregisterCounter(name)
	}
}
