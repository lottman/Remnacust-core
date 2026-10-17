//go:build !wasm

package command

import (
	"context"

	"github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/proxy"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *handlerServer) applyInboundPersisted(ctx context.Context, handler inbound.Handler, operation InboundOperation) error {
	if !s.s.RuntimeManagementReady() {
		return status.Error(codes.Unavailable, "The owning profile plan is not ready for management")
	}
	p, err := getInbound(handler)
	if err != nil {
		return err
	}
	manager, ok := p.(proxy.UserManager)
	if !ok {
		return operation.ApplyInbound(ctx, handler)
	}
	before := append([]*protocol.MemoryUser(nil), manager.GetUsers(ctx)...)
	previous := s.runtimeConfiguration(ctx)
	barrier := s.s.RuntimeAccess()
	barrier.Lock()
	// A profile-scoped removal revokes streams only after its checkpoint is
	// durable. The generic operation's node-wide revoke would interrupt sibling
	// graphs (and would be irreversible if persistence subsequently failed).
	removed := ""
	var operationErr error
	if remove, ok := operation.(*RemoveUserOperation); ok {
		if manager.GetUser(ctx, remove.Email) != nil {
			operationErr = manager.RemoveUser(ctx, remove.Email)
		}
		removed = remove.Email
	} else {
		operationErr = operation.ApplyInbound(ctx, handler)
	}
	if operationErr != nil {
		barrier.Unlock()
		return operationErr
	}
	config := s.runtimeConfiguration(ctx)
	if err := s.s.SaveRuntimeConfiguration(config); err != nil {
		change := runtimeUsers{tag: handler.Tag(), manager: manager, before: before, after: append([]*protocol.MemoryUser(nil), manager.GetUsers(ctx)...)}
		failure := replaceRuntimeUsers(context.Background(), change, before)
		journalFailure := s.s.SaveRuntimeConfiguration(previous)
		if failure != nil || journalFailure != nil {
			recovery := s.quarantineRuntimeUsersLocked([]runtimeUsers{change})
			barrier.Unlock()
			recovery()
			return status.Error(codes.Internal, "Credential checkpoint and rollback need administrator recovery")
		}
		barrier.Unlock()
		return status.Error(codes.Internal, "Credential change could not be persisted and was rolled back")
	}
	s.s.RecordConfiguration(config)
	barrier.Unlock()
	if removed != "" {
		dispatcher.RevokeProfileInboundUserLinks(s.s, handler.Tag(), removed)
	}
	return nil
}
