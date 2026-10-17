//go:build !wasm

package command

import (
	"context"
	"fmt"

	"github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

func (s *handlerServer) requireUserQuota(ctx context.Context, request *structpb.Struct) (*structpb.Struct, error) {
	fields := request.AsMap()
	if len(fields) != 1 || fields["coreProcessId"] != core.ProcessID() {
		return nil, status.Error(codes.FailedPrecondition, "Core incarnation changed before quota requirement commit")
	}
	s.operation.Lock()
	defer s.operation.Unlock()
	if s.runtime.stopped {
		return nil, status.Error(codes.Unavailable, "The core is shutting down")
	}
	if !s.s.RuntimeCheckpointEnabled() || !dispatcher.UserQuotaPolicyReady() {
		return nil, status.Error(codes.FailedPrecondition, "An applied user quota policy and native checkpoint are required")
	}
	if err := s.updateRuntimeLocked(ctx, core.RequireUserQuotaConfig); err != nil {
		return nil, err
	}
	return structpb.NewStruct(map[string]any{"hash": runtimeHash(s.runtimeConfiguration(ctx)), "generation": fmt.Sprint(s.runtime.sequence), "userQuotaRequired": s.s.RequiresUserQuota(), "coreProcessId": core.ProcessID()})
}
