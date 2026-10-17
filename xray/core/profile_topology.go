package core

import (
	"errors"
	"sort"

	"google.golang.org/protobuf/proto"
)

// CommitManagedProfileTopology publishes a complete, already prepared roster.
// Caller holds ManagementAccess from clock/CAS validation through this commit.
// The optional non-failing hook marks retired credentials while admission is
// still barred; interruption of their existing streams runs after this returns.
func (s *Instance) CommitManagedProfileTopology(target *Config, live map[string]*Instance, beforeAdmission ...func()) error {
	if len(beforeAdmission) > 1 || len(beforeAdmission) == 1 && beforeAdmission[0] == nil {
		return errors.New("invalid profile admission hook")
	}
	p := s.profilePlan
	if p == nil || s.AccountingOwner() != s || !s.RuntimeManagementReady() || s.runtimeJournal == "" {
		return errors.New("an enrolled running profile owner and durable checkpoint are required")
	}
	bindings, enrolled, err := ConfigProfileBindings(target)
	if err != nil || !enrolled || len(bindings) != len(live) {
		return errors.New("profile topology does not match the prepared roster")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.retiring) > 512 {
		return errors.New("too many profiles are still draining")
	}
	previous := s.Configuration()
	control := func(config *Config) *Config {
		copy := proto.Clone(config).(*Config)
		apps := copy.App[:0]
		for _, app := range copy.App {
			if app.Type != ProfilePlanType && app.Type != ProfileDeliveryType && app.Type != UserQuotaRequirementType {
				apps = append(apps, app)
			}
		}
		copy.App = apps
		return copy
	}
	if !proto.Equal(control(previous), control(target)) {
		return errors.New("profile topology cannot change the root control configuration")
	}
	oldClock, err := ConfigProfileDelivery(previous)
	if err != nil {
		return err
	}
	nextClock, err := ConfigProfileDelivery(target)
	if err != nil {
		return err
	}
	if nextClock == nil {
		return errors.New("a versioned profile delivery is required")
	}
	digest, err := ProfilePlanDigest(target)
	if err != nil || nextClock.PlanHash != digest {
		return errors.New("profile topology source digest mismatch")
	}
	if err := CheckProfileDeliveryAdvance(oldClock, nextClock); err != nil {
		return err
	}
	if oldClock != nil && nextClock != nil && *oldClock == *nextClock {
		return errors.New("topology commit requires a fresh delivery clock")
	}
	var activate, retire []*Instance
	seen := make(map[*Instance]bool, len(bindings))
	for index, binding := range bindings {
		instance := live[binding.BindingID]
		if instance == nil || instance == s || seen[instance] || instance.AccountingOwner() != s || !instance.profilePrepared() {
			return errors.New("prepared profile is not running under this owner")
		}
		seen[instance] = true
		config := instance.Configuration()
		expected := proto.Clone(binding.Config).(*Config)
		if instance.RequiresUserQuota() {
			if err := RequireUserQuotaConfig(expected); err != nil {
				return err
			}
		}
		if !proto.Equal(config, expected) {
			return errors.New("prepared profile changed before topology publication")
		}
		bindings[index].Config = config
		if p.live[binding.BindingID] != instance {
			if instance.boundProfilePlan != nil || !instance.profilePreparing.Load() {
				return errors.New("prepared profile already belongs to another roster")
			}
			activate = append(activate, instance)
		}
	}
	ids := make([]string, 0, len(p.live))
	for id := range p.live {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if live[id] != p.live[id] {
			retire = append(retire, p.live[id])
		}
	}
	if len(p.retiring)+len(retire) > 512 {
		return errors.New("too many profiles would remain draining")
	}
	actual, err := WithProfileBindings(target, bindings)
	if err != nil {
		return err
	}
	if s.RequiresUserQuota() {
		if err := RequireUserQuotaConfig(actual); err != nil {
			return err
		}
	}
	// Block cached multiplex/QUIC admission while replacing profile ownership.
	barriers := append(append([]*Instance(nil), activate...), retire...)
	for _, instance := range barriers {
		instance.RuntimeAccess().Lock()
	}
	defer func() {
		for i := len(barriers) - 1; i >= 0; i-- {
			barriers[i].RuntimeAccess().Unlock()
		}
	}()
	err = s.commitProfileTCPListeners(activate, retire, func() error {
		if err := SaveRuntimeJournal(s.runtimeJournal, actual); err != nil {
			if restore := SaveRuntimeJournal(s.runtimeJournal, previous); restore != nil {
				s.FenceRuntimeAdmission()
				return errors.New("profile topology checkpoint is uncertain; admission fenced for recovery")
			}
			return err
		}
		return nil
	}, false)
	if err != nil {
		return err
	}
	// No fallible operations follow the durable socket decision. Root shutdown
	// can see the instances already registered by NewManagedProfile.
	s.lifecycleAccess.Lock()
	for _, binding := range bindings {
		instance := live[binding.BindingID]
		instance.boundProfilePlan = p
		instance.profileBindingID = binding.BindingID
	}
	s.lifecycleAccess.Unlock()
	if p.retiring == nil {
		p.retiring = make(map[*Instance]struct{})
	}
	for _, instance := range retire {
		instance.profileRetired.Store(true)
		p.retiring[instance] = struct{}{}
	}
	p.bindings = bindings
	p.live = make(map[string]*Instance, len(live))
	for id, instance := range live {
		p.live[id] = instance
	}
	s.RecordConfiguration(actual)
	if len(beforeAdmission) == 1 {
		beforeAdmission[0]()
	}
	for _, instance := range activate {
		instance.profilePreparing.Store(false)
	}
	for _, instance := range retire {
		go func(instance *Instance) {
			select {
			case <-instance.TCPProfileDrained():
			case <-s.RuntimeDone():
			}
			_ = instance.Close()
			p.mu.Lock()
			delete(p.retiring, instance)
			p.mu.Unlock()
		}(instance)
	}
	return nil
}

// RelatedManagedProfiles includes the current graph and retired graphs of the
// same binding. Their authenticated links remain revocable while they drain.
func (s *Instance) RelatedManagedProfiles() []*Instance {
	if s.scopeParent != nil {
		return s.scopeParent.RelatedManagedProfiles()
	}
	owner := s.AccountingOwner()
	owner.lifecycleAccess.Lock()
	defer owner.lifecycleAccess.Unlock()
	if s.profileBindingID == "" {
		return []*Instance{s}
	}
	profiles := make([]*Instance, 0, 2)
	for instance := range owner.managedProfiles {
		if instance.profileBindingID == s.profileBindingID {
			profiles = append(profiles, instance)
		}
	}
	return profiles
}

// RetiredManagedProfiles also includes bindings no longer present in the live
// manifest. A node-wide device revocation must still reach their transports.
func (s *Instance) RetiredManagedProfiles() []*Instance {
	owner := s.AccountingOwner()
	owner.lifecycleAccess.Lock()
	defer owner.lifecycleAccess.Unlock()
	var profiles []*Instance
	for instance := range owner.managedProfiles {
		if instance.profileRetired.Load() {
			profiles = append(profiles, instance)
		}
	}
	return profiles
}
