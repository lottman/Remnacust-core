//go:build !wasm

package command

import (
	"context"

	"github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ProfileBindingMetadata selects a graph over the existing private HandlerService.
// It changes neither client subscription formats nor public proxy protocols.
const ProfileBindingMetadata = "x-remnacust-profile-binding"
const RetiredUserRemovalMetadata = "x-remnacust-retired-user-removal"

// Explicit node-wide removal is distinct from a scoped entitlement change.
// It cannot mutate any current graph and never relies on a network peer IP.
func (s *handlerServer) removeRetiredUserLocked(ctx context.Context, request *AlterInboundRequest) (bool, error) {
	values := metadata.ValueFromIncomingContext(ctx, RetiredUserRemovalMetadata)
	if len(values) == 0 {
		return false, nil
	}
	if len(values) != 1 || values[0] != "1" || request.Tag != "" || len(metadata.ValueFromIncomingContext(ctx, ProfileBindingMetadata)) != 0 {
		return true, status.Error(codes.InvalidArgument, "Invalid retired-user removal scope")
	}
	if s.s.AccountingOwner() != s.s {
		return true, status.Error(codes.PermissionDenied, "Retired-user removal requires the node owner")
	}
	if !s.s.RuntimeManagementReady() {
		return true, status.Error(codes.Unavailable, "The node owner is not ready")
	}
	if request.Operation == nil {
		return true, status.Error(codes.InvalidArgument, "A retired-user removal operation is required")
	}
	operation, err := request.Operation.GetInstance()
	if err != nil {
		return true, status.Error(codes.InvalidArgument, "Invalid retired-user removal operation")
	}
	remove, ok := operation.(*RemoveUserOperation)
	if !ok || len(remove.Email) == 0 || len(remove.Email) > 1024 {
		return true, status.Error(codes.InvalidArgument, "A bounded user identity is required")
	}
	for _, instance := range s.s.RetiredManagedProfiles() {
		dispatcher.RevokeRetiredInstanceUserLinks(instance, remove.Email)
	}
	return true, nil
}

// Caller holds the accounting owner's management mutex through lookup and use.
// Legacy tag-only SDKs remain usable when exactly one live graph owns the tag.
func (s *handlerServer) inboundUserTargetLocked(ctx context.Context, tag string) (*handlerServer, error) {
	selected := metadata.ValueFromIncomingContext(ctx, ProfileBindingMetadata)
	if len(selected) > 1 || len(selected) == 1 && selected[0] == "" {
		return nil, status.Error(codes.InvalidArgument, "Select exactly one nonempty profile binding")
	}
	if !s.s.RuntimeManagementReady() {
		return nil, status.Error(codes.Unavailable, "The owning profile plan is not ready for management")
	}
	bindings, enrolled, err := core.ConfigProfileBindings(s.s.Configuration())
	if err != nil {
		return nil, status.Error(codes.Internal, "The owning profile manifest needs recovery")
	}
	if len(selected) == 0 && !enrolled {
		return s, nil
	}
	var target *core.Instance
	if len(selected) == 1 {
		for _, binding := range bindings {
			if binding.BindingID == selected[0] {
				target = s.s.ManagedProfile(binding.BindingID)
				break
			}
		}
		if target == nil {
			return nil, status.Error(codes.NotFound, "Profile binding is not enrolled")
		}
	} else {
		for _, inbound := range s.s.Configuration().Inbound {
			if inbound.Tag == tag {
				target = s.s
				break
			}
		}
		for _, binding := range bindings {
			for _, inbound := range binding.Config.Inbound {
				if inbound.Tag != tag {
					continue
				}
				if target != nil {
					return nil, status.Error(codes.FailedPrecondition, "Inbound tag is ambiguous; select its profile binding")
				}
				target = s.s.ManagedProfile(binding.BindingID)
				if target == nil {
					return nil, status.Error(codes.Unavailable, "The selected profile is not running")
				}
			}
		}
		if target == nil {
			return nil, status.Error(codes.NotFound, "Inbound tag is not enrolled")
		}
	}
	if target == s.s {
		return s, nil
	}
	if !target.RuntimeManagementReady() {
		return nil, status.Error(codes.Unavailable, "The selected profile is not ready for management")
	}
	if err := ensureProfileController(target); err != nil {
		return nil, err
	}
	handler, ok := target.RuntimeController().(*handlerServer)
	if !ok {
		return nil, status.Error(codes.Unavailable, "The selected profile controller is unavailable")
	}
	return handler, nil
}
