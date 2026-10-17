//go:build wasm

package command

import (
	"context"
	"github.com/xtls/xray-core/features/inbound"
	"google.golang.org/grpc"
)

// The browser configuration validator does not run a core or expose control RPCs.
type runtimeState struct{}

func registerRuntimeControl(*grpc.Server, *handlerServer) {}

func (s *handlerServer) inboundUserTargetLocked(context.Context, string) (*handlerServer, error) {
	return s, nil
}

func (s *handlerServer) removeRetiredUserLocked(context.Context, *AlterInboundRequest) (bool, error) {
	return false, nil
}

func (s *handlerServer) removeRuntimeInbound(ctx context.Context, tag string) error {
	return s.ihm.RemoveHandler(ctx, tag)
}

func (s *handlerServer) applyInboundPersisted(ctx context.Context, handler inbound.Handler, operation InboundOperation) error {
	return operation.ApplyInbound(ctx, handler)
}
