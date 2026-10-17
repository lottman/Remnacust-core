//go:build !wasm

package command_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	control "github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestSameBindingPortChangesDrainAndRevokeOnlyRemovedCredentials(t *testing.T) {
	a, b := runtimeEcho(t, "A:"), runtimeEcho(t, "B:")
	portA, portB := availableRuntimePort(t), availableRuntimePort(t)
	apiPort := availableRuntimePort(t)
	build := func(port, destination int, enabled bool) *core.Config {
		clients := []interface{}{}
		if enabled {
			clients = append(clients, map[string]interface{}{"id": runtimeTestUUID, "email": "shape-user"})
		}
		return runtimeBuildJSON(t, map[string]interface{}{
			"log":    map[string]interface{}{"loglevel": "none"},
			"policy": map[string]interface{}{"levels": map[string]interface{}{"0": map[string]interface{}{"uplinkOnly": 0, "downlinkOnly": 0}}},
			"inbounds": []interface{}{map[string]interface{}{"tag": "LOCAL", "listen": "127.0.0.1", "port": port, "protocol": "vless",
				"settings": map[string]interface{}{"decryption": "none", "clients": clients}}},
			"outbounds": []interface{}{map[string]interface{}{"tag": "direct", "protocol": "freedom", "settings": map[string]interface{}{
				"redirect": fmt.Sprint("127.0.0.1:", destination), "finalRules": []interface{}{map[string]interface{}{"action": "allow", "ip": []string{"127.0.0.1/32"}}}}}},
		})
	}
	base := runtimeBuildJSON(t, map[string]interface{}{
		"api":   map[string]interface{}{"tag": "CONTROL", "listen": fmt.Sprint("127.0.0.1:", apiPort), "services": []string{"HandlerService"}},
		"stats": map[string]interface{}{}, "log": map[string]interface{}{"loglevel": "none"},
	})
	bindings := func(port int, enabled bool) []core.ProfileBinding {
		return []core.ProfileBinding{
			{BindingID: enrolledA, ProfileID: enrolledA, InboundID: enrolledA, Config: build(port, a, enabled)},
			{BindingID: enrolledB, ProfileID: enrolledB, InboundID: enrolledB, Config: build(portB, b, true)},
		}
	}
	initial, err := core.WithProfileBindings(base, bindings(portA, true))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := core.NewProfile(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	owner.SetRuntimeJournal(filepath.Join(t.TempDir(), "private", "shape.pb"))
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	rootA, rootB := owner.ManagedProfile(enrolledA), owner.ManagedProfile(enrolledB)
	streamA, streamB := runtimeClient(t, portA, a), runtimeClient(t, portB, b)
	defer streamA.Close()
	defer streamB.Close()
	runtimePing(t, streamA, "A:", true)
	runtimePing(t, streamB, "B:", true)
	rpc, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", apiPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.Close()
	apply := func(port int, enabled bool, epoch string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		current := new(structpb.Struct)
		if err := rpc.Invoke(ctx, control.ProfileControlInspectMethod, &structpb.Struct{}, current); err != nil {
			t.Fatal(err)
		}
		target, err := core.WithProfileBindings(base, bindings(port, enabled))
		if err != nil {
			t.Fatal(err)
		}
		digest, err := core.ProfilePlanDigest(target)
		if err != nil {
			t.Fatal(err)
		}
		target, err = core.WithProfileDelivery(target, core.ProfileDelivery{Epoch: epoch, Owner: enrolledA, Revision: epoch, Enabled: true, InputsHash: strings.Repeat("a", 64), PlanHash: digest})
		if err != nil {
			t.Fatal(err)
		}
		ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("if-match", current.GetFields()["hash"].GetStringValue()))
		if err := rpc.Invoke(ctx, control.ProfileControlApplyMethod, target, new(structpb.Struct)); err != nil {
			t.Fatal(err)
		}
	}
	secondPort := availableRuntimePort(t)
	apply(secondPort, true, "1")
	if owner.ManagedProfile(enrolledA) == rootA {
		t.Fatal("same-binding port change did not create a new graph")
	}
	if owner.ManagedProfile(enrolledB) != rootB {
		t.Fatal("unmodified sibling graph was unnecessarily replaced")
	}
	runtimePing(t, streamA, "A:", false)
	runtimePing(t, streamB, "B:", false)
	secondA := runtimeClient(t, secondPort, a)
	defer secondA.Close()
	runtimePing(t, secondA, "A:", true)
	thirdPort := availableRuntimePort(t)
	apply(thirdPort, false, "2")
	// Both old A generations contain the removed identity; sibling B does too,
	// but its access was retained and must survive the same owning transaction.
	runtimeExpectClosed(t, streamA)
	runtimeExpectClosed(t, secondA)
	runtimePing(t, streamB, "B:", false)
	denied := runtimeClient(t, thirdPort, a)
	defer denied.Close()
	runtimeExpectClosed(t, denied)
}
