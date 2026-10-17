package dispatcher

import (
	"context"
	"testing"

	appstats "github.com/xtls/xray-core/app/stats"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
)

func TestPreparingProfileCannotDispatchBeforeCommit(t *testing.T) {
	owner, err := core.NewProfile(context.Background(), &core.Config{App: []*serial.TypedMessage{serial.ToTypedMessage(&appstats.Config{})}})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	profile, err := core.NewStagedManagedProfile(owner, &core.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer profile.Close()
	if err := profile.Start(); err != nil {
		t.Fatal(err)
	}
	if profile.RuntimeManagementReady() || !profile.RuntimeAdmissionFenced() {
		t.Fatal("a staged graph became ready before activation")
	}
	d := new(DefaultDispatcher)
	destination := net.TCPDestination(net.LocalHostIP, 443)
	if link, err := d.Dispatch(profile.RuntimeContext(), destination); err == nil || link != nil {
		t.Fatal("preparing graph dispatched traffic")
	}
	if err := d.DispatchLink(profile.RuntimeContext(), destination, nil); err == nil {
		t.Fatal("preparing graph dispatched an existing link")
	}
	owner.ManagementAccess().Lock()
	err = owner.CommitProfileTCPListeners([]*core.Instance{profile}, nil, func() error { return nil })
	owner.ManagementAccess().Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !profile.RuntimeManagementReady() || profile.RuntimeAdmissionFenced() || profile.ProfilePreparing() {
		t.Fatal("activation did not release the staged admission fence")
	}
}

func TestPreparingWholePlanCannotDispatchBeforeStartup(t *testing.T) {
	config, err := core.WithProfileBindings(&core.Config{App: []*serial.TypedMessage{serial.ToTypedMessage(&appstats.Config{})}}, []core.ProfileBinding{{BindingID: "11111111-1111-4111-8111-111111111111", ProfileID: "22222222-2222-4222-8222-222222222222", InboundID: "33333333-3333-4333-8333-333333333333", Config: &core.Config{}}})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := core.NewProfile(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	child, err := core.NewManagedProfile(owner, &core.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	d := new(DefaultDispatcher)
	for _, instance := range []*core.Instance{owner, child} {
		if !instance.ProfilePreparing() {
			t.Fatal("an uncommitted startup graph lacks its admission fence")
		}
		if link, err := d.Dispatch(instance.RuntimeContext(), net.TCPDestination(net.LocalHostIP, 443)); err == nil || link != nil {
			t.Fatal("whole-plan startup dispatched traffic")
		}
	}
}
