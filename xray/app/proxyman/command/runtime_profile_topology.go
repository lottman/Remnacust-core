//go:build !wasm

package command

import (
	"context"

	"github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func profileRosterChanged(previous, next []core.ProfileBinding) bool {
	if len(previous) != len(next) {
		return true
	}
	for i, binding := range next {
		if binding.BindingID != previous[i].BindingID || binding.ProfileID != previous[i].ProfileID || binding.InboundID != previous[i].InboundID {
			return true
		}
	}
	return false
}

// User lists and generation-scoped routing/outbounds can use a runtime batch.
// Listener/proxy shape or process-local feature changes need a complete graph.
func profileGraphShape(config *core.Config) (*core.Config, error) {
	shape := proto.Clone(config).(*core.Config)
	shape.Outbound = nil
	apps := shape.App[:0]
	for _, app := range shape.App {
		if app.Type != core.UserQuotaRequirementType && !runtimeContextApp(app) {
			apps = append(apps, app)
		}
	}
	shape.App = apps
	for _, inbound := range shape.Inbound {
		settings, err := inbound.ProxySettings.GetInstance()
		if err != nil {
			return nil, err
		}
		if field := settings.ProtoReflect().Descriptor().Fields().ByName("users"); field != nil && field.IsList() {
			settings.ProtoReflect().Clear(field)
			inbound.ProxySettings.Value, err = proto.MarshalOptions{Deterministic: true}.Marshal(settings)
			if err != nil {
				return nil, err
			}
		}
	}
	return shape, nil
}

func profileGraphReplacementRequired(previous, next []core.ProfileBinding) bool {
	if profileRosterChanged(previous, next) {
		return true
	}
	for i, binding := range next {
		a, err := profileGraphShape(previous[i].Config)
		if err != nil {
			return true
		}
		b, err := profileGraphShape(binding.Config)
		if err != nil || !proto.Equal(a, b) {
			return true
		}
	}
	return false
}

func (s *handlerServer) applyProfileTopology(ctx context.Context, target *core.Config, expected string) (*structpb.Struct, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished, accepted := s.s.TrackRuntimeWorker(cancel)
	if !accepted {
		return nil, status.Error(codes.Unavailable, "The profile owner is shutting down")
	}
	defer finished()
	s.operation.Lock()
	var prepared []*core.Instance
	committed := false
	// Closing a native controller may acquire this same management lock.
	defer func() {
		s.operation.Unlock()
		if !committed {
			for _, instance := range prepared {
				_ = instance.Close()
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	current := s.s.Configuration()
	if !s.s.RuntimeManagementReady() {
		return nil, status.Error(codes.Unavailable, "The profile owner is not running")
	}
	if runtimeHash(current) != expected {
		return nil, status.Error(codes.Aborted, "The owning profile plan changed; inspect it again")
	}
	if !proto.Equal(profileRootControl(current), profileRootControl(target)) {
		return nil, status.Error(codes.FailedPrecondition, "A profile delivery cannot change the root control configuration")
	}
	previous, err := core.ConfigProfileDelivery(current)
	if err != nil {
		return nil, status.Error(codes.Internal, "The current delivery needs recovery")
	}
	next, err := core.ConfigProfileDelivery(target)
	if err != nil || next == nil {
		return nil, status.Error(codes.InvalidArgument, "A versioned delivery is required")
	}
	if err := core.CheckProfileDeliveryAdvance(previous, next); err != nil {
		return nil, status.Error(codes.Aborted, err.Error())
	}
	if previous != nil && *previous == *next {
		response, err := s.profileStatusLocked()
		if err == nil {
			response.Fields["unchanged"] = structpb.NewBoolValue(true)
		}
		return response, err
	}
	bindings, _, err := core.ConfigProfileBindings(target)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Invalid profile topology")
	}
	oldBindings, _, err := core.ConfigProfileBindings(current)
	if err != nil {
		return nil, status.Error(codes.Internal, "The current profile roster needs recovery")
	}
	old := make(map[string]core.ProfileBinding, len(oldBindings))
	for _, binding := range oldBindings {
		old[binding.BindingID] = binding
	}
	live := make(map[string]*core.Instance, len(bindings))
	for _, binding := range bindings {
		instance := s.s.ManagedProfile(binding.BindingID)
		oldBinding, exists := old[binding.BindingID]
		config := proto.Clone(binding.Config).(*core.Config)
		if instance != nil && instance.RequiresUserQuota() {
			if err := core.RequireUserQuotaConfig(config); err != nil {
				return nil, status.Error(codes.InvalidArgument, "Invalid quota requirement")
			}
		}
		if exists && instance != nil && oldBinding.ProfileID == binding.ProfileID && oldBinding.InboundID == binding.InboundID && proto.Equal(instance.Configuration(), config) {
			live[binding.BindingID] = instance
			continue
		}
		staged, err := core.NewStagedManagedProfile(s.s, config)
		if err != nil {
			return nil, status.Error(codes.FailedPrecondition, "The profile graph could not be prepared")
		}
		prepared = append(prepared, staged)
		if err := staged.Start(); err != nil {
			return nil, status.Error(codes.FailedPrecondition, "The profile listeners could not be prepared without interrupting the active plan")
		}
		live[binding.BindingID] = staged
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	revocations := profileTopologyCredentialRevocations(ctx, s.s, oldBindings, live)
	markRevocations := func() {
		for _, revoked := range revocations {
			dispatcher.MarkRetiredProfileUserDenied(revoked.instance, revoked.email)
		}
	}
	if err := s.s.CommitManagedProfileTopology(target, live, markRevocations); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	committed = true
	for _, revoked := range revocations {
		dispatcher.RevokeRetiredProfileUserLinks(revoked.instance, revoked.email)
	}
	response, err := s.profileStatusLocked()
	if err == nil {
		response.Fields["unchanged"] = structpb.NewBoolValue(false)
	}
	return response, err
}
