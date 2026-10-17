//go:build !wasm

package command_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	control "github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/core"
	"google.golang.org/protobuf/proto"
)

const enrolledA = "11111111-1111-4111-8111-111111111111"
const enrolledB = "22222222-2222-4222-8222-222222222222"

func enrolledHash(t *testing.T, config *core.Config) string {
	t.Helper()
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestEnrolledProfileManifestPersistsBatchAndAPIUpdateAcrossProcessRestart(t *testing.T) {
	a, b, c := runtimeEcho(t, "A:"), runtimeEcho(t, "B:"), runtimeEcho(t, "C:")
	portA, portB := availableRuntimePort(t), availableRuntimePort(t)
	apiPort := availableRuntimePort(t)
	build := func(port, destination int) *core.Config {
		return runtimeBuildJSON(t, map[string]interface{}{
			"log":       map[string]interface{}{"loglevel": "none"},
			"inbounds":  []interface{}{map[string]interface{}{"tag": "SAME", "listen": "127.0.0.1", "port": port, "protocol": "vless", "settings": map[string]interface{}{"decryption": "none", "clients": []interface{}{map[string]interface{}{"id": runtimeTestUUID, "email": "same-enrolled-user"}}}}},
			"outbounds": []interface{}{map[string]interface{}{"tag": "direct", "protocol": "freedom", "settings": map[string]interface{}{"redirect": fmt.Sprint("127.0.0.1:", destination), "finalRules": []interface{}{map[string]interface{}{"action": "allow", "ip": []string{"127.0.0.1/32"}}}}}},
		})
	}
	configA, configB := build(portA, a), build(portB, b)
	root := runtimeBuildJSON(t, map[string]interface{}{"api": map[string]interface{}{"tag": "CONTROL", "listen": fmt.Sprint("127.0.0.1:", apiPort), "services": []string{"HandlerService"}}, "stats": map[string]interface{}{}, "log": map[string]interface{}{"loglevel": "none"}, "outbounds": []interface{}{map[string]interface{}{"tag": "root", "protocol": "freedom"}}})
	root, err := core.WithProfileBindings(root, []core.ProfileBinding{
		{BindingID: enrolledA, ProfileID: enrolledA, InboundID: enrolledA, Config: configA},
		{BindingID: enrolledB, ProfileID: enrolledB, InboundID: enrolledB, Config: configB},
	})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "state", "manifest.pb")
	blocked, err := net.Listen("tcp", fmt.Sprint("127.0.0.1:", portB))
	if err != nil {
		t.Fatal(err)
	}
	failed, err := core.NewProfile(context.Background(), root)
	if err != nil {
		blocked.Close()
		t.Fatal(err)
	}
	failed.SetRuntimeJournal(filepath.Join(t.TempDir(), "state", "failed.pb"))
	if err := failed.Start(); err == nil {
		failed.Close()
		blocked.Close()
		t.Fatal("partial startup with an occupied second graph succeeded")
	}
	if !failed.RuntimeAdmissionFenced() || failed.ManagedProfile(enrolledA) != nil {
		failed.Close()
		blocked.Close()
		t.Fatal("failed startup retained an admitted partial plan")
	}
	if err := failed.Close(); err != nil {
		blocked.Close()
		t.Fatal(err)
	}
	probe, err := net.Listen("tcp", fmt.Sprint("127.0.0.1:", portA))
	if err != nil {
		blocked.Close()
		t.Fatal("failed startup leaked the first graph listener", err)
	}
	probe.Close()
	blocked.Close()
	owner, err := core.NewProfile(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	owner.SetRuntimeJournal(file)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	oldA, oldB := runtimeClient(t, portA, a), runtimeClient(t, portB, b)
	runtimePing(t, oldA, "A:", true)
	runtimePing(t, oldB, "B:", true)
	nextA, nextB := build(portA, c), build(portB, c)
	result, err := control.ApplyEnrolledProfileBatch(context.Background(), owner, []control.EnrolledProfileTarget{
		{BindingID: enrolledA, Config: nextA, ExpectedHash: enrolledHash(t, configA)},
		{BindingID: enrolledB, Config: nextB, ExpectedHash: enrolledHash(t, configB)},
	})
	if err != nil || len(result) != 2 {
		t.Fatal(result, err)
	}
	runtimePing(t, oldA, "A:", false)
	runtimePing(t, oldB, "B:", false)
	runtimePing(t, runtimeClient(t, portA, a), "C:", true)
	runtimePing(t, runtimeClient(t, portB, b), "C:", true)

	rpc, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", apiPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.Close()
	inspect := func() map[string]interface{} {
		t.Helper()
		response := new(structpb.Struct)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := rpc.Invoke(ctx, control.ProfileControlInspectMethod, &structpb.Struct{}, response); err != nil {
			t.Fatal(err)
		}
		return response.AsMap()
	}
	apply := func(target *core.Config, hash string) (*structpb.Struct, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx, "if-match", hash)
		response := new(structpb.Struct)
		err := rpc.Invoke(ctx, control.ProfileControlApplyMethod, target, response, grpc.MaxCallSendMsgSize(core.RuntimeJournalLimit))
		return response, err
	}
	desiredBindings := []core.ProfileBinding{
		{BindingID: enrolledA, ProfileID: enrolledA, InboundID: enrolledA, Config: nextA},
		{BindingID: enrolledB, ProfileID: enrolledB, InboundID: enrolledB, Config: nextB},
	}
	sourceRoot, err := core.WithProfileBindings(root, desiredBindings)
	if err != nil {
		t.Fatal(err)
	}
	sourceHash, err := core.ProfilePlanDigest(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	delivery := core.ProfileDelivery{Epoch: "1", Owner: enrolledA, Revision: "1", Enabled: true, InputsHash: strings.Repeat("a", 64), PlanHash: sourceHash}
	desired, err := core.WithProfileBindings(root, desiredBindings)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = core.WithProfileDelivery(desired, delivery)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := apply(desired, inspect()["hash"].(string))
	if err != nil || ack.AsMap()["unchanged"] != false {
		t.Fatal("clock-only durable delivery failed", err)
	}
	// A later ordinary native controller update must write through the same
	// manifest, rather than resurrecting a stale independent graph checkpoint.
	if err := owner.ManagedProfile(enrolledA).RuntimeController().Update(context.Background(), func(config *core.Config) error {
		config.Outbound = proto.Clone(configA).(*core.Config).Outbound
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	ack, err = apply(desired, inspect()["hash"].(string))
	if err != nil || ack.AsMap()["unchanged"] != true {
		t.Fatal("identical delivery replay was not idempotent", err)
	}
	runtimePing(t, runtimeClient(t, portA, a), "A:", true)
	// The replay must not undo A's subsequent controller edit.
	nextBindings := append([]core.ProfileBinding(nil), desiredBindings...)
	nextBindings[0].Config = configA
	nextSourceRoot, err := core.WithProfileBindings(root, nextBindings)
	if err != nil {
		t.Fatal(err)
	}
	nextHash, err := core.ProfilePlanDigest(nextSourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	delivery2 := delivery
	delivery2.Epoch = "2"
	delivery2.Owner = enrolledB
	delivery2.Revision = "2"
	delivery2.InputsHash = strings.Repeat("b", 64)
	delivery2.PlanHash = nextHash
	desired2, err := core.WithProfileBindings(root, nextBindings)
	if err != nil {
		t.Fatal(err)
	}
	desired2, err = core.WithProfileDelivery(desired2, delivery2)
	if err != nil {
		t.Fatal(err)
	}
	staleHash := inspect()["hash"].(string)
	if _, err := apply(desired2, staleHash); err != nil {
		t.Fatal(err)
	}
	if _, err := apply(desired, inspect()["hash"].(string)); status.Code(err) != codes.Aborted {
		t.Fatal("old persisted delivery epoch accepted", err)
	}
	if _, err := apply(desired2, staleHash); status.Code(err) != codes.Aborted {
		t.Fatal("stale manifest CAS accepted", err)
	}
	conflicting := delivery2
	conflicting.Owner = enrolledA
	conflict, err := core.WithProfileDelivery(desired2, conflicting)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apply(conflict, inspect()["hash"].(string)); status.Code(err) != codes.Aborted {
		t.Fatal("same epoch with another owner accepted", err)
	}

	reused := delivery2
	reused.PlanHash = delivery.PlanHash
	reusedRoot, err := core.WithProfileDelivery(desired, reused)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apply(reusedRoot, inspect()["hash"].(string)); status.Code(err) != codes.Aborted {
		t.Fatal("same delivery lease accepted a different source plan", err)
	}
	badDigest := delivery2
	badDigest.PlanHash = "0" + nextHash[1:]
	if badDigest.PlanHash == nextHash {
		badDigest.PlanHash = "1" + nextHash[1:]
	}
	badRoot, err := core.WithProfileDelivery(desired2, badDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apply(badRoot, inspect()["hash"].(string)); status.Code(err) != codes.InvalidArgument {
		t.Fatal("forged native source digest accepted", err)
	}
	capabilities := inspect()
	if capabilities["dynamicMembership"] != false || capabilities["nodeDelivery"] != false {
		t.Fatal("partial native profile API advertised complete node integration")
	}
	saved, err := core.LoadRuntimeJournal(file)
	if err != nil {
		t.Fatal(err)
	}
	bindings, _, err := core.ConfigProfileBindings(saved)
	if err != nil || len(bindings) != 2 || !proto.Equal(bindings[0].Config, configA) || !proto.Equal(bindings[1].Config, nextB) {
		t.Fatal("manifest lost a batch or subsequent API mutation", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "-test.run=^TestEnrolledProfileManifestRestartProcess$", "-test.timeout=25s")
	child.Env = append(os.Environ(), "REMNACUST_PROFILE_MANIFEST_RESTART_TEST="+file)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	child.Stderr = &stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	var waitOnce sync.Once
	var waitError error
	stop := func() { waitOnce.Do(func() { stdin.Close(); waitError = child.Wait() }) }
	t.Cleanup(stop)
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
			t.Fatalf("restart failed: %v %s", waitError, stderr.String())
		}
	case <-ctx.Done():
		t.Fatal("restart readiness timed out")
	}
	runtimePing(t, runtimeClient(t, portA, a), "A:", true)
	runtimePing(t, runtimeClient(t, portB, b), "C:", true)

	restoredRPC, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", apiPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer restoredRPC.Close()
	restored := new(structpb.Struct)
	restoredCtx, restoredCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer restoredCancel()
	if err := restoredRPC.Invoke(restoredCtx, control.ProfileControlInspectMethod, &structpb.Struct{}, restored); err != nil {
		t.Fatal(err)
	}
	restoredDelivery, ok := restored.AsMap()["delivery"].(map[string]interface{})
	if !ok || restoredDelivery["epoch"] != "2" || restoredDelivery["planHash"] != nextHash {
		t.Fatal("process restart lost delivery fence", restoredDelivery)
	}
	stop()
	if waitError != nil {
		t.Fatalf("restart process shutdown failed: %v %s", waitError, stderr.String())
	}
}

func TestEnrolledProfileManifestRestartProcess(t *testing.T) {
	file := os.Getenv("REMNACUST_PROFILE_MANIFEST_RESTART_TEST")
	if file == "" {
		t.Skip("spawned process fixture")
	}
	config, err := core.LoadRuntimeJournal(file)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := core.New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	owner.SetRuntimeJournal(file)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	fmt.Println("PROFILE_MANIFEST_READY")
	_, _ = io.Copy(io.Discard, os.Stdin)
}
