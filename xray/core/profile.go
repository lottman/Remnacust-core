package core

import (
	"context"
	"reflect"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/log"
	"github.com/xtls/xray-core/features"
	"github.com/xtls/xray-core/features/stats"
	"google.golang.org/protobuf/proto"
)

// NewProfile owns a complete independent feature graph in the current process.
// It does not reload process environment or replace the process DNS/dialer/log.
// The caller owns Start/Close and must coordinate listener admission and drain.
func NewProfile(ctx context.Context, config *Config) (*Instance, error) {
	return newProfile(ctx, config, nil, false)
}

// NewManagedProfile owns an independent graph while borrowing the accounting
// manager of its parent. Closing a profile never starts/closes that manager.
// Parent shutdown closes registered profiles before closing its own features.
func NewManagedProfile(parent *Instance, config *Config) (*Instance, error) {
	return newManagedProfile(parent, config, false)
}

// NewStagedManagedProfile prepares a replacement graph without accepting new
// TCP connections until its owning durable delivery is committed.
func NewStagedManagedProfile(parent *Instance, config *Config) (*Instance, error) {
	return newManagedProfile(parent, config, true)
}

func newManagedProfile(parent *Instance, config *Config, staged bool) (*Instance, error) {
	if parent == nil || parent.ctx == nil || parent.scopeParent != nil {
		return nil, errors.New("a managed profile requires an owning accounting instance")
	}
	finished, accepted := parent.TrackRuntimeWorker(func() {})
	if !accepted {
		return nil, errors.New("the managed profile accounting owner is shutting down")
	}
	defer finished()
	return newProfile(parent.RuntimeContext(), config, parent, staged)
}

func (s *Instance) AccountingOwner() *Instance {
	if s.scopeParent != nil {
		return s.scopeParent.AccountingOwner()
	}
	if s.accountingParent != nil {
		return s.accountingParent.AccountingOwner()
	}
	return s
}

func (s *Instance) borrowsAccounting(feature features.Feature) bool {
	return s.accountingParent != nil && reflect.TypeOf(feature.Type()) == reflect.TypeOf(stats.ManagerType())
}

func newProfile(ctx context.Context, config *Config, accountingParent *Instance, staged bool) (*Instance, error) {
	if ctx == nil || config == nil {
		return nil, errors.New("a profile requires context and configuration")
	}
	parent := FromContext(ctx)
	// Copy before attaching mandatory server metadata: profile callers retain
	// their editable configuration, while checkpoints carry the requirement.
	config = proto.Clone(config).(*Config)
	if parent != nil && parent.RequiresUserQuota() {
		if err := RequireUserQuotaConfig(config); err != nil {
			return nil, err
		}
	}
	server := &Instance{ctx: log.ContextWithIsolatedHandler(ctx), isolatedProfile: true, quotaRequirementParent: parent, accountingParent: accountingParent, tcpListenersStaged: staged}
	server.profilePreparing.Store(staged)
	if accountingParent != nil {
		manager, ok := accountingParent.GetFeature(stats.ManagerType()).(stats.Manager)
		if !ok {
			return nil, errors.New("the managed profile accounting manager is missing")
		}
		switch manager.(type) {
		case stats.NoopManager, *stats.NoopManager:
			return nil, errors.New("managed profiles require enabled node accounting")
		}
		server.features = []features.Feature{manager}
	}
	if failed, err := initInstanceWithConfig(config, server); failed {
		_ = server.Close()
		return nil, err
	}
	if accountingParent != nil {
		accountingParent.lifecycleAccess.Lock()
		if accountingParent.closing {
			accountingParent.lifecycleAccess.Unlock()
			_ = server.Close()
			return nil, errors.New("the managed profile accounting owner is shutting down")
		}
		if accountingParent.managedProfiles == nil {
			accountingParent.managedProfiles = make(map[*Instance]struct{})
		}
		accountingParent.managedProfiles[server] = struct{}{}
		accountingParent.lifecycleAccess.Unlock()
	}
	return server, nil
}
func (s *Instance) LogHandler() log.Handler {
	if handler := s.loggingHandler.Load(); handler != nil {
		return *handler
	}
	return nil
}
