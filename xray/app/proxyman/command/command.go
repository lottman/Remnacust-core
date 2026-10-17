package command

import (
	"context"
	"sync"

	"github.com/xtls/xray-core/app/commander"
	"github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/proxy"
	grpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// InboundOperation is the interface for operations that applies to inbound handlers.
type InboundOperation interface {
	// ApplyInbound applies this operation to the given inbound handler.
	ApplyInbound(context.Context, inbound.Handler) error
}

// OutboundOperation is the interface for operations that applies to outbound handlers.
type OutboundOperation interface {
	// ApplyOutbound applies this operation to the given outbound handler.
	ApplyOutbound(context.Context, outbound.Handler) error
}

func getInbound(handler inbound.Handler) (proxy.Inbound, error) {
	gi, ok := handler.(proxy.GetInbound)
	if !ok {
		return nil, errors.New("can't get inbound proxy from handler.")
	}
	return gi.GetInbound(), nil
}

// ApplyInbound implements InboundOperation.
func (op *AddUserOperation) ApplyInbound(ctx context.Context, handler inbound.Handler) error {
	p, err := getInbound(handler)
	if err != nil {
		return err
	}
	um, ok := p.(proxy.UserManager)
	if !ok {
		return errors.New("proxy is not a UserManager")
	}
	mUser, err := op.User.ToMemoryUser()
	if err != nil {
		return errors.New("failed to parse user").Base(err)
	}
	if err := um.AddUser(ctx, mUser); err != nil {
		return err
	}
	dispatcher.AllowUserLinks(mUser.Email)
	return nil
}

// ApplyInbound implements InboundOperation.
func (op *RemoveUserOperation) ApplyInbound(ctx context.Context, handler inbound.Handler) error {
	p, err := getInbound(handler)
	if err != nil {
		return err
	}
	um, ok := p.(proxy.UserManager)
	if !ok {
		return errors.New("proxy is not a UserManager")
	}
	if um.GetUser(ctx, op.Email) != nil {
		if err := um.RemoveUser(ctx, op.Email); err != nil {
			return err
		}
	}
	dispatcher.RevokeUserLinks(op.Email)
	return nil
}

type handlerServer struct {
	operation *sync.Mutex
	runtime   runtimeState
	s         *core.Instance
	ihm       inbound.Manager
	ohm       outbound.Manager
}

func (s *handlerServer) AddInbound(ctx context.Context, request *AddInboundRequest) (*AddInboundResponse, error) {
	if controller := s.s.RuntimeController(); controller != nil {
		return &AddInboundResponse{}, controller.Update(ctx, func(config *core.Config) error {
			if request.Inbound == nil {
				return status.Error(codes.InvalidArgument, "Inbound configuration is required")
			}
			for _, existing := range config.Inbound {
				if existing.Tag == request.Inbound.Tag {
					return status.Error(codes.AlreadyExists, "Inbound tag already exists")
				}
			}
			config.Inbound = append(config.Inbound, proto.Clone(request.Inbound).(*core.InboundHandlerConfig))
			return nil
		})
	}
	s.operation.Lock()
	defer s.operation.Unlock()
	if err := core.AddInboundHandler(s.s, request.Inbound); err != nil {
		return nil, err
	}

	return &AddInboundResponse{}, nil
}

func (s *handlerServer) RemoveInbound(ctx context.Context, request *RemoveInboundRequest) (*RemoveInboundResponse, error) {
	if controller := s.s.RuntimeController(); controller != nil {
		return &RemoveInboundResponse{}, s.removeRuntimeInbound(ctx, request.Tag)
	}
	s.operation.Lock()
	defer s.operation.Unlock()
	return &RemoveInboundResponse{}, s.ihm.RemoveHandler(ctx, request.Tag)
}

func (s *handlerServer) AlterInbound(ctx context.Context, request *AlterInboundRequest) (*AlterInboundResponse, error) {
	s.operation.Lock()
	defer s.operation.Unlock()
	if handled, err := s.removeRetiredUserLocked(ctx, request); handled {
		return &AlterInboundResponse{}, err
	}
	target, err := s.inboundUserTargetLocked(ctx, request.Tag)
	if err != nil {
		return nil, err
	}
	rawOperation, err := request.Operation.GetInstance()
	if err != nil {
		return nil, errors.New("unknown operation").Base(err)
	}
	operation, ok := rawOperation.(InboundOperation)
	if !ok {
		return nil, errors.New("not an inbound operation")
	}

	handler, err := target.ihm.GetHandler(ctx, request.Tag)
	if err != nil {
		return nil, errors.New("failed to get handler: ", request.Tag).Base(err)
	}

	return &AlterInboundResponse{}, target.applyInboundPersisted(ctx, handler, operation)
}

func (s *handlerServer) ListInbounds(ctx context.Context, request *ListInboundsRequest) (*ListInboundsResponse, error) {
	handlers := s.ihm.ListHandlers(ctx)
	response := &ListInboundsResponse{}
	if request.GetIsOnlyTags() {
		for _, handler := range handlers {
			response.Inbounds = append(response.Inbounds, &core.InboundHandlerConfig{
				Tag: handler.Tag(),
			})
		}
	} else {
		for _, handler := range handlers {
			response.Inbounds = append(response.Inbounds, &core.InboundHandlerConfig{
				Tag:              handler.Tag(),
				ReceiverSettings: handler.ReceiverSettings(),
				ProxySettings:    handler.ProxySettings(),
			})
		}
	}

	return response, nil
}

func (s *handlerServer) GetInboundUsers(ctx context.Context, request *GetInboundUserRequest) (*GetInboundUserResponse, error) {
	s.operation.Lock()
	defer s.operation.Unlock()
	target, err := s.inboundUserTargetLocked(ctx, request.Tag)
	if err != nil {
		return nil, err
	}
	handler, err := target.ihm.GetHandler(ctx, request.Tag)
	if err != nil {
		return nil, errors.New("failed to get handler: ", request.Tag).Base(err)
	}
	p, err := getInbound(handler)
	if err != nil {
		return nil, err
	}
	um, ok := p.(proxy.UserManager)
	if !ok {
		return nil, errors.New("proxy is not a UserManager")
	}
	if len(request.Email) > 0 {
		return &GetInboundUserResponse{Users: []*protocol.User{protocol.ToProtoUser(um.GetUser(ctx, request.Email))}}, nil
	}
	result := make([]*protocol.User, 0, 100)
	users := um.GetUsers(ctx)
	for _, u := range users {
		result = append(result, protocol.ToProtoUser(u))
	}
	return &GetInboundUserResponse{Users: result}, nil
}

func (s *handlerServer) GetInboundUsersCount(ctx context.Context, request *GetInboundUserRequest) (*GetInboundUsersCountResponse, error) {
	s.operation.Lock()
	defer s.operation.Unlock()
	target, err := s.inboundUserTargetLocked(ctx, request.Tag)
	if err != nil {
		return nil, err
	}
	handler, err := target.ihm.GetHandler(ctx, request.Tag)
	if err != nil {
		return nil, errors.New("failed to get handler: ", request.Tag).Base(err)
	}
	p, err := getInbound(handler)
	if err != nil {
		return nil, err
	}
	um, ok := p.(proxy.UserManager)
	if !ok {
		return nil, errors.New("proxy is not a UserManager")
	}
	return &GetInboundUsersCountResponse{Count: um.GetUsersCount(ctx)}, nil
}

func (s *handlerServer) AddOutbound(ctx context.Context, request *AddOutboundRequest) (*AddOutboundResponse, error) {
	if controller := s.s.RuntimeController(); controller != nil {
		return &AddOutboundResponse{}, controller.Update(ctx, func(config *core.Config) error {
			if request.Outbound == nil {
				return status.Error(codes.InvalidArgument, "Outbound configuration is required")
			}
			for _, existing := range config.Outbound {
				if existing.Tag == request.Outbound.Tag {
					return status.Error(codes.AlreadyExists, "Outbound tag already exists")
				}
			}
			config.Outbound = append(config.Outbound, proto.Clone(request.Outbound).(*core.OutboundHandlerConfig))
			return nil
		})
	}
	if err := core.AddOutboundHandler(s.s, request.Outbound); err != nil {
		return nil, err
	}
	return &AddOutboundResponse{}, nil
}

func (s *handlerServer) RemoveOutbound(ctx context.Context, request *RemoveOutboundRequest) (*RemoveOutboundResponse, error) {
	if controller := s.s.RuntimeController(); controller != nil {
		return &RemoveOutboundResponse{}, controller.Update(ctx, func(config *core.Config) error {
			for i, existing := range config.Outbound {
				if existing.Tag == request.Tag {
					config.Outbound = append(config.Outbound[:i], config.Outbound[i+1:]...)
					return nil
				}
			}
			return status.Error(codes.NotFound, "Outbound tag not found")
		})
	}
	return &RemoveOutboundResponse{}, s.ohm.RemoveHandler(ctx, request.Tag)
}

func (s *handlerServer) AlterOutbound(ctx context.Context, request *AlterOutboundRequest) (*AlterOutboundResponse, error) {
	if s.s.RuntimeController() != nil {
		return nil, status.Error(codes.Unimplemented, "Replace outbound settings through a configuration reconciliation")
	}
	rawOperation, err := request.Operation.GetInstance()
	if err != nil {
		return nil, errors.New("unknown operation").Base(err)
	}
	operation, ok := rawOperation.(OutboundOperation)
	if !ok {
		return nil, errors.New("not an outbound operation")
	}

	handler := s.ohm.GetHandler(request.Tag)
	return &AlterOutboundResponse{}, operation.ApplyOutbound(ctx, handler)
}

func (s *handlerServer) ListOutbounds(ctx context.Context, request *ListOutboundsRequest) (*ListOutboundsResponse, error) {
	if controller := s.s.RuntimeController(); controller != nil {
		response := &ListOutboundsResponse{}
		err := controller.View(ctx, outbound.ManagerType(), func(feature features.Feature) error {
			for _, handler := range feature.(outbound.Manager).ListHandlers(ctx) {
				if _, api := handler.(*commander.Outbound); api {
					continue
				}
				if handler.ProxySettings() == nil {
					continue
				}
				response.Outbounds = append(response.Outbounds, &core.OutboundHandlerConfig{Tag: handler.Tag(), SenderSettings: handler.SenderSettings(), ProxySettings: handler.ProxySettings()})
			}
			return nil
		})
		return response, err
	}
	handlers := s.ohm.ListHandlers(ctx)
	response := &ListOutboundsResponse{}
	for _, handler := range handlers {
		// Ignore gRPC outbound
		if _, ok := handler.(*commander.Outbound); ok {
			continue
		}
		response.Outbounds = append(response.Outbounds, &core.OutboundHandlerConfig{
			Tag:            handler.Tag(),
			SenderSettings: handler.SenderSettings(),
			ProxySettings:  handler.ProxySettings(),
		})
	}
	return response, nil
}

func (s *handlerServer) mustEmbedUnimplementedHandlerServiceServer() {}

type service struct {
	v *core.Instance
}

func (s *service) Register(server *grpc.Server) {
	hs := &handlerServer{
		s: s.v, operation: s.v.ManagementAccess(),
	}
	common.Must(s.v.RequireFeatures(func(im inbound.Manager, om outbound.Manager) {
		hs.ihm = im
		hs.ohm = om
	}, false))
	RegisterHandlerServiceServer(server, hs)
	registerRuntimeControl(server, hs)

	// For compatibility purposes
	vCoreDesc := HandlerService_ServiceDesc
	vCoreDesc.ServiceName = "v2ray.core.app.proxyman.command.HandlerService"
	server.RegisterService(&vCoreDesc, hs)
}

func init() {
	common.Must(common.RegisterConfig((*Config)(nil), func(ctx context.Context, cfg interface{}) (interface{}, error) {
		s := core.MustFromContext(ctx)
		return &service{v: s}, nil
	}))
}
