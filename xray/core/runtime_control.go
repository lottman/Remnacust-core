package core

import (
	"context"
	"github.com/xtls/xray-core/features"
)

// RuntimeController serializes management operations with native port commits.
// Readers hold a generation lease for the duration of the callback.
type RuntimeController interface {
	Update(context.Context, func(*Config) error) error
	View(context.Context, interface{}, func(features.Feature) error) error
}

// Lazy native fleet controllers cannot overwrite a profile's existing API
// controller or be installed after its shutdown has begun.
func (s *Instance) InstallRuntimeController(candidate RuntimeController, beforeClose func()) (RuntimeController, bool) {
	if candidate == nil || beforeClose == nil {
		return nil, false
	}
	s.lifecycleAccess.Lock()
	defer s.lifecycleAccess.Unlock()
	if s.closing {
		return nil, false
	}
	if s.runtimeController != nil {
		return s.runtimeController, false
	}
	s.runtimeController = candidate
	s.beforeClose = append(s.beforeClose, beforeClose)
	return candidate, true
}

func (s *Instance) SetRuntimeController(controller RuntimeController) {
	s.lifecycleAccess.Lock()
	s.runtimeController = controller
	s.lifecycleAccess.Unlock()
}

func (s *Instance) RuntimeController() RuntimeController {
	if s.scopeParent != nil {
		return s.scopeParent.RuntimeController()
	}
	s.lifecycleAccess.Lock()
	defer s.lifecycleAccess.Unlock()
	return s.runtimeController
}
