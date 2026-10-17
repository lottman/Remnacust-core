//go:build !wasm

package command

import (
	"context"
	"sort"
	"sync"

	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// RuntimeProfileTarget identifies a graph, independently of its local tags.
type RuntimeProfileTarget struct {
	BindingID    string
	Instance     *core.Instance
	Config       *core.Config
	ExpectedHash string
}

// Serialize batches before taking controller locks: overlapping batches cannot
// invert their lock order by supplying different binding names for an instance.
var runtimeBatchOperation sync.Mutex

// ApplyRuntimeProfileBatch stages every existing graph before changing admission.
// persist must atomically durably replace the complete owning plan, and return
// nil only after that decision is durable. An error must leave the previous
// durable plan intact: a store with an ambiguous commit result needs its own
// recovery protocol before it can implement this callback. It must not call a
// core controller.
// This native primitive does not enroll graphs, add a public API or advertise a
// delivery capability. The owning fleet service must supply manifest persistence
// and route later single-graph mutations through that same manifest transaction.
func ApplyRuntimeProfileBatch(ctx context.Context, owner *core.Instance, targets []RuntimeProfileTarget, persist func(map[string]*core.Config) error) (map[string]string, error) {
	return applyRuntimeProfileBatch(ctx, owner, targets, persist, nil, false)
}

func applyRuntimeProfileBatch(ctx context.Context, owner *core.Instance, targets []RuntimeProfileTarget, persist func(map[string]*core.Config) error, validateLocked func() error, forcePersist bool) (map[string]string, error) {
	if owner == nil || owner.AccountingOwner() != owner || persist == nil || len(targets) == 0 || len(targets) > 512 {
		return nil, status.Error(codes.InvalidArgument, "A profile batch requires an accounting owner, bounded targets and durable persistence")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished, accepted := owner.TrackRuntimeWorker(cancel)
	if !accepted {
		return nil, status.Error(codes.Unavailable, "The profile owner is shutting down")
	}
	defer finished()
	runtimeBatchOperation.Lock()
	defer runtimeBatchOperation.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	targets = append([]RuntimeProfileTarget(nil), targets...)
	sort.Slice(targets, func(i, j int) bool { return targets[i].BindingID < targets[j].BindingID })
	controllers := make([]*handlerServer, len(targets))
	seen := make(map[*handlerServer]bool)
	for i, target := range targets {
		if target.BindingID == "" || len(target.BindingID) > 200 || (i > 0 && targets[i-1].BindingID == target.BindingID) || target.Instance == nil || target.Instance == owner || target.Instance.AccountingOwner() != owner || target.Config == nil {
			return nil, status.Error(codes.InvalidArgument, "Profile batch identity or accounting owner mismatch")
		}
		controller, ok := target.Instance.RuntimeController().(*handlerServer)
		if !ok || controller.s != target.Instance || seen[controller] {
			return nil, status.Error(codes.FailedPrecondition, "Each profile requires its own native runtime controller")
		}
		seen[controller] = true
		controllers[i] = controller
	}
	management := owner.ManagementAccess()
	management.Lock()
	defer management.Unlock()
	if validateLocked != nil {
		// The private whole-plan API validates the owning manifest CAS here.
		// Derive child revisions under that same lock, including API user edits.
		if err := validateLocked(); err != nil {
			return nil, err
		}
		for i := range targets {
			targets[i].ExpectedHash = runtimeHash(controllers[i].runtimeConfiguration(ctx))
		}
	}
	var prepared []*runtimePreparation
	defer func() {
		for _, entry := range prepared {
			entry.abort()
		}
	}()
	snapshots := make(map[string]*core.Config, len(targets))
	changed := false
	for i, target := range targets {
		md, _ := metadata.FromIncomingContext(ctx)
		md = md.Copy()
		md.Set("if-match", target.ExpectedHash)
		entry, err := controllers[i].prepareRuntimeLocked(metadata.NewIncomingContext(ctx, md), target.Config)
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, entry)
		snapshots[target.BindingID] = proto.Clone(entry.target).(*core.Config)
		changed = changed || !entry.unchanged
	}
	// All affected listeners are fenced together. Existing streams retain their
	// dispatcher leases and do not need this barrier to continue transferring.
	for _, controller := range controllers {
		controller.s.RuntimeAccess().Lock()
	}
	unlock := func() {
		for i := len(controllers) - 1; i >= 0; i-- {
			controllers[i].s.RuntimeAccess().Unlock()
		}
	}
	var failure error
	for _, entry := range prepared {
		select {
		case <-entry.server.s.RuntimeDone():
			failure = status.Error(codes.Unavailable, "A profile is shutting down")
		default:
		}
		if failure != nil {
			break
		}
		if failure = entry.installUsers(ctx); failure != nil {
			break
		}
	}
	if failure == nil {
		failure = ctx.Err()
	}
	if failure == nil && (changed || forcePersist) {
		failure = persist(snapshots)
	}
	if failure != nil {
		var cleanup []func()
		var rollback error
		for i := len(prepared) - 1; i >= 0; i-- {
			closes, err := prepared[i].rollbackUsers()
			cleanup = append(cleanup, closes...)
			if err != nil {
				rollback = err
			}
		}
		unlock()
		for _, close := range cleanup {
			close()
		}
		if rollback != nil {
			return nil, status.Error(codes.Internal, "Profile credential rollback failed; affected profiles require administrator recovery")
		}
		return nil, failure
	}
	// Persistence is the decision point. Cancellation after it cannot turn a
	// committed plan into an error or leave only some graphs activated.
	cleanup := make([][]func(), len(prepared))
	result := make(map[string]string, len(prepared))
	for i, entry := range prepared {
		cleanup[i] = entry.commitLocked(context.WithoutCancel(ctx))
		result[targets[i].BindingID] = runtimeHash(entry.target)
	}
	unlock()
	for i, entry := range prepared {
		entry.finish(cleanup[i])
	}
	return result, nil
}
