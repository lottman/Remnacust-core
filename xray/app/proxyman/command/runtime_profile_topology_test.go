//go:build !wasm

package command_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	control "github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	vlessinbound "github.com/xtls/xray-core/proxy/vless/inbound"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestProfileTopologyRPCRebindsPortKeepsOldFlowAndRejectsFailedStage(t *testing.T) {
	a, b, c := runtimeEcho(t, "A:"), runtimeEcho(t, "B:"), runtimeEcho(t, "C:")
	portA, portB, apiPort := availableRuntimePort(t), availableRuntimePort(t), availableRuntimePort(t)
	build := func(port, destination int) *core.Config {
		return runtimeBuildJSON(t, map[string]interface{}{
			"log":       map[string]interface{}{"loglevel": "none"},
			"policy":    map[string]interface{}{"levels": map[string]interface{}{"0": map[string]interface{}{"uplinkOnly": 0, "downlinkOnly": 0}}},
			"inbounds":  []interface{}{map[string]interface{}{"tag": "SAME", "listen": "127.0.0.1", "port": port, "protocol": "vless", "settings": map[string]interface{}{"decryption": "none", "clients": []interface{}{map[string]interface{}{"id": runtimeTestUUID, "email": "topology-user"}}}}},
			"outbounds": []interface{}{map[string]interface{}{"tag": "direct", "protocol": "freedom", "settings": map[string]interface{}{"redirect": fmt.Sprint("127.0.0.1:", destination), "finalRules": []interface{}{map[string]interface{}{"action": "allow", "ip": []string{"127.0.0.1/32"}}}}}},
		})
	}
	base := runtimeBuildJSON(t, map[string]interface{}{"api": map[string]interface{}{"tag": "CONTROL", "listen": fmt.Sprint("127.0.0.1:", apiPort), "services": []string{"HandlerService"}}, "stats": map[string]interface{}{}, "log": map[string]interface{}{"loglevel": "none"}})
	binding := func(id string, config *core.Config) core.ProfileBinding {
		return core.ProfileBinding{BindingID: id, ProfileID: id, InboundID: id, Config: config}
	}
	rootConfig, err := core.WithProfileBindings(base, []core.ProfileBinding{binding(enrolledA, build(portA, a)), binding(enrolledB, build(portB, b))})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := core.NewProfile(context.Background(), rootConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	journal := filepath.Join(t.TempDir(), "state", "topology.pb")
	owner.SetRuntimeJournal(journal)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	rpc, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", apiPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.Close()
	inspect := func() *structpb.Struct {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out := new(structpb.Struct)
		if err := rpc.Invoke(ctx, control.ProfileControlInspectMethod, &structpb.Struct{}, out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	apply := func(target *core.Config, hash string) (*structpb.Struct, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("if-match", hash))
		out := new(structpb.Struct)
		err := rpc.Invoke(ctx, control.ProfileControlApplyMethod, target, out, grpc.MaxCallSendMsgSize(core.RuntimeJournalLimit))
		return out, err
	}
	desired := func(port int) *core.Config {
		target, err := core.WithProfileBindings(base, []core.ProfileBinding{binding(enrolledB, build(portB, b)), binding("33333333-3333-4333-8333-333333333333", build(port, c))})
		if err != nil {
			t.Fatal(err)
		}
		digest, err := core.ProfilePlanDigest(target)
		if err != nil {
			t.Fatal(err)
		}
		target, err = core.WithProfileDelivery(target, core.ProfileDelivery{Epoch: "1", Owner: enrolledA, Revision: "1", Enabled: true, InputsHash: strings.Repeat("a", 64), PlanHash: digest})
		if err != nil {
			t.Fatal(err)
		}
		return target
	}
	oldA := owner.ManagedProfile(enrolledA)
	stream := runtimeClient(t, portA, a)
	runtimePing(t, stream, "A:", true)
	stable := runtimeClient(t, portB, b)
	runtimePing(t, stable, "B:", true)
	before := inspect().GetFields()["hash"].GetStringValue()
	blocked, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apply(desired(blocked.Addr().(*net.TCPAddr).Port), before); err == nil {
		_ = blocked.Close()
		t.Fatal("occupied staged port committed")
	}
	_ = blocked.Close()
	if inspect().GetFields()["hash"].GetStringValue() != before {
		t.Fatal("failed staging changed the owning checkpoint")
	}
	runtimePing(t, stream, "A:", false)
	runtimePing(t, stable, "B:", false)
	target := desired(portA)
	if _, err := apply(target, before); err != nil {
		t.Fatal(err)
	}
	if owner.ManagedProfile(enrolledA) != nil || owner.ManagedProfile("33333333-3333-4333-8333-333333333333") == nil {
		t.Fatal("RPC did not publish the new profile roster")
	}
	runtimePing(t, runtimeClient(t, portA, c), "C:", true)
	runtimePing(t, stream, "A:", false)
	runtimePing(t, stable, "B:", false)
	if err := oldA.SaveRuntimeConfiguration(oldA.Configuration()); err == nil {
		t.Fatal("retired graph rewrote the current manifest")
	}
	saved, err := core.LoadRuntimeJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	bindings, _, err := core.ConfigProfileBindings(saved)
	if err != nil || len(bindings) != 2 || bindings[0].BindingID != enrolledB {
		t.Fatal("durable roster is not the published roster", err)
	}
	clock, err := core.ConfigProfileDelivery(saved)
	if err != nil || clock == nil || clock.Epoch != "1" {
		t.Fatal("topology clock was not persisted", err)
	}
	current := inspect().GetFields()["hash"].GetStringValue()
	if response, err := apply(target, current); err != nil || !response.GetFields()["unchanged"].GetBoolValue() {
		t.Fatal("topology replay was not idempotent", err)
	}
	_ = stream.Close()
	select {
	case <-oldA.RuntimeDone():
	case <-time.After(8 * time.Second):
		t.Fatal("retired graph did not close after its native flow drained")
	}
	runtimePing(t, stable, "B:", false)
	// Replace this binding's entire graph while retaining its logical identity.
	currentC := owner.ManagedProfile("33333333-3333-4333-8333-333333333333")
	drainingC := runtimeClient(t, portA, c)
	runtimePing(t, drainingC, "C:", true)
	changedBindings, _, err := core.ConfigProfileBindings(target)
	if err != nil {
		t.Fatal(err)
	}
	for index := range changedBindings {
		if changedBindings[index].BindingID == "33333333-3333-4333-8333-333333333333" {
			changedBindings[index].ProfileID = enrolledA
			changedBindings[index].InboundID = enrolledA
			changedBindings[index].Config = build(portA, a)
			changedBindings[index].Config.Inbound[0].Tag = "NEXT"
		}
	}
	nextRoot := proto.Clone(base).(*core.Config)
	nextRoot, err = core.WithProfileBindings(nextRoot, changedBindings)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := core.ProfilePlanDigest(nextRoot)
	if err != nil {
		t.Fatal(err)
	}
	nextRoot, err = core.WithProfileDelivery(nextRoot, core.ProfileDelivery{Epoch: "2", Owner: enrolledA, Revision: "2", Enabled: true, InputsHash: strings.Repeat("b", 64), PlanHash: digest})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apply(nextRoot, inspect().GetFields()["hash"].GetStringValue()); err != nil {
		t.Fatal(err)
	}
	newC := owner.ManagedProfile("33333333-3333-4333-8333-333333333333")
	if newC == currentC {
		t.Fatal("provenance change retained the old graph")
	}
	activeC := runtimeClient(t, portA, a)
	runtimePing(t, activeC, "A:", true)
	runtimePing(t, drainingC, "C:", false)
	removed := newC.Configuration()
	proxyConfig, err := removed.Inbound[0].ProxySettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	proxyConfig.(*vlessinbound.Config).Users = nil
	removed.Inbound[0].ProxySettings = serial.ToTypedMessage(proxyConfig)
	if _, err := control.ApplyEnrolledProfileBatch(context.Background(), owner, []control.EnrolledProfileTarget{{BindingID: "33333333-3333-4333-8333-333333333333", Config: removed, ExpectedHash: enrolledHash(t, newC.Configuration())}}); err != nil {
		t.Fatal("user removal reconciliation failed", err)
	}
	for _, conn := range []net.Conn{drainingC, activeC} {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		var value [1]byte
		if _, err := conn.Read(value[:]); err == nil {
			t.Fatal("revocation left an authenticated binding flow open")
		} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatal("revocation failed to close a binding flow")
		}
	}
	runtimePing(t, stable, "B:", false)
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	startTopologyRestartProcess(t, journal)
	runtimePing(t, runtimeClient(t, portB, b), "B:", true)
	denied := runtimeClient(t, portA, a)
	_ = denied.SetReadDeadline(time.Now().Add(3 * time.Second))
	var reply [1]byte
	if _, err := denied.Read(reply[:]); err == nil {
		t.Fatal("deleted credentials worked after process restart")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("deleted credentials were not rejected after restart")
	}
	restored := inspect()
	if restored.GetFields()["delivery"].GetStructValue().GetFields()["epoch"].GetStringValue() != "2" {
		t.Fatal("process restart lost the topology delivery clock")
	}
	for _, value := range restored.GetFields()["graphs"].GetListValue().GetValues() {
		if value.GetStructValue().GetFields()["bindingUuid"].GetStringValue() == enrolledA {
			t.Fatal("restart resurrected a removed binding")
		}
	}
}

func startTopologyRestartProcess(t *testing.T, journal string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	child := exec.CommandContext(ctx, executable, "-test.run=^TestEnrolledProfileManifestRestartProcess$", "-test.timeout=25s")
	child.Env = append(os.Environ(), "REMNACUST_PROFILE_MANIFEST_RESTART_TEST="+journal)
	stdout, err := child.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdin, err := child.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	child.Stderr = &stderr
	if err := child.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	var once sync.Once
	var waitErr error
	stop := func() { once.Do(func() { _ = stdin.Close(); waitErr = child.Wait(); cancel() }) }
	t.Cleanup(func() {
		stop()
		if waitErr != nil {
			t.Errorf("topology restart process failed: %v %s", waitErr, stderr.String())
		}
	})
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "PROFILE_MANIFEST_READY" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			stop()
			t.Fatalf("topology restart failed: %v %s", waitErr, stderr.String())
		}
	case <-ctx.Done():
		stop()
		t.Fatal("topology restart readiness timed out")
	}
}
