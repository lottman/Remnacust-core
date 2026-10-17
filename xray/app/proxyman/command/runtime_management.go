//go:build !wasm

package command

import (
	"context"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features"
	"github.com/xtls/xray-core/features/inbound"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func (s *handlerServer) Update(ctx context.Context, transform func(*core.Config) error) error {
	s.operation.Lock()
	defer s.operation.Unlock()
	return s.updateRuntimeLocked(ctx, transform)
}

func (s *handlerServer) updateRuntimeLocked(ctx context.Context, transform func(*core.Config) error) error {
	current := s.runtimeConfiguration(ctx)
	if current == nil {
		return status.Error(codes.FailedPrecondition, "Runtime configuration is unavailable")
	}
	expected := runtimeHash(current)
	if err := transform(current); err != nil {
		return err
	}
	md, _ := metadata.FromIncomingContext(ctx)
	md = md.Copy()
	md.Set("if-match", expected)
	_, err := s.applyRuntimeLocked(metadata.NewIncomingContext(ctx, md), current)
	return err
}

// The existing explicit RemoveInbound RPC retains its immediate-close contract.
// Only full configuration reconciliation performs a graceful listener drain.
func (s *handlerServer) removeRuntimeInbound(ctx context.Context, tag string) error {
	s.operation.Lock()
	defer s.operation.Unlock()
	handler, err := s.ihm.GetHandler(ctx, tag)
	if err != nil {
		return status.Error(codes.NotFound, "Inbound tag not found")
	}
	err = s.updateRuntimeLocked(ctx, func(config *core.Config) error {
		for index, existing := range config.Inbound {
			if existing.Tag == tag {
				config.Inbound = append(config.Inbound[:index], config.Inbound[index+1:]...)
				return nil
			}
		}
		return status.Error(codes.NotFound, "Inbound tag not found")
	})
	if err != nil {
		return err
	}
	if manager, ok := s.ihm.(interface{ CloseRetired(inbound.Handler) error }); ok {
		return manager.CloseRetired(handler)
	}
	return handler.Close()
}

func (s *handlerServer) View(ctx context.Context, typ interface{}, callback func(features.Feature) error) error {
	s.operation.Lock()
	defer s.operation.Unlock()
	if s.runtime.stopped {
		return status.Error(codes.Unavailable, "The core is shutting down")
	}
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	instance := s.s
	if s.runtime.generation != nil {
		instance = core.MustFromContext(s.runtime.generation.Context)
	}
	feature := instance.GetFeature(typ)
	if feature == nil {
		return status.Error(codes.Unimplemented, "Runtime feature is unavailable")
	}
	return callback(feature)
}
