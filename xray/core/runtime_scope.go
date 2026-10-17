package core

import (
	"context"
	"reflect"
	"sync"

	"github.com/xtls/xray-core/common/errors"
	clog "github.com/xtls/xray-core/common/log"
	"github.com/xtls/xray-core/features"
	"google.golang.org/protobuf/proto"
)

// RuntimeAccess coordinates admission with a configuration commit. Scopes use
// the parent's barrier, and never start a second Xray server.
func (s *Instance) RuntimeAccess() *sync.RWMutex {
	if s.scopeParent != nil {
		return s.scopeParent.RuntimeAccess()
	}
	return &s.runtimeAccess
}

func (s *Instance) Configuration() *Config {
	s.configAccess.RLock()
	defer s.configAccess.RUnlock()
	if s.initialConfig == nil {
		return nil
	}
	config := proto.Clone(s.initialConfig).(*Config)
	if s.RequiresUserQuota() {
		_ = RequireUserQuotaConfig(config)
	}
	return config
}

func (s *Instance) RecordConfiguration(config *Config) {
	s.configAccess.Lock()
	defer s.configAccess.Unlock()
	s.initialConfig = proto.Clone(config).(*Config)
	if s.RequiresUserQuota() {
		if required, err := ConfigRequiresUserQuota(s.initialConfig); err == nil && !required {
			_ = RequireUserQuotaConfig(s.initialConfig)
		}
	}
	s.hostPolicyRequired.Store(hostPolicyTags(s.initialConfig))
	if required, err := ConfigRequiresUserQuota(s.initialConfig); err == nil && required {
		s.userQuotaRequired.Store(true)
	}
}

type FeatureScope struct {
	Instance  *Instance
	owned     []features.Feature
	closeOnce sync.Once
	closeErr  error
}

func (s *Instance) RuntimeContext() context.Context { return toContext(s.ctx, s) }

func (s *Instance) BeforeClose(callback func()) {
	s.lifecycleAccess.Lock()
	if s.closing {
		s.lifecycleAccess.Unlock()
		callback()
		return
	}
	s.beforeClose = append(s.beforeClose, callback)
	s.lifecycleAccess.Unlock()
}

// Root shutdown must release waiters in all still-draining feature generations.
// Closing an individual generation does not terminate the parent data plane.
func (s *Instance) RuntimeDone() <-chan struct{} {
	if s.scopeParent != nil {
		return s.scopeParent.RuntimeDone()
	}
	s.lifecycleAccess.Lock()
	defer s.lifecycleAccess.Unlock()
	if s.shutdown == nil {
		s.shutdown = make(chan struct{})
		if s.closing {
			close(s.shutdown)
		}
	}
	return s.shutdown
}

func RuntimeContext(ctx, generation context.Context) context.Context {
	return toContext(ctx, MustFromContext(generation))
}

// NewFeatureScope borrows existing policy, statistics and DNS features. Only
// explicitly added features belong to the generation's teardown.
func NewFeatureScope(parent *Instance, replacedTypes ...interface{}) *FeatureScope {
	return newFeatureScope(parent, parent.isolatedProfile, replacedTypes...)
}

// NewIsolatedFeatureScope owns context logging as well as its replacement
// features; constructing a new logger must not replace the process logger.
func NewIsolatedFeatureScope(parent *Instance, replacedTypes ...interface{}) *FeatureScope {
	return newFeatureScope(parent, true, replacedTypes...)
}

func newFeatureScope(parent *Instance, isolated bool, replacedTypes ...interface{}) *FeatureScope {
	removed := make(map[reflect.Type]bool, len(replacedTypes))
	for _, typ := range replacedTypes {
		removed[reflect.TypeOf(typ)] = true
	}
	instance := &Instance{scopeParent: parent, isolatedProfile: isolated}
	instance.ctx = toContext(parent.ctx, instance)
	parent.resolveLock.Lock()
	for _, feature := range parent.features {
		if !removed[reflect.TypeOf(feature.Type())] {
			instance.features = append(instance.features, feature)
			if handler, ok := feature.(clog.Handler); ok {
				instance.loggingHandler.Store(&handler)
			}
		}
	}
	parent.resolveLock.Unlock()
	return &FeatureScope{Instance: instance}
}

func (s *FeatureScope) Context() context.Context { return s.Instance.ctx }

func (s *FeatureScope) Add(feature features.Feature) error {
	s.owned = append(s.owned, feature)
	return s.Instance.AddFeature(feature)
}

func (s *FeatureScope) Start() error {
	if len(s.Instance.pendingResolutions) != 0 {
		return errors.New("unresolved runtime dependencies")
	}
	for _, feature := range s.owned {
		if preparer, ok := feature.(features.StartPreparer); ok {
			if err := preparer.PrepareStart(); err != nil {
				return err
			}
		}
	}
	for _, feature := range s.owned {
		if err := feature.Start(); err != nil {
			return err
		}
	}
	s.Instance.statusLock.Lock()
	s.Instance.running = true
	s.Instance.statusLock.Unlock()
	return nil
}

func (s *FeatureScope) Close() error {
	s.closeOnce.Do(func() {
		s.Instance.statusLock.Lock()
		s.Instance.running = false
		s.Instance.statusLock.Unlock()
		var errs []error
		for i := len(s.owned) - 1; i >= 0; i-- {
			errs = append(errs, s.owned[i].Close())
		}
		s.closeErr = errors.Combine(errs...)
	})
	return s.closeErr
}
