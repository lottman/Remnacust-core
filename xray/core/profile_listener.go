package core

import (
	"context"
	"fmt"

	"github.com/xtls/xray-core/transport/internet"
)

// Shared physical decoders belong to the node, never to a retiring graph.
func (s *Instance) PacketSocketContext() context.Context { return s.AccountingOwner().RuntimeContext() }

// Runtime feature generations share their profile's logical socket ownership.
// The accounting root owns the physical sockets until process shutdown.
func (s *Instance) ListenerProfile() *internet.TCPListenerProfile {
	if s.scopeParent != nil {
		return s.scopeParent.ListenerProfile()
	}
	owner := s.AccountingOwner()
	if owner != s {
		owner.ListenerProfile()
	}
	s.tcpListenerOnce.Do(func() {
		if owner == s {
			s.tcpListenerPool = internet.NewTCPListenerPool()
		} else {
			s.tcpListenerPool = owner.tcpListenerPool
		}
		s.tcpListenerProfile = s.tcpListenerPool.NewProfile(s.tcpListenersStaged)
		s.BeforeClose(func() {
			_ = s.tcpListenerProfile.Close()
			if owner == s {
				_ = s.tcpListenerPool.Close()
			}
		})
	})
	return s.tcpListenerProfile
}

// CommitProfileTCPListeners keeps the socket decision atomic with a durable
// checkpoint callback. The owning management lock must cover the whole change.
// It does not replace UDP/QUIC ownership or the managed graph roster.
func (s *Instance) CommitProfileTCPListeners(activate, retire []*Instance, persist func() error) error {
	seen := make(map[*Instance]bool, len(activate))
	for _, instance := range activate {
		if instance == nil || seen[instance] {
			return fmt.Errorf("invalid duplicate profile activation")
		}
		seen[instance] = true
	}
	for _, instance := range activate {
		instance.RuntimeAccess().Lock()
	}
	defer func() {
		for i := len(activate) - 1; i >= 0; i-- {
			activate[i].RuntimeAccess().Unlock()
		}
	}()
	return s.commitProfileTCPListeners(activate, retire, persist, true)
}

func (s *Instance) commitProfileTCPListeners(activate, retire []*Instance, persist func() error, markReady bool) error {
	owner := s.AccountingOwner()
	convert := func(instances []*Instance, preparing bool) ([]*internet.TCPListenerProfile, error) {
		profiles := make([]*internet.TCPListenerProfile, 0, len(instances))
		for _, instance := range instances {
			if instance == nil || instance.AccountingOwner() != owner || instance.scopeParent != nil || !instance.profilePrepared() || !preparing && instance.ProfilePreparing() {
				return nil, fmt.Errorf("profile socket owner mismatch")
			}
			profiles = append(profiles, instance.ListenerProfile())
			if instance.ListenerProfile().HasPacketListeners() {
				return nil, fmt.Errorf("packet-flow ownership is required before replacing UDP or QUIC profiles")
			}
		}
		return profiles, nil
	}
	next, err := convert(activate, true)
	if err != nil {
		return err
	}
	old, err := convert(retire, false)
	if err != nil {
		return err
	}
	owner.ListenerProfile()
	if err := owner.tcpListenerPool.CommitSwitch(next, old, persist); err != nil {
		return err
	}
	if markReady {
		for _, instance := range activate {
			instance.profilePreparing.Store(false)
		}
	}
	return nil
}

func (s *Instance) TCPProfileDrained() <-chan struct{} { return s.ListenerProfile().Done() }
