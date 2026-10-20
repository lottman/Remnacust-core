//go:build !wasm

package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/xtls/xray-core/app/commander"
	"github.com/xtls/xray-core/app/dispatcher"
	appdns "github.com/xtls/xray-core/app/dns"
	applog "github.com/xtls/xray-core/app/log"
	"github.com/xtls/xray-core/app/observatory"
	burstobservatory "github.com/xtls/xray-core/app/observatory/burst"
	apppolicy "github.com/xtls/xray-core/app/policy"
	"github.com/xtls/xray-core/app/proxyman"
	managedinbound "github.com/xtls/xray-core/app/proxyman/inbound"
	managedoutbound "github.com/xtls/xray-core/app/proxyman/outbound"
	"github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features"
	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/dns/localdns"
	"github.com/xtls/xray-core/features/extension"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/policy"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/proxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

const RuntimeServiceName = "remnacust-live-ports-v1"
const RuntimeApplyMethod = "/" + RuntimeServiceName + "/Apply"
const RuntimeInspectMethod = "/" + RuntimeServiceName + "/Inspect"
const RuntimeRequireUserQuotaMethod = "/" + RuntimeServiceName + "/RequireUserQuota"
const maxRuntimeGenerations = 16

type runtimeControl interface {
	applyRuntime(context.Context, *core.Config) (*structpb.Struct, error)
	inspectRuntime(context.Context, *structpb.Struct) (*structpb.Struct, error)
	requireUserQuota(context.Context, *structpb.Struct) (*structpb.Struct, error)
}

type runtimeState struct {
	hold       func()
	stopped    bool
	generation *managedinbound.RuntimeGeneration
	owned      atomic.Int64
	sequence   uint64
}

func runtimeApp(app *serial.TypedMessage) bool {
	return app.Type == serial.GetMessageType(&router.Config{}) || app.Type == serial.GetMessageType(&observatory.Config{}) || app.Type == serial.GetMessageType(&burstobservatory.Config{})
}

func runtimeContextApp(app *serial.TypedMessage) bool {
	return runtimeApp(app) || app.Type == serial.GetMessageType(&appdns.Config{}) || app.Type == serial.GetMessageType(&applog.Config{}) || app.Type == serial.GetMessageType(&apppolicy.Config{})
}

func initializeRuntimeControl(handler *handlerServer) error {
	controller, _ := handler.s.InstallRuntimeController(handler, func() {
		handler.operation.Lock()
		defer handler.operation.Unlock()
		handler.runtime.stopped = true
		barrier := handler.s.RuntimeAccess()
		barrier.Lock()
		if registry, ok := handler.ihm.(*managedinbound.Manager); ok {
			registry.QuiesceRuntimeLocked()
		}
		barrier.Unlock()
		if handler.runtime.hold != nil {
			handler.runtime.hold()
			handler.runtime.hold = nil
		}
	})
	if controller != handler {
		return status.Error(codes.Unavailable, "The profile controller is unavailable")
	}
	return nil
}

// This service inherits HandlerService's private control transport. No public
// HTTP port, client extension or remotely supplied code is introduced.
func registerRuntimeControl(server *grpc.Server, handler *handlerServer) {
	common.Must(initializeRuntimeControl(handler))
	registerProfileControl(server, handler)
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: RuntimeServiceName,
		HandlerType: (*runtimeControl)(nil),
		Methods: []grpc.MethodDesc{
			{MethodName: "RequireUserQuota", Handler: func(s interface{}, ctx context.Context, decode func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
				request := new(structpb.Struct)
				if err := decode(request); err != nil {
					return nil, err
				}
				call := func(ctx context.Context, request interface{}) (interface{}, error) {
					return s.(runtimeControl).requireUserQuota(ctx, request.(*structpb.Struct))
				}
				if interceptor == nil {
					return call(ctx, request)
				}
				return interceptor(ctx, request, &grpc.UnaryServerInfo{Server: s, FullMethod: RuntimeRequireUserQuotaMethod}, call)
			}},
			{MethodName: "Apply", Handler: func(s interface{}, ctx context.Context, decode func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
				request := new(core.Config)
				if err := decode(request); err != nil {
					return nil, err
				}
				call := func(ctx context.Context, request interface{}) (interface{}, error) {
					return s.(runtimeControl).applyRuntime(ctx, request.(*core.Config))
				}
				if interceptor == nil {
					return call(ctx, request)
				}
				return interceptor(ctx, request, &grpc.UnaryServerInfo{Server: s, FullMethod: RuntimeApplyMethod}, call)
			}},
			{MethodName: "Inspect", Handler: func(s interface{}, ctx context.Context, decode func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
				request := new(structpb.Struct)
				if err := decode(request); err != nil {
					return nil, err
				}
				call := func(ctx context.Context, request interface{}) (interface{}, error) {
					return s.(runtimeControl).inspectRuntime(ctx, request.(*structpb.Struct))
				}
				if interceptor == nil {
					return call(ctx, request)
				}
				return interceptor(ctx, request, &grpc.UnaryServerInfo{Server: s, FullMethod: RuntimeInspectMethod}, call)
			}},
		},
	}, handler)
}

func runtimeHash(config *core.Config) string {
	data, _ := proto.MarshalOptions{Deterministic: true}.Marshal(config)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func (s *handlerServer) inspectRuntime(ctx context.Context, request *structpb.Struct) (*structpb.Struct, error) {
	s.operation.Lock()
	defer s.operation.Unlock()
	return structpb.NewStruct(map[string]interface{}{"hash": runtimeHash(s.runtimeConfiguration(ctx)), "generation": fmt.Sprint(s.runtime.sequence), "capability": RuntimeServiceName, "userQuotaRequired": s.s.RequiresUserQuota()})
}

func staticRuntimeConfig(config *core.Config) *core.Config {
	copy := proto.Clone(config).(*core.Config)
	copy.Inbound, copy.Outbound = nil, nil
	apps := copy.App[:0]
	for _, app := range copy.App {
		if app.Type == serial.GetMessageType(&apppolicy.Config{}) {
			settings, err := app.GetInstance()
			if err != nil {
				apps = append(apps, app)
			} else if system := settings.(*apppolicy.Config).System; system != nil {
				// Native listener byte counters are installed at construction.
				// Level policy is generation-local; system counters stay fixed.
				apps = append(apps, serial.ToTypedMessage(&apppolicy.Config{System: system}))
			}
			continue
		}
		if runtimeContextApp(app) || app.Type == core.UserQuotaRequirementType {
			continue
		}
		apps = append(apps, app)
	}
	copy.App = apps
	sort.Slice(copy.App, func(i, j int) bool { return copy.App[i].Type < copy.App[j].Type })
	return copy
}

func routingConfig(config *core.Config) *serial.TypedMessage {
	for _, app := range config.App {
		if app.Type == serial.GetMessageType(&router.Config{}) {
			return app
		}
	}
	return serial.ToTypedMessage(&router.Config{})
}

func sameGeneration(old, next *core.Config) bool {
	if !proto.Equal(routingConfig(old), routingConfig(next)) || len(old.Outbound) != len(next.Outbound) {
		return false
	}
	for i := range old.Outbound {
		if !proto.Equal(old.Outbound[i], next.Outbound[i]) {
			return false
		}
	}
	oldContext, nextContext := &core.Config{}, &core.Config{}
	for _, app := range old.App {
		if runtimeContextApp(app) {
			oldContext.App = append(oldContext.App, app)
		}
	}
	for _, app := range next.App {
		if runtimeContextApp(app) {
			nextContext.App = append(nextContext.App, app)
		}
	}
	if !proto.Equal(oldContext, nextContext) {
		return false
	}
	return true
}

type borrowedOutbound struct{ outbound.Handler }

func (borrowedOutbound) Start() error { return nil }
func (borrowedOutbound) Close() error { return nil }

func (s *handlerServer) prepareGeneration(config *core.Config) (*managedinbound.RuntimeGeneration, func(), error) {
	if s.runtime.owned.Load() >= maxRuntimeGenerations {
		return nil, nil, status.Error(codes.ResourceExhausted, "Earlier profile generations still have active sessions; wait for them to drain")
	}
	scope := core.NewIsolatedFeatureScope(s.s, outbound.ManagerType(), routing.RouterType(), routing.DispatcherType(), extension.ObservatoryType(), dns.ClientType(), policy.ManagerType(), (*applog.Instance)(nil))
	cleanup := func() { _ = scope.Close() }
	for _, app := range config.App {
		if app.Type != serial.GetMessageType(&appdns.Config{}) && app.Type != serial.GetMessageType(&applog.Config{}) && app.Type != serial.GetMessageType(&apppolicy.Config{}) {
			continue
		}
		settings, err := app.GetInstance()
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		feature, err := common.CreateObject(scope.Context(), settings)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		if err := scope.Add(feature.(features.Feature)); err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	if scope.Instance.GetFeature(dns.ClientType()) == nil {
		if err := scope.Add(localdns.New()); err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	if scope.Instance.GetFeature(policy.ManagerType()) == nil {
		if err := scope.Add(policy.DefaultManager{}); err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	manager, err := managedoutbound.New(scope.Context(), &proxyman.OutboundConfig{})
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	if err = scope.Add(manager); err != nil {
		cleanup()
		return nil, nil, err
	}
	rawDispatcher, err := common.CreateObject(scope.Context(), &dispatcher.Config{})
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	if err = scope.Add(rawDispatcher.(features.Feature)); err != nil {
		cleanup()
		return nil, nil, err
	}
	settings, err := routingConfig(config).GetInstance()
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	rawRouter, err := common.CreateObject(scope.Context(), settings)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	if err = scope.Add(rawRouter.(features.Feature)); err != nil {
		cleanup()
		return nil, nil, err
	}
	for _, outboundConfig := range config.Outbound {
		if err = core.AddOutboundHandler(scope.Instance, outboundConfig); err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	for _, app := range config.App {
		if !runtimeApp(app) || app.Type == serial.GetMessageType(&router.Config{}) {
			continue
		}
		setting, err := app.GetInstance()
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		feature, err := common.CreateObject(scope.Context(), setting)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		if err := scope.Add(feature.(features.Feature)); err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	// Commander owns its API outbound; generations borrow it without closing it.
	for _, app := range config.App {
		if app.Type != serial.GetMessageType(&commander.Config{}) {
			continue
		}
		setting, err := app.GetInstance()
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		tag := setting.(*commander.Config).Tag
		if manager.GetHandler(tag) != nil {
			cleanup()
			return nil, nil, status.Error(codes.InvalidArgument, "The API outbound tag is reserved")
		}
		if handler := s.ohm.GetHandler(tag); handler != nil {
			if err := manager.AddHandler(scope.Context(), borrowedOutbound{handler}); err != nil {
				cleanup()
				return nil, nil, err
			}
		}
	}
	if err := scope.Start(); err != nil {
		cleanup()
		return nil, nil, err
	}
	s.runtime.owned.Add(1)
	var closed atomic.Bool
	close := func() {
		if closed.CompareAndSwap(false, true) {
			cleanup()
			s.runtime.owned.Add(-1)
		}
	}
	return managedinbound.NewRuntimeGeneration(scope.Context(), scope.Instance.GetFeature(routing.DispatcherType()).(routing.Dispatcher), close), close, nil
}

type runtimeUsers struct {
	tag     string
	manager proxy.UserManager
	before  []*protocol.MemoryUser
	after   []*protocol.MemoryUser
}

func proxyWithoutUsers(config *serial.TypedMessage) (proto.Message, error) {
	settings, err := config.GetInstance()
	if err != nil {
		return nil, err
	}
	settings = proto.Clone(settings)
	field := settings.ProtoReflect().Descriptor().Fields().ByName("users")
	if field != nil {
		settings.ProtoReflect().Clear(field)
	}
	return settings, nil
}

func replaceRuntimeUsers(ctx context.Context, change runtimeUsers, users []*protocol.MemoryUser) error {
	target := make(map[string]*protocol.MemoryUser, len(users))
	for _, user := range users {
		target[user.Email] = user
	}
	for _, user := range change.manager.GetUsers(ctx) {
		if next := target[user.Email]; next != nil && proto.Equal(protocol.ToProtoUser(user), protocol.ToProtoUser(next)) {
			delete(target, user.Email)
			continue
		}
		if err := change.manager.RemoveUser(ctx, user.Email); err != nil {
			return err
		}
	}
	for _, user := range target {
		if err := change.manager.AddUser(ctx, user); err != nil {
			return err
		}
		dispatcher.AllowUserLinks(user.Email)
	}
	return nil
}

func (s *handlerServer) applyRuntime(ctx context.Context, target *core.Config) (*structpb.Struct, error) {
	s.operation.Lock()
	defer s.operation.Unlock()
	return s.applyRuntimeLocked(ctx, target)
}

func (s *handlerServer) applyRuntimeLocked(ctx context.Context, target *core.Config) (*structpb.Struct, error) {
	prepared, err := s.prepareRuntimeLocked(ctx, target)
	if err != nil {
		return nil, err
	}
	defer prepared.abort()
	if prepared.unchanged {
		return prepared.result()
	}
	barrier := s.s.RuntimeAccess()
	barrier.Lock()
	if err := prepared.installUsers(ctx); err != nil {
		cleanup, rollback := prepared.rollbackUsers()
		barrier.Unlock()
		for _, close := range cleanup {
			close()
		}
		if rollback != nil {
			return nil, status.Error(codes.Internal, "Credential rollback failed; affected ports are closed to new sessions and require administrator recovery")
		}
		return nil, err
	}
	if err := s.s.SaveRuntimeConfiguration(prepared.target); err != nil {
		cleanup, rollback := prepared.rollbackUsers()
		if failure := s.s.SaveRuntimeConfiguration(prepared.current); failure != nil {
			rollback = failure
			cleanup = append(cleanup, s.quarantineRuntimeUsersLocked(prepared.users))
		}
		barrier.Unlock()
		for _, close := range cleanup {
			close()
		}
		if rollback != nil {
			return nil, status.Error(codes.Internal, "Runtime checkpoint and user rollback need administrator recovery")
		}
		return nil, status.Error(codes.Internal, "Could not persist the runtime configuration; previous configuration retained")
	}
	cleanup := prepared.commitLocked(ctx)
	barrier.Unlock()
	prepared.finish(cleanup)
	return prepared.result()
}

func (s *handlerServer) prepareRuntimeLocked(ctx context.Context, target *core.Config) (*runtimePreparation, error) {
	if !s.s.RuntimeManagementReady() {
		return nil, status.Error(codes.Unavailable, "The owning profile plan is not ready for management")
	}
	if s.runtime.stopped {
		return nil, status.Error(codes.Unavailable, "The core is shutting down")
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	current := s.runtimeConfiguration(ctx)
	if current == nil || target == nil {
		return nil, status.Error(codes.FailedPrecondition, "Runtime configuration is unavailable")
	}
	target = proto.Clone(target).(*core.Config)
	quotaRequired, requirementErr := core.ConfigRequiresUserQuota(target)
	if requirementErr != nil {
		return nil, status.Error(codes.InvalidArgument, "Invalid server quota requirement metadata")
	}
	if s.s.RequiresUserQuota() && !quotaRequired {
		if err := core.RequireUserQuotaConfig(target); err != nil {
			return nil, status.Error(codes.InvalidArgument, "Invalid server quota requirements")
		}
		quotaRequired = true
	}
	if quotaRequired && (!s.s.RuntimeCheckpointEnabled() || !dispatcher.UserQuotaPolicyReady()) {
		return nil, status.Error(codes.FailedPrecondition, "A durable native checkpoint and applied user quota policy are required")
	}
	if len(target.App) > 64 || len(target.Outbound) == 0 {
		return nil, status.Error(codes.InvalidArgument, "A live configuration needs outbounds and a bounded feature set")
	}
	seenApps := make(map[string]bool, len(target.App))
	for _, app := range target.App {
		if app == nil || app.Type == "" {
			return nil, status.Error(codes.InvalidArgument, "Invalid runtime feature settings")
		}
		if seenApps[app.Type] {
			return nil, status.Error(codes.InvalidArgument, "Duplicate runtime feature settings")
		}
		seenApps[app.Type] = true
	}
	ports := uint64(0)
	for _, config := range target.Inbound {
		if config == nil || config.ReceiverSettings == nil || config.ProxySettings == nil {
			return nil, status.Error(codes.InvalidArgument, "Invalid inbound settings")
		}
		settings, err := config.ReceiverSettings.GetInstance()
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "Invalid native receiver settings")
		}
		receiver, ok := settings.(*proxyman.ReceiverConfig)
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "A live inbound needs native receiver settings")
		}
		if receiver.PortList != nil {
			for _, interval := range receiver.PortList.Range {
				if interval == nil || interval.From == 0 || interval.To > 65535 || interval.To < interval.From {
					return nil, status.Error(codes.InvalidArgument, "Invalid live port range")
				}
				ports += uint64(interval.To-interval.From) + 1
				if ports > 512 {
					return nil, status.Error(codes.ResourceExhausted, "Live configuration exceeds 512 native ports")
				}
			}
		}
	}
	for _, config := range target.Outbound {
		if config == nil || config.ProxySettings == nil {
			return nil, status.Error(codes.InvalidArgument, "Invalid outbound settings")
		}
	}
	expected := metadata.ValueFromIncomingContext(ctx, "if-match")
	if len(expected) != 1 || expected[0] != runtimeHash(current) {
		return nil, status.Error(codes.Aborted, "The core configuration changed; request its current revision and retry")
	}
	if !proto.Equal(staticRuntimeConfig(current), staticRuntimeConfig(target)) {
		return nil, status.Error(codes.FailedPrecondition, "Global core settings changed; preserve current sessions and apply this change during an explicit maintenance restart")
	}
	if len(target.Inbound) > 512 || len(target.Outbound) > 4096 {
		return nil, status.Error(codes.ResourceExhausted, "Runtime configuration exceeds the port or outbound limit")
	}
	registry, ok := s.ihm.(*managedinbound.Manager)
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "The inbound manager does not support live ports")
	}
	if proto.Equal(current, target) {
		return &runtimePreparation{server: s, current: current, target: target, unchanged: true}, nil
	}
	old := make(map[string]*core.InboundHandlerConfig)
	for _, config := range current.Inbound {
		old[config.Tag] = config
	}
	seen := make(map[string]bool)
	var staged []*managedinbound.AlwaysOnInboundHandler
	var retained []*managedinbound.AlwaysOnInboundHandler
	var users []runtimeUsers
	committed := false
	var abortGeneration func()
	defer func() {
		if !committed {
			for _, handler := range staged {
				_ = handler.Close()
			}
			if abortGeneration != nil {
				abortGeneration()
			}
		}
	}()
	// Parse and instantiate every changed native proxy before touching a listener
	// or a live user map. Wire-setting changes require a new port/tag.
	for _, config := range target.Inbound {
		if config == nil || config.Tag == "" || seen[config.Tag] {
			return nil, status.Error(codes.InvalidArgument, "Each live inbound requires a unique non-empty tag")
		}
		seen[config.Tag] = true
		previous := old[config.Tag]
		if previous != nil {
			handler, err := s.ihm.GetHandler(ctx, config.Tag)
			if err != nil {
				return nil, err
			}
			always, ok := handler.(*managedinbound.AlwaysOnInboundHandler)
			if !ok {
				return nil, status.Error(codes.FailedPrecondition, "A service inbound cannot be changed through live ports")
			}
			sameReceiver, err := sameRuntimeReceiver(previous.ReceiverSettings, config.ReceiverSettings)
			if err != nil {
				return nil, status.Error(codes.InvalidArgument, "Invalid live receiver settings")
			}
			if !sameReceiver {
				return nil, status.Errorf(codes.FailedPrecondition, "Inbound %s changes its port or transport; create a separate port/tag and retain this listener", config.Tag)
			}
			before, err := proxyWithoutUsers(previous.ProxySettings)
			if err != nil {
				return nil, err
			}
			after, err := proxyWithoutUsers(config.ProxySettings)
			if err != nil {
				return nil, err
			}
			if !proto.Equal(before, after) {
				return nil, status.Errorf(codes.FailedPrecondition, "Inbound %s changes its native authentication settings; stage it on a separate port/tag", config.Tag)
			}
			retained = append(retained, always)
			if !proto.Equal(previous.ProxySettings, config.ProxySettings) {
				liveManager, liveOK := always.GetInbound().(proxy.UserManager)
				if !liveOK {
					return nil, status.Error(codes.FailedPrecondition, "This inbound cannot replace users without a maintenance restart")
				}
				nextUsers, err := runtimeUserList(config.ProxySettings)
				if err != nil {
					return nil, err
				}
				if _, err := runtimeUserList(previous.ProxySettings); err != nil {
					return nil, err
				}
				users = append(users, runtimeUsers{config.Tag, liveManager, liveManager.GetUsers(ctx), nextUsers})
			}
		} else {
			handler, err := managedinbound.NewHandler(s.s.RuntimeContext(), config)
			if err != nil {
				return nil, err
			}
			always, ok := handler.(*managedinbound.AlwaysOnInboundHandler)
			if !ok {
				_ = handler.Close()
				return nil, status.Error(codes.FailedPrecondition, "A live port requires a native inbound handler")
			}
			staged = append(staged, always)
		}
	}
	var removed []string
	for tag := range old {
		if !seen[tag] {
			removed = append(removed, tag)
		}
	}
	for _, app := range current.App {
		if app.Type != serial.GetMessageType(&commander.Config{}) {
			continue
		}
		setting, _ := app.GetInstance()
		apiTag := setting.(*commander.Config).Tag
		if old[apiTag] != nil && !seen[apiTag] {
			return nil, status.Error(codes.FailedPrecondition, "The private API inbound cannot be removed")
		}
	}
	generation := s.runtime.generation
	if generation == nil {
		generation = managedinbound.NewRuntimeGeneration(s.s.RuntimeContext(), s.s.GetFeature(routing.DispatcherType()).(routing.Dispatcher), nil)
	}
	if !sameGeneration(current, target) {
		var err error
		generation, abortGeneration, err = s.prepareGeneration(target)
		if err != nil {
			return nil, err
		}
	}
	for _, handler := range staged {
		if err := registry.Stage(handler); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "Could not bind inbound %s: %v", handler.Tag(), err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	var hold func()
	if generation != s.runtime.generation {
		hold = generation.Retain()
	}
	// Ownership of staged listeners and the generation passes to the transaction.
	committed = true
	return &runtimePreparation{server: s, current: current, target: target,
		registry: registry, staged: staged, retained: retained, removed: removed,
		users: users, generation: generation, abortGeneration: abortGeneration, hold: hold}, nil
}

// Only affected listeners stop admitting when a credential rollback fails.
// Healthy ports keep serving their established sessions during recovery.
func (s *handlerServer) quarantineRuntimeUsersLocked(changes []runtimeUsers) func() {
	s.runtime.stopped = true
	for _, change := range changes {
		if handler, err := s.ihm.GetHandler(context.Background(), change.tag); err == nil {
			if always, ok := handler.(*managedinbound.AlwaysOnInboundHandler); ok {
				always.RuntimeSlot().Prepare()
			}
		}
	}
	return func() {
		for _, change := range changes {
			for _, user := range append(append([]*protocol.MemoryUser(nil), change.before...), change.after...) {
				dispatcher.RevokeProfileInboundUserLinks(s.s, change.tag, user.Email)
			}
		}
	}
}
