package core

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/features"
	"github.com/xtls/xray-core/features/stats"
)

type managedProfileAccounting struct {
	stats.NoopManager
	starts atomic.Int32
	closes atomic.Int32
}

func (m *managedProfileAccounting) Start() error { m.starts.Add(1); return nil }
func (m *managedProfileAccounting) Close() error { m.closes.Add(1); return nil }

func managedProfileParent(t *testing.T) (*Instance, *managedProfileAccounting) {
	t.Helper()
	parent := &Instance{ctx: context.Background()}
	manager := new(managedProfileAccounting)
	if err := parent.AddFeature(manager); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { parent.Close() })
	return parent, manager
}

type managedProfileController struct{ id int }

func (*managedProfileController) Update(context.Context, func(*Config) error) error { return nil }
func (*managedProfileController) View(context.Context, interface{}, func(features.Feature) error) error {
	return nil
}

func TestManagedProfileOwnsLifecycleControllerAndBorrowsOnlyAccounting(t *testing.T) {
	parent, manager := managedProfileParent(t)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	parent.SetRuntimeController(&managedProfileController{id: 1})
	profile, err := NewManagedProfile(parent, &Config{})
	if err != nil {
		t.Fatal(err)
	}
	controller := &managedProfileController{id: 2}
	profile.SetRuntimeController(controller)
	if profile.RuntimeController() != controller || profile.RuntimeAccess() == parent.RuntimeAccess() {
		t.Fatal("managed graph borrowed controller or admission instead of accounting")
	}
	if profile.AccountingOwner() != parent || profile.GetFeature(stats.ManagerType()) != manager {
		t.Fatal("managed graph did not use authoritative node accounting")
	}
	if err := profile.Start(); err != nil {
		t.Fatal(err)
	}
	if manager.starts.Load() != 1 {
		t.Fatal("profile restarted accounting")
	}
	generation := NewIsolatedFeatureScope(profile)
	defer generation.Close()
	if generation.Instance.AccountingOwner() != parent {
		t.Fatal("generation lost accounting owner")
	}
	if err := profile.Close(); err != nil {
		t.Fatal(err)
	}
	if manager.closes.Load() != 0 || !parent.IsRunning() {
		t.Fatal("profile closed node accounting")
	}
	select {
	case <-parent.RuntimeDone():
		t.Fatal("profile closed node lifetime")
	default:
	}
	select {
	case <-profile.RuntimeDone():
	default:
		t.Fatal("profile lifetime did not end")
	}
	if len(parent.managedProfiles) != 0 {
		t.Fatal("closed profile retained by parent registry")
	}
	if err := profile.Start(); err == nil {
		t.Fatal("closed managed graph restarted outside parent registry")
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if manager.closes.Load() != 1 {
		t.Fatal("node did not close accounting exactly once")
	}
}

func TestManagedProfileConstructionFailureCannotCloseParentAccounting(t *testing.T) {
	parent, manager := managedProfileParent(t)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManagedProfile(parent, &Config{App: []*serial.TypedMessage{{Type: "missing.managed.feature"}}}); err == nil {
		t.Fatal("invalid managed graph accepted")
	}
	if manager.closes.Load() != 0 || len(parent.managedProfiles) != 0 {
		t.Fatal("failed construction changed accounting or registry")
	}
	_ = parent.Close()
	if _, err := NewManagedProfile(parent, &Config{}); err == nil {
		t.Fatal("profile admitted after owner shutdown")
	}
	if _, err := NewManagedProfile(nil, &Config{}); err == nil {
		t.Fatal("missing owner accepted")
	}
	if _, err := NewManagedProfile(&Instance{}, &Config{}); err == nil {
		t.Fatal("uninitialized owner accepted")
	}
	scope := NewFeatureScope(parent)
	defer scope.Close()
	if _, err := NewManagedProfile(scope.Instance, &Config{}); err == nil {
		t.Fatal("borrowed generation accepted as lifecycle owner")
	}
	plain, err := New(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, err := NewManagedProfile(plain, &Config{}); err == nil {
		t.Fatal("disabled node accounting accepted")
	}
}

type managedProfileCloseBarrier struct {
	entered, release       chan struct{}
	manager                *managedProfileAccounting
	closedBeforeAccounting atomic.Bool
}

func (*managedProfileCloseBarrier) Type() interface{} { return (*managedProfileCloseBarrier)(nil) }
func (*managedProfileCloseBarrier) Start() error      { return nil }
func (f *managedProfileCloseBarrier) Close() error {
	close(f.entered)
	<-f.release
	f.closedBeforeAccounting.Store(f.manager.closes.Load() == 0)
	return nil
}

func TestManagedProfileParentWaitsForAlreadyClosingChildBeforeAccounting(t *testing.T) {
	parent, manager := managedProfileParent(t)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	profile, err := NewManagedProfile(parent, &Config{})
	if err != nil {
		t.Fatal(err)
	}
	barrier := &managedProfileCloseBarrier{entered: make(chan struct{}), release: make(chan struct{}), manager: manager}
	release := sync.OnceFunc(func() { close(barrier.release) })
	defer release()
	if err := profile.AddFeature(barrier); err != nil {
		t.Fatal(err)
	}
	if err := profile.Start(); err != nil {
		t.Fatal(err)
	}
	childClosed, parentClosed := make(chan error, 1), make(chan error, 1)
	go func() { childClosed <- profile.Close() }()
	select {
	case <-barrier.entered:
	case <-time.After(time.Second):
		t.Fatal("child did not close")
	}
	go func() { parentClosed <- parent.Close() }()
	select {
	case <-parent.RuntimeDone():
	case <-time.After(time.Second):
		t.Fatal("parent did not start shutdown")
	}
	if manager.closes.Load() != 0 {
		t.Fatal("accounting closed before child resources")
	}
	release()
	for _, done := range []chan error{childClosed, parentClosed} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("shutdown did not finish")
		}
	}
	if !barrier.closedBeforeAccounting.Load() || manager.closes.Load() != 1 {
		t.Fatal("wrong accounting teardown order")
	}
}

func TestManagedProfileShutdownAdmissionRaceLeavesNoLiveOrRetainedGraph(t *testing.T) {
	parent, manager := managedProfileParent(t)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	accepted := make(chan *Instance, 32)
	begin := make(chan struct{})
	for range 32 {
		workers.Go(func() {
			<-begin
			profile, err := NewManagedProfile(parent, &Config{})
			if err == nil {
				accepted <- profile
			}
		})
	}
	close(begin)
	_ = parent.Close()
	workers.Wait()
	close(accepted)
	for profile := range accepted {
		select {
		case <-profile.RuntimeDone():
		default:
			t.Fatal("admitted profile survived owner shutdown")
		}
	}
	if len(parent.managedProfiles) != 0 || manager.closes.Load() != 1 {
		t.Fatal("shutdown retained profiles or closed accounting twice")
	}
}

func TestManagedProfileCannotStartBeforeOwnerAndNestedShutdownClosesAccountingOnce(t *testing.T) {
	parent, manager := managedProfileParent(t)
	profile, err := NewManagedProfile(parent, &Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := profile.Start(); err == nil {
		t.Fatal("profile admitted traffic before accounting owner started")
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	if err := profile.Start(); err != nil {
		t.Fatal(err)
	}
	nested, err := NewManagedProfile(profile, &Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := nested.Start(); err != nil {
		t.Fatal(err)
	}
	if nested.AccountingOwner() != parent {
		t.Fatal("nested profile created a second financial source")
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	for _, child := range []*Instance{profile, nested} {
		select {
		case <-child.RuntimeDone():
		default:
			t.Fatal("nested profile survived node shutdown")
		}
	}
	if manager.starts.Load() != 1 || manager.closes.Load() != 1 {
		t.Fatal("nested profiles changed accounting lifecycle")
	}
}

func TestManagedProfileShutdownWaitsForFinalWorkerAccounting(t *testing.T) {
	parent, manager := managedProfileParent(t)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	profile, err := NewManagedProfile(parent, &Config{})
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	finished, accepted := profile.TrackRuntimeWorker(func() { close(stop) })
	if !accepted {
		t.Fatal("worker rejected before shutdown")
	}
	var finalBytes atomic.Int64
	var accountingWasLive atomic.Bool
	go func() {
		<-stop
		accountingWasLive.Store(manager.closes.Load() == 0)
		finalBytes.Store(7)
		finished()
		finished()
	}()
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if !accountingWasLive.Load() || finalBytes.Load() != 7 || manager.closes.Load() != 1 {
		t.Fatal("statistics closed before final worker accounting")
	}
	if _, accepted := profile.TrackRuntimeWorker(func() {}); accepted {
		t.Fatal("worker admitted after shutdown")
	}
	if _, accepted := parent.TrackRuntimeWorker(nil); accepted {
		t.Fatal("worker without shutdown action admitted")
	}
}

type managedProfileStartHook struct{ start func() error }

func (*managedProfileStartHook) Type() interface{} { return (*managedProfileStartHook)(nil) }
func (h *managedProfileStartHook) Start() error    { return h.start() }
func (*managedProfileStartHook) Close() error      { return nil }

func TestManagedProfileAdmissionDuringOwnerStartupDoesNotDeadlock(t *testing.T) {
	parent, _ := managedProfileParent(t)
	profile, err := NewManagedProfile(parent, &Config{})
	if err != nil {
		t.Fatal(err)
	}
	var rejected bool
	if err := parent.AddFeature(&managedProfileStartHook{start: func() error { rejected = profile.Start() != nil; return nil }}); err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	if !rejected {
		t.Fatal("profile started before owner initialization finished")
	}
	if err := profile.Start(); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
}
