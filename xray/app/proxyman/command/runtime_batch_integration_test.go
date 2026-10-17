//go:build !wasm

package command_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	control "github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

func withoutRuntimeProfileUsers(t *testing.T, config *core.Config) *core.Config {
	t.Helper()
	config = proto.Clone(config).(*core.Config)
	settings, err := config.Inbound[0].ProxySettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	field := settings.ProtoReflect().Descriptor().Fields().ByName("users")
	if field == nil {
		t.Fatal("test inbound has no users")
	}
	settings.ProtoReflect().Clear(field)
	config.Inbound[0].ProxySettings = serial.ToTypedMessage(settings)
	return config
}

func TestRuntimeProfileBatchStagesAllGraphsBeforeDurableDecision(t *testing.T) {
	a, b, c := runtimeEcho(t, "A:"), runtimeEcho(t, "B:"), runtimeEcho(t, "C:")
	owner, err := core.NewProfile(context.Background(), runtimeBuildJSON(t, map[string]interface{}{
		"stats": map[string]interface{}{}, "log": map[string]interface{}{"loglevel": "none"},
		"outbounds": []interface{}{map[string]interface{}{"tag": "root", "protocol": "freedom"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	if err = owner.Start(); err != nil {
		t.Fatal(err)
	}
	portA, portB, apiA, apiB := availableRuntimePort(t), availableRuntimePort(t), availableRuntimePort(t), availableRuntimePort(t)
	configA, configB := runtimeTestConfig(t, apiA, portA, a, 0), runtimeTestConfig(t, apiB, portB, b, 0)
	files := t.TempDir()
	makeProfile := func(config *core.Config, name string) *core.Instance {
		profile, err := core.NewManagedProfile(owner, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { profile.Close() })
		profile.SetRuntimeJournal(filepath.Join(files, "state", name+".pb"))
		if err := profile.Start(); err != nil {
			t.Fatal(err)
		}
		return profile
	}
	profileA, profileB := makeProfile(configA, "a"), makeProfile(configB, "b")
	client := func(port int) *grpc.ClientConn {
		conn, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", port), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	apiConnA, apiConnB := client(apiA), client(apiB)
	beforeA, beforeB := runtimeInspect(t, apiConnA), runtimeInspect(t, apiConnB)
	oldA, oldB := runtimeClient(t, portA, a), runtimeClient(t, portB, b)
	runtimePing(t, oldA, "A:", true)
	runtimePing(t, oldB, "B:", true)
	nextA, nextB := runtimeTestConfig(t, apiA, portA, c, 0), runtimeTestConfig(t, apiB, portB, c, 0)
	targets := []control.RuntimeProfileTarget{
		{BindingID: "b", Instance: profileB, Config: nextB, ExpectedHash: beforeB},
		{BindingID: "a", Instance: profileA, Config: nextA, ExpectedHash: beforeA},
	}
	calls := 0
	persistFailure := errors.New("controlled durable decision failure")
	persist := func(snapshots map[string]*core.Config) error {
		calls++
		if len(snapshots) != 2 || !proto.Equal(snapshots["a"], nextA) || !proto.Equal(snapshots["b"], nextB) {
			t.Error("incomplete plan supplied to persistence")
		}
		for _, profile := range []*core.Instance{profileA, profileB} {
			if profile.RuntimeAccess().TryRLock() {
				profile.RuntimeAccess().RUnlock()
				t.Error("profile admission was open during durable decision")
			}
		}
		return persistFailure
	}
	// A stale second graph must abort the first prepared graph without persistence.
	stale := append([]control.RuntimeProfileTarget(nil), targets...)
	stale[0].ExpectedHash = "stale"
	stagedPort := availableRuntimePort(t)
	stale[1].Config = runtimeTestConfig(t, apiA, portA, c, stagedPort)
	if _, err := control.ApplyRuntimeProfileBatch(context.Background(), owner, stale, persist); err == nil || calls != 0 {
		t.Fatal("stale batch reached persistence", err, calls)
	}
	probe, err := net.Listen("tcp", fmt.Sprint("127.0.0.1:", stagedPort))
	if err != nil {
		t.Fatal("aborted batch leaked its staged listener", err)
	}
	probe.Close()
	blocked, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blocked.Close() })
	bindFailure := append([]control.RuntimeProfileTarget(nil), targets...)
	bindFailure[1].Config = stale[1].Config
	bindFailure[0].Config = runtimeTestConfig(t, apiB, portB, c, blocked.Addr().(*net.TCPAddr).Port)
	if _, err := control.ApplyRuntimeProfileBatch(context.Background(), owner, bindFailure, persist); err == nil || calls != 0 {
		t.Fatal("failed second bind reached persistence", err, calls)
	}
	probe, err = net.Listen("tcp", fmt.Sprint("127.0.0.1:", stagedPort))
	if err != nil {
		t.Fatal("failed second bind leaked first staged listener", err)
	}
	probe.Close()
	if _, err := control.ApplyRuntimeProfileBatch(context.Background(), owner, targets, persist); !errors.Is(err, persistFailure) || calls != 1 {
		t.Fatal("durable failure was not returned", err, calls)
	}
	if runtimeInspect(t, apiConnA) != beforeA || runtimeInspect(t, apiConnB) != beforeB {
		t.Fatal("failed batch changed an active graph")
	}
	// A failure after credential mutation must restore users without revoking
	// established authenticated streams in any graph.
	credentialTargets := append([]control.RuntimeProfileTarget(nil), targets...)
	credentialTargets[1].Config = withoutRuntimeProfileUsers(t, nextA)
	if _, err := control.ApplyRuntimeProfileBatch(context.Background(), owner, credentialTargets, func(map[string]*core.Config) error { return persistFailure }); !errors.Is(err, persistFailure) {
		t.Fatal(err)
	}
	if runtimeInspect(t, apiConnA) != beforeA || runtimeInspect(t, apiConnB) != beforeB {
		t.Fatal("credential rollback changed a profile")
	}
	runtimePing(t, oldA, "A:", false)
	runtimePing(t, oldB, "B:", false)
	runtimePing(t, runtimeClient(t, portA, a), "A:", true)
	runtimePing(t, runtimeClient(t, portB, b), "B:", true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := control.ApplyRuntimeProfileBatch(ctx, owner, targets, func(snapshots map[string]*core.Config) error {
		if err := persist(snapshots); !errors.Is(err, persistFailure) {
			t.Fatal(err)
		}
		// Simulate caller disconnect after the durable commit decision.
		cancel()
		return nil
	})
	if err != nil || len(result) != 2 || calls != 2 {
		t.Fatal("batch did not commit both graphs", result, err, calls)
	}
	if runtimeInspect(t, apiConnA) != result["a"] || runtimeInspect(t, apiConnB) != result["b"] {
		t.Fatal("API revision does not match committed batch")
	}
	runtimePing(t, oldA, "A:", false)
	runtimePing(t, oldB, "B:", false)
	runtimePing(t, runtimeClient(t, portA, a), "C:", true)
	runtimePing(t, runtimeClient(t, portB, b), "C:", true)
	for i := range targets {
		targets[i].ExpectedHash = result[targets[i].BindingID]
	}
	if _, err := control.ApplyRuntimeProfileBatch(context.Background(), owner, targets, func(map[string]*core.Config) error { t.Fatal("idempotent batch persisted again"); return nil }); err != nil {
		t.Fatal(err)
	}
	// Removing A's credential cannot close B's same-tag, same-email stream.
	if err := runtimeApply(apiConnA, withoutRuntimeProfileUsers(t, nextA), result["a"]); err != nil {
		t.Fatal(err)
	}
	if _, err := oldA.Read(make([]byte, 1)); err == nil {
		t.Fatal("removed profile credential remained connected")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("credential removal did not close A")
	}
	runtimePing(t, oldB, "B:", false)
	runtimePing(t, runtimeClient(t, portB, b), "C:", true)

	if err := runtimeApply(apiConnA, nextA, runtimeInspect(t, apiConnA)); err != nil {
		t.Fatal(err)
	}
	readdedA := runtimeClient(t, portA, a)
	runtimePing(t, readdedA, "C:", true)
	_, err = control.NewHandlerServiceClient(apiConnA).AlterInbound(context.Background(), &control.AlterInboundRequest{
		Tag: "PORT-A", Operation: serial.ToTypedMessage(&control.RemoveUserOperation{Email: "runtime-user"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readdedA.Read(make([]byte, 1)); err == nil {
		t.Fatal("API removal left A connected")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("API removal did not close A")
	}
	runtimePing(t, oldB, "B:", false)

}
