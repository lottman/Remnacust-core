//go:build !wasm

package command

import (
	"context"
	"errors"

	"github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

const ProfileControlServiceName = "remnacust-profile-control-v1"
const ProfileControlInspectMethod = "/" + ProfileControlServiceName + "/Inspect"
const ProfileControlApplyMethod = "/" + ProfileControlServiceName + "/Apply"
const ProfileControlTrafficMethod = "/" + ProfileControlServiceName + "/Traffic"
const ProfileTrafficCapability = "remnacust-profile-traffic-v1"

var profileDeliveryReplay = errors.New("profile delivery already applied")

type profileControl interface {
	inspectProfiles(context.Context, *structpb.Struct) (*structpb.Struct, error)
	applyProfiles(context.Context, *core.Config) (*structpb.Struct, error)
	trafficProfiles(context.Context, *structpb.Struct) (*structpb.Struct, error)
}

func registerProfileControl(server *grpc.Server, handler *handlerServer) {
	server.RegisterService(&grpc.ServiceDesc{ServiceName: ProfileControlServiceName, HandlerType: (*profileControl)(nil), Methods: []grpc.MethodDesc{
		{MethodName: "Traffic", Handler: func(s interface{}, ctx context.Context, decode func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
			request := new(structpb.Struct)
			if err := decode(request); err != nil {
				return nil, err
			}
			call := func(ctx context.Context, request interface{}) (interface{}, error) {
				return s.(profileControl).trafficProfiles(ctx, request.(*structpb.Struct))
			}
			if interceptor == nil {
				return call(ctx, request)
			}
			return interceptor(ctx, request, &grpc.UnaryServerInfo{Server: s, FullMethod: ProfileControlTrafficMethod}, call)
		}},
		{MethodName: "Inspect", Handler: func(s interface{}, ctx context.Context, decode func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
			request := new(structpb.Struct)
			if err := decode(request); err != nil {
				return nil, err
			}
			call := func(ctx context.Context, request interface{}) (interface{}, error) {
				return s.(profileControl).inspectProfiles(ctx, request.(*structpb.Struct))
			}
			if interceptor == nil {
				return call(ctx, request)
			}
			return interceptor(ctx, request, &grpc.UnaryServerInfo{Server: s, FullMethod: ProfileControlInspectMethod}, call)
		}},
		{MethodName: "Apply", Handler: func(s interface{}, ctx context.Context, decode func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
			request := new(core.Config)
			if err := decode(request); err != nil {
				return nil, err
			}
			call := func(ctx context.Context, request interface{}) (interface{}, error) {
				return s.(profileControl).applyProfiles(ctx, request.(*core.Config))
			}
			if interceptor == nil {
				return call(ctx, request)
			}
			return interceptor(ctx, request, &grpc.UnaryServerInfo{Server: s, FullMethod: ProfileControlApplyMethod}, call)
		}},
	}}, handler)
}

func (s *handlerServer) trafficProfiles(ctx context.Context, request *structpb.Struct) (*structpb.Struct, error) {
	if len(request.GetFields()) != 0 {
		return nil, status.Error(codes.InvalidArgument, "Profile traffic inspection does not accept fields")
	}
	s.operation.Lock()
	defer s.operation.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if s.s.AccountingOwner() != s.s || !s.s.RuntimeManagementReady() {
		return nil, status.Error(codes.Unavailable, "The profile owner is not ready")
	}
	records, enrolled := s.s.BindingTrafficRecords()
	if !enrolled {
		return nil, status.Error(codes.FailedPrecondition, "No native profile plan is enrolled")
	}
	bindings := make([]interface{}, 0, len(records))
	for _, record := range records {
		bindings = append(bindings, map[string]interface{}{
			"bindingUuid": record.BindingID, "uplink": record.Uplink, "downlink": record.Downlink, "available": record.Available, "retired": record.Retired,
		})
	}
	return structpb.NewStruct(map[string]interface{}{"capability": ProfileTrafficCapability, "coreProcessId": core.ProcessID(), "bindings": bindings})
}

func profileRootControl(config *core.Config) *core.Config {
	config = proto.Clone(config).(*core.Config)
	apps := config.App[:0]
	for _, app := range config.App {
		if app.Type != core.ProfilePlanType && app.Type != core.ProfileDeliveryType && app.Type != core.UserQuotaRequirementType {
			apps = append(apps, app)
		}
	}
	config.App = apps
	return config
}

func (s *handlerServer) profileStatusLocked() (*structpb.Struct, error) {
	if s.s.AccountingOwner() != s.s || !s.s.RuntimeManagementReady() {
		return nil, status.Error(codes.FailedPrecondition, "The owning profile plan is not ready")
	}
	config := s.s.Configuration()
	bindings, enrolled, err := core.ConfigProfileBindings(config)
	if err != nil || !enrolled {
		return nil, status.Error(codes.FailedPrecondition, "No native profile plan is enrolled")
	}
	graphs := make([]interface{}, 0, len(bindings))
	for _, binding := range bindings {
		instance := s.s.ManagedProfile(binding.BindingID)
		if instance == nil || !instance.RuntimeManagementReady() {
			return nil, status.Error(codes.Unavailable, "An enrolled profile is not running")
		}
		up, down, available := s.s.BindingTrafficSnapshot(binding.BindingID)
		graphs = append(graphs, map[string]interface{}{"bindingUuid": binding.BindingID, "configProfileUuid": binding.ProfileID, "configProfileInboundUuid": binding.InboundID, "hash": runtimeHash(instance.Configuration()),
			"traffic": map[string]interface{}{"uplink": up, "downlink": down, "available": available}})
	}
	delivery, err := core.ConfigProfileDelivery(config)
	if err != nil {
		return nil, status.Error(codes.Internal, "The saved profile delivery needs recovery")
	}
	fields := map[string]interface{}{"capability": ProfileControlServiceName, "hash": runtimeHash(config), "coreProcessId": core.ProcessID(), "graphs": graphs, "dynamicMembership": false, "nodeDelivery": false}
	if delivery != nil {
		fields["delivery"] = delivery.Fields()
	}
	return structpb.NewStruct(fields)
}

func (s *handlerServer) inspectProfiles(ctx context.Context, request *structpb.Struct) (*structpb.Struct, error) {
	if len(request.GetFields()) != 0 {
		return nil, status.Error(codes.InvalidArgument, "Profile inspection does not accept fields")
	}
	s.operation.Lock()
	defer s.operation.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return s.profileStatusLocked()
}

func (s *handlerServer) applyProfiles(ctx context.Context, target *core.Config) (*structpb.Struct, error) {
	if s.s.AccountingOwner() != s.s || target == nil || proto.Size(target) > core.RuntimeJournalLimit {
		return nil, status.Error(codes.InvalidArgument, "Invalid owning profile plan request")
	}
	target = proto.Clone(target).(*core.Config)
	quotaRequired, quotaErr := core.ConfigRequiresUserQuota(target)
	if quotaErr != nil {
		return nil, status.Error(codes.InvalidArgument, "Invalid owning quota requirement metadata")
	}
	if quotaRequired && !dispatcher.UserQuotaPolicyReady() {
		return nil, status.Error(codes.FailedPrecondition, "Apply the node user quota policy before requiring it")
	}
	bindings, enrolled, err := core.ConfigProfileBindings(target)
	if err != nil || !enrolled || len(bindings) == 0 {
		return nil, status.Error(codes.InvalidArgument, "A nonempty compiled profile plan is required")
	}
	delivery, err := core.ConfigProfileDelivery(target)
	if err != nil || delivery == nil || !delivery.Enabled {
		return nil, status.Error(codes.InvalidArgument, "A versioned enabled profile delivery is required")
	}
	digest, err := core.ProfilePlanDigest(target)
	if err != nil || delivery.PlanHash != digest {
		return nil, status.Error(codes.InvalidArgument, "Profile source digest does not match its delivery")
	}
	expected := metadata.ValueFromIncomingContext(ctx, "if-match")
	if len(expected) != 1 || len(expected[0]) != 64 {
		return nil, status.Error(codes.Aborted, "Inspect the current owning plan before applying a delivery")
	}
	s.operation.Lock()
	previousRoster, _, rosterErr := core.ConfigProfileBindings(s.s.Configuration())
	s.operation.Unlock()
	if rosterErr != nil {
		return nil, status.Error(codes.Internal, "The current profile roster needs recovery")
	}
	if profileGraphReplacementRequired(previousRoster, bindings) {
		return s.applyProfileTopology(ctx, target, expected[0])
	}
	native := make([]RuntimeProfileTarget, 0, len(bindings))
	for _, binding := range bindings {
		instance := s.s.ManagedProfile(binding.BindingID)
		if instance == nil {
			return nil, status.Error(codes.FailedPrecondition, "Dynamic profile membership is not yet available")
		}
		if err := ensureProfileController(instance); err != nil {
			return nil, err
		}
		native = append(native, RuntimeProfileTarget{BindingID: binding.BindingID, Instance: instance, Config: binding.Config})
	}
	validate := func() error {
		if !s.s.RuntimeManagementReady() {
			return status.Error(codes.Unavailable, "The owning plan is not running")
		}
		current := s.s.Configuration()
		if runtimeHash(current) != expected[0] {
			return status.Error(codes.Aborted, "The owning profile plan changed; inspect it again")
		}
		if !proto.Equal(profileRootControl(current), profileRootControl(target)) {
			return status.Error(codes.FailedPrecondition, "A profile delivery cannot change the root control configuration")
		}
		oldBindings, _, err := core.ConfigProfileBindings(current)
		if err != nil || len(oldBindings) != len(bindings) {
			return status.Error(codes.FailedPrecondition, "Dynamic profile membership is not yet available")
		}
		for i, binding := range bindings {
			if binding.BindingID != oldBindings[i].BindingID || binding.ProfileID != oldBindings[i].ProfileID || binding.InboundID != oldBindings[i].InboundID {
				return status.Error(codes.FailedPrecondition, "Profile binding provenance changed")
			}
		}
		previous, err := core.ConfigProfileDelivery(current)
		if err != nil {
			return status.Error(codes.Internal, "The current profile delivery needs recovery")
		}
		if err := core.CheckProfileDeliveryAdvance(previous, delivery); err != nil {
			return status.Error(codes.Aborted, err.Error())
		}
		if previous != nil && *previous == *delivery {
			return profileDeliveryReplay
		}
		if s.s.RequiresUserQuota() {
			if err := core.RequireUserQuotaConfig(target); err != nil {
				return status.Error(codes.InvalidArgument, "Invalid mandatory quota metadata")
			}
		}
		return nil
	}
	_, err = applyRuntimeProfileBatch(ctx, s.s, native, func(configs map[string]*core.Config) error { return s.s.SaveManagedProfileDelivery(target, configs) }, validate, true)
	replay := errors.Is(err, profileDeliveryReplay)
	if err != nil && !replay {
		return nil, err
	}
	s.operation.Lock()
	defer s.operation.Unlock()
	currentDelivery, clockErr := core.ConfigProfileDelivery(s.s.Configuration())
	if clockErr != nil || currentDelivery == nil || *currentDelivery != *delivery {
		return nil, status.Error(codes.Aborted, "The profile delivery was superseded before acknowledgement")
	}
	response, err := s.profileStatusLocked()
	if err == nil {
		response.Fields["unchanged"] = structpb.NewBoolValue(replay)
	}
	return response, err
}
