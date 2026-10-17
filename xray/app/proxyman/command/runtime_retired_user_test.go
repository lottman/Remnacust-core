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
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestNodeWideRetiredUserRemovalReachesDeletedBindingsAndPreservesCurrentGraph(t *testing.T) {
	a, b := runtimeEcho(t, "A:"), runtimeEcho(t, "B:")
	portA, portB, apiPort := availableRuntimePort(t), availableRuntimePort(t), availableRuntimePort(t)
	build := func(port, destination int) *core.Config {
		return runtimeBuildJSON(t, map[string]interface{}{
			"log":    map[string]interface{}{"loglevel": "none"},
			"policy": map[string]interface{}{"levels": map[string]interface{}{"0": map[string]interface{}{"uplinkOnly": 0, "downlinkOnly": 0}}},
			"inbounds": []interface{}{map[string]interface{}{"tag": "LOCAL", "listen": "127.0.0.1", "port": port, "protocol": "vless", "settings": map[string]interface{}{
				"decryption": "none", "clients": []interface{}{map[string]interface{}{"id": runtimeTestUUID, "email": "900~retired-device"}}}}},
			"outbounds": []interface{}{map[string]interface{}{"tag": "direct", "protocol": "freedom", "settings": map[string]interface{}{
				"redirect": fmt.Sprint("127.0.0.1:", destination), "finalRules": []interface{}{map[string]interface{}{"action": "allow", "ip": []string{"127.0.0.1/32"}}}}}},
		})
	}
	base := runtimeBuildJSON(t, map[string]interface{}{
		"api":   map[string]interface{}{"tag": "CONTROL", "listen": fmt.Sprint("127.0.0.1:", apiPort), "services": []string{"HandlerService"}},
		"stats": map[string]interface{}{}, "log": map[string]interface{}{"loglevel": "none"},
	})
	bindings := []core.ProfileBinding{
		{BindingID: enrolledA, ProfileID: enrolledA, InboundID: enrolledA, Config: build(portA, a)},
		{BindingID: enrolledB, ProfileID: enrolledB, InboundID: enrolledB, Config: build(portB, b)},
	}
	initial, err := core.WithProfileBindings(base, bindings)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := core.NewProfile(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	owner.SetRuntimeJournal(filepath.Join(t.TempDir(), "private", "retired.pb"))
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	current := new(structpb.Struct)
	if err := rpc.Invoke(ctx, control.ProfileControlInspectMethod, &structpb.Struct{}, current); err != nil {
		t.Fatal(err)
	}
	target, err := core.WithProfileBindings(base, bindings[1:])
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
	applyCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("if-match", current.GetFields()["hash"].GetStringValue()))
	if err := rpc.Invoke(applyCtx, control.ProfileControlApplyMethod, target, new(structpb.Struct)); err != nil {
		t.Fatal(err)
	}
	if owner.ManagedProfile(enrolledA) != nil {
		t.Fatal("removed binding remained in the live roster")
	}
	runtimePing(t, streamA, "A:", false)
	client := control.NewHandlerServiceClient(rpc)
	remove := serial.ToTypedMessage(&control.RemoveUserOperation{Email: "900~retired-device"})
	for _, request := range []struct {
		pairs []string
		body  *control.AlterInboundRequest
	}{
		{[]string{control.RetiredUserRemovalMetadata, "2"}, &control.AlterInboundRequest{Operation: remove}},
		{[]string{control.RetiredUserRemovalMetadata, "1", control.ProfileBindingMetadata, enrolledB}, &control.AlterInboundRequest{Operation: remove}},
		{[]string{control.RetiredUserRemovalMetadata, "1"}, &control.AlterInboundRequest{Tag: "LOCAL", Operation: remove}},
		{[]string{control.RetiredUserRemovalMetadata, "1"}, &control.AlterInboundRequest{Operation: serial.ToTypedMessage(&control.AddUserOperation{})}},
		{[]string{control.RetiredUserRemovalMetadata, "1"}, &control.AlterInboundRequest{}},
	} {
		callCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs(request.pairs...))
		if _, err := client.AlterInbound(callCtx, request.body); err == nil {
			t.Fatal("invalid retired-user removal scope was accepted")
		}
	}
	runtimePing(t, streamA, "A:", false)
	runtimePing(t, streamB, "B:", false)
	removeCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs(control.RetiredUserRemovalMetadata, "1"))
	if _, err := client.AlterInbound(removeCtx, &control.AlterInboundRequest{Operation: remove}); err != nil {
		t.Fatal(err)
	}
	runtimeExpectClosed(t, streamA)
	runtimePing(t, streamB, "B:", false)
	// Normal scoped deletion persists current credentials separately.
	liveCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs(control.ProfileBindingMetadata, enrolledB))
	if _, err := client.AlterInbound(liveCtx, &control.AlterInboundRequest{Tag: "LOCAL", Operation: remove}); err != nil {
		t.Fatal(err)
	}
	runtimeExpectClosed(t, streamB)
}
