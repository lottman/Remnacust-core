//go:build !wasm

package command

import (
	"context"

	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/features/outbound"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type EnrolledProfileTarget struct {
	BindingID    string
	Config       *core.Config
	ExpectedHash string
}

func ensureProfileController(instance *core.Instance) error {
	if instance.RuntimeController() != nil {
		return nil
	}
	if !instance.RuntimeManagementReady() {
		return status.Error(codes.Unavailable, "The profile is not running")
	}
	im, inboundOK := instance.GetFeature(inbound.ManagerType()).(inbound.Manager)
	om, outboundOK := instance.GetFeature(outbound.ManagerType()).(outbound.Manager)
	if !inboundOK || !outboundOK {
		return status.Error(codes.FailedPrecondition, "The graph has no native port managers")
	}
	handler := &handlerServer{s: instance, ihm: im, ohm: om, operation: instance.ManagementAccess()}
	if err := initializeRuntimeControl(handler); err != nil {
		// A concurrent initializer may have won; it must be this native type.
		if _, ok := instance.RuntimeController().(*handlerServer); !ok {
			return err
		}
	}
	return nil
}

// ApplyEnrolledProfileBatch uses the real owning manifest rather than a caller
// persistence stub. Graphs without their own API listener get an internal native
// controller; no new network listener or client protocol is introduced.
func ApplyEnrolledProfileBatch(ctx context.Context, owner *core.Instance, targets []EnrolledProfileTarget) (map[string]string, error) {
	if owner == nil || len(targets) == 0 || len(targets) > 512 {
		return nil, status.Error(codes.InvalidArgument, "Invalid enrolled profile update set")
	}
	native := make([]RuntimeProfileTarget, 0, len(targets))
	for _, target := range targets {
		instance := owner.ManagedProfile(target.BindingID)
		if instance == nil {
			return nil, status.Error(codes.NotFound, "Profile binding is not enrolled")
		}
		if err := ensureProfileController(instance); err != nil {
			return nil, err
		}
		native = append(native, RuntimeProfileTarget{BindingID: target.BindingID, Instance: instance, Config: target.Config, ExpectedHash: target.ExpectedHash})
	}
	return ApplyRuntimeProfileBatch(ctx, owner, native, owner.SaveManagedProfileBatch)
}
