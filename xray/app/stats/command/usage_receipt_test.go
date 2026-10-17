package command

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/xtls/xray-core/app/stats"
	"github.com/xtls/xray-core/common/quotausage"
	"github.com/xtls/xray-core/core"
	feature_stats "github.com/xtls/xray-core/features/stats"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"
)

const receiptStream = "6bde8fbb-6d28-4d50-99a2-28074f3c235b"
const receiptOther = "ff29ee68-a625-4160-9573-cc17de317a5d"
const receiptCounter = "user>>>77>>>traffic>>>uplink"

func TestUsageReceiptPrivateDebtReplaysAndCannotBeResetSeparately(t *testing.T) {
	server, normal := receiptFixture(t)
	name, err := (quotausage.Counter{Account: receiptOther, Owner: "77", Direction: "downlink", Factor: 1500000000, Scale: 1000000000}).Name()
	if err != nil {
		t.Fatal(err)
	}
	debt, err := server.stats.RegisterCounter(name)
	if err != nil {
		t.Fatal(err)
	}
	normal.Add(17)
	debt.Add(9007199254740993)
	first := receiptSample(t, server, "1")
	values := first.AsMap()["stats"].([]any)
	if len(values) != 2 || values[0].(map[string]any)["name"] != name || values[0].(map[string]any)["value"] != "9007199254740993" || normal.Value() != 0 || debt.Value() != 0 {
		t.Fatal("private debt lost precision or was omitted", values)
	}
	debt.Add(7)
	for _, read := range []func() error{
		func() error {
			_, err := server.GetStats(context.Background(), &GetStatsRequest{Name: name, Reset_: true})
			return err
		},
		func() error {
			_, err := server.QueryStats(context.Background(), &QueryStatsRequest{Pattern: quotausage.Prefix, Reset_: true})
			return err
		},
	} {
		if err := read(); status.Code(err) != codes.FailedPrecondition || debt.Value() != 7 {
			t.Fatal("private debt destructively read", err)
		}
	}
	// Reopen the actual journal with a fresh statistics manager.
	restarted, _ := receiptFixture(t)
	restarted.usagePath = server.usagePath
	if !reflect.DeepEqual(first.AsMap(), receiptSample(t, restarted, "1").AsMap()) {
		t.Fatal("private debt did not survive journal reload")
	}
}

func TestUsageReceiptRejectsMalformedPrivateDebtBeforeReset(t *testing.T) {
	server, normal := receiptFixture(t)
	debt, _ := server.stats.RegisterCounter("qfree>>>invalid>>>77>>>1>>>100>>>traffic>>>uplink")
	normal.Add(19)
	debt.Add(23)
	if _, err := server.exchangeUsage(context.Background(), sampleRequest("1", core.ProcessID())); status.Code(err) != codes.ResourceExhausted || normal.Value() != 19 || debt.Value() != 23 {
		t.Fatal("malformed private debt was silently discarded", err)
	}
}

func TestUsageReceiptCrossLanguageDigest(t *testing.T) {
	frame := &usageFrame{Sequence: "1", CoreProcessID: receiptOther, CapturedAt: "1", Stats: []usageValue{{Name: receiptCounter, Value: "9007199254740993"}}}
	if usageDigest(frame) != "8a36f1c97332306024c3fbd25101e639271c84e0091abe904e143b5118f903eb" {
		t.Fatal("Node/core receipt contract changed")
	}
}

func TestUsageReceiptRejectsDisabledStatisticsManager(t *testing.T) {
	server := NewStatsServer(feature_stats.NoopManager{}).(*statsServer)
	server.usagePath = filepath.Join(t.TempDir(), "usage", "receipt.json")
	if _, err := server.exchangeUsage(context.Background(), sampleRequest("1", core.ProcessID())); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("disabled statistics must not prove a zero debt", err)
	}
	if _, err := os.Stat(server.usagePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsupported manager wrote a receipt", err)
	}
}

func receiptFixture(t *testing.T) (*statsServer, feature_stats.Counter) {
	t.Helper()
	manager, err := stats.NewManager(context.Background(), &stats.Config{})
	if err != nil {
		t.Fatal(err)
	}
	counter, err := manager.RegisterCounter(receiptCounter)
	if err != nil {
		t.Fatal(err)
	}
	server := NewStatsServer(manager).(*statsServer)
	server.usagePath = filepath.Join(t.TempDir(), "usage", "receipt.json")
	return server, counter
}

func sampleRequest(sequence string, process string) *structpb.Struct {
	request, _ := structpb.NewStruct(map[string]any{"action": "sample", "stream": receiptStream, "sequence": sequence, "coreProcessId": process})
	return request
}

func acknowledgeRequest(response *structpb.Struct) *structpb.Struct {
	fields := response.AsMap()
	request, _ := structpb.NewStruct(map[string]any{"action": "ack", "stream": fields["stream"], "sequence": fields["sequence"], "coreProcessId": fields["coreProcessId"], "digest": fields["digest"]})
	return request
}

func receiptSample(t *testing.T, server *statsServer, sequence string) *structpb.Struct {
	t.Helper()
	response, err := server.exchangeUsage(context.Background(), sampleRequest(sequence, core.ProcessID()))
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func receiptValue(t *testing.T, response *structpb.Struct) string {
	t.Helper()
	values := response.AsMap()["stats"].([]any)
	if len(values) != 1 {
		t.Fatalf("unexpected stats: %v", values)
	}
	return values[0].(map[string]any)["value"].(string)
}

func TestUsageReceiptReplayAndAcknowledgement(t *testing.T) {
	server, counter := receiptFixture(t)
	counter.Add(9007199254740993)
	first := receiptSample(t, server, "1")
	if receiptValue(t, first) != "9007199254740993" || counter.Value() != 0 {
		t.Fatal("counter lost precision or was not captured")
	}
	counter.Add(17)
	second := receiptSample(t, server, "1")
	if !reflect.DeepEqual(first.AsMap(), second.AsMap()) || counter.Value() != 17 {
		t.Fatal("retry consumed a second sample")
	}
	if _, err := server.exchangeUsage(context.Background(), sampleRequest("2", core.ProcessID())); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("unacknowledged frame was replaced", err)
	}
	ack := acknowledgeRequest(first)
	for range 2 {
		if _, err := server.exchangeUsage(context.Background(), ack); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := server.exchangeUsage(context.Background(), sampleRequest("1", core.ProcessID())); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("acknowledged sample was recaptured", err)
	}
	third := receiptSample(t, server, "2")
	if receiptValue(t, third) != "17" {
		t.Fatal("tail did not survive replay")
	}
	if _, err := server.exchangeUsage(context.Background(), ack); err != nil {
		t.Fatal("lost ACK retry must preserve next pending frame", err)
	}
	if !reflect.DeepEqual(third.AsMap(), receiptSample(t, server, "2").AsMap()) {
		t.Fatal("old ACK cleared a newer pending frame")
	}
}

func TestUsageReceiptCrashReplayBeforeSubtraction(t *testing.T) {
	server, counter := receiptFixture(t)
	counter.Add(91)
	server.usageSave = func(path string, state *usageJournal) error {
		if err := saveUsageJournal(path, state); err != nil {
			return err
		}
		return errors.New("simulated crash after rename before counter subtraction")
	}
	if _, err := server.exchangeUsage(context.Background(), sampleRequest("1", core.ProcessID())); status.Code(err) != codes.Unavailable || counter.Value() != 91 {
		t.Fatal("ambiguous commit reset live counters", err)
	}
	if _, err := server.exchangeUsage(context.Background(), sampleRequest("1", core.ProcessID())); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("poisoned writer continued", err)
	}
	restarted, fresh := receiptFixture(t)
	restarted.usagePath = server.usagePath
	fresh.Add(13)
	first := receiptSample(t, restarted, "1")
	if receiptValue(t, first) != "91" || fresh.Value() != 13 {
		t.Fatal("restart replay touched new process counters")
	}
}

func TestUsageReceiptPreservesConcurrentTrafficAndRegistration(t *testing.T) {
	server, counter := receiptFixture(t)
	counter.Add(41)
	server.usageSave = func(path string, state *usageJournal) error {
		counter.Add(19)
		other, err := server.stats.RegisterCounter("user>>>88>>>traffic>>>downlink")
		if err != nil {
			return err
		}
		other.Add(29)
		return saveUsageJournal(path, state)
	}
	first := receiptSample(t, server, "1")
	if receiptValue(t, first) != "41" || counter.Value() != 19 {
		t.Fatal("concurrent additions were reset")
	}
	if server.stats.GetCounter("user>>>88>>>traffic>>>downlink").Value() != 29 {
		t.Fatal("new registration was reset")
	}
}

func TestUsageReceiptWriteFailureDoesNotReset(t *testing.T) {
	server, counter := receiptFixture(t)
	counter.Add(31)
	server.usageSave = func(string, *usageJournal) error { return errors.New("disk full") }
	if _, err := server.exchangeUsage(context.Background(), sampleRequest("1", core.ProcessID())); status.Code(err) != codes.Unavailable || counter.Value() != 31 {
		t.Fatal("failed write lost usage", err)
	}
	if _, err := server.GetStats(context.Background(), &GetStatsRequest{Name: receiptCounter, Reset_: true}); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("legacy reset bypassed ambiguous write", err)
	}
}

func TestUsageReceiptOversizedFrameIsRejectedBeforeWritingOrResetting(t *testing.T) {
	server, counter := receiptFixture(t)
	counter.Add(31)
	for i := range 2000 {
		other, err := server.stats.RegisterCounter("user>>>" + strings.Repeat(">", 900) + strconv.Itoa(i) + ">>>traffic>>>uplink")
		if err != nil {
			t.Fatal(err)
		}
		other.Add(1)
	}
	if _, err := server.exchangeUsage(context.Background(), sampleRequest("1", core.ProcessID())); status.Code(err) != codes.ResourceExhausted {
		t.Fatal("oversized frame accepted", err)
	}
	if counter.Value() != 31 || server.usageErr != nil {
		t.Fatal("a frame size limit consumed traffic or poisoned the journal")
	}
	if _, err := os.Stat(server.usagePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("oversized frame was written", err)
	}
}

func TestUsageReceiptWrongCoreStreamSequenceAndDigest(t *testing.T) {
	server, counter := receiptFixture(t)
	counter.Add(57)
	for _, request := range []*structpb.Struct{sampleRequest("1", receiptOther), sampleRequest("2", core.ProcessID()), sampleRequest("01", core.ProcessID()), sampleRequest("9223372036854775808", core.ProcessID())} {
		if _, err := server.exchangeUsage(context.Background(), request); err == nil || counter.Value() != 57 {
			t.Fatal("invalid request reset counters", err)
		}
	}
	first := receiptSample(t, server, "1")
	wrong := acknowledgeRequest(first).AsMap()
	wrong["digest"] = strings.Repeat("0", 64)
	request, _ := structpb.NewStruct(wrong)
	if _, err := server.exchangeUsage(context.Background(), request); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("wrong digest acknowledged", err)
	}
	other := sampleRequest("1", core.ProcessID()).AsMap()
	other["stream"] = receiptOther
	request, _ = structpb.NewStruct(other)
	if _, err := server.exchangeUsage(context.Background(), request); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("stream binding changed", err)
	}
}

func TestUsageReceiptJournalLossCorruptionAndSymlink(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "symlink", "public"} {
		t.Run(mode, func(t *testing.T) {
			if runtime.GOOS == "windows" && (mode == "symlink" || mode == "public") {
				t.Skip("requires Unix file permissions")
			}
			server, counter := receiptFixture(t)
			counter.Add(11)
			receiptSample(t, server, "1")
			counter.Add(7)
			switch mode {
			case "missing":
				if err := os.Remove(server.usagePath); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(server.usagePath, []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(server.usagePath, server.usagePath+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(server.usagePath+".real", server.usagePath); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.Chmod(server.usagePath, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := server.exchangeUsage(context.Background(), sampleRequest("1", core.ProcessID())); status.Code(err) != codes.FailedPrecondition || counter.Value() != 7 {
				t.Fatal("untrusted journal restarted the stream", err)
			}
			if _, err := server.QueryStats(context.Background(), &QueryStatsRequest{Reset_: true}); status.Code(err) != codes.FailedPrecondition {
				t.Fatal("legacy reset bypassed missing/corrupt journal", err)
			}
		})
	}
}

func TestUsageReceiptLegacyAPIsAndShortNonUserName(t *testing.T) {
	server, counter := receiptFixture(t)
	other, _ := server.stats.RegisterCounter(">>>traffic>>>uplink")
	other.Add(9)
	if _, err := server.GetUsersStats(context.Background(), &GetUsersStatsRequest{IncludeTraffic: true}); err != nil {
		t.Fatal(err)
	}
	counter.Add(71)
	receiptSample(t, server, "1")
	counter.Add(5)
	for _, call := range []func() error{
		func() error {
			_, err := server.GetStats(context.Background(), &GetStatsRequest{Name: receiptCounter, Reset_: true})
			return err
		},
		func() error {
			_, err := server.QueryStats(context.Background(), &QueryStatsRequest{Pattern: "traffic", Reset_: true})
			return err
		},
		func() error {
			_, err := server.GetUsersStats(context.Background(), &GetUsersStatsRequest{IncludeTraffic: true, Reset_: true})
			return err
		},
	} {
		if err := call(); status.Code(err) != codes.FailedPrecondition || counter.Value() != 5 {
			t.Fatal("legacy reader consumed durable traffic", err)
		}
	}
	if _, err := server.GetStats(context.Background(), &GetStatsRequest{Name: receiptCounter}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.GetStats(context.Background(), &GetStatsRequest{Name: ">>>traffic>>>uplink", Reset_: true}); err != nil {
		t.Fatal("unrelated counter reset blocked", err)
	}
}

func TestUsageReceiptConcurrentRetries(t *testing.T) {
	server, counter := receiptFixture(t)
	counter.Add(121)
	var wait sync.WaitGroup
	errors := make(chan error, 20)
	for range 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			response, err := server.exchangeUsage(context.Background(), sampleRequest("1", core.ProcessID()))
			if err == nil && response.AsMap()["stats"].([]any)[0].(map[string]any)["value"] != "121" {
				err = status.Error(codes.Internal, "different sample")
			}
			errors <- err
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if counter.Value() != 0 {
		t.Fatal("sample was not reset exactly once")
	}
}

func TestUsageReceiptRecoveryFromPreviousCoreAndSequenceWatermark(t *testing.T) {
	server, counter := receiptFixture(t)
	counter.Add(73)
	receiptSample(t, server, "1")
	state, err := readUsageJournal(server.usagePath)
	if err != nil {
		t.Fatal(err)
	}
	state.Pending.CoreProcessID = receiptOther
	if err := saveUsageJournal(server.usagePath, state); err != nil {
		t.Fatal(err)
	}
	old, err := server.exchangeUsage(context.Background(), sampleRequest("1", receiptOther))
	if err != nil || receiptValue(t, old) != "73" {
		t.Fatal("previous process frame was not replayable", err)
	}
	if _, err := server.exchangeUsage(context.Background(), acknowledgeRequest(old)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(server.usagePath); err != nil {
		t.Fatal(err)
	}
	restarted, _ := receiptFixture(t)
	restarted.usagePath = server.usagePath
	if _, err := restarted.exchangeUsage(context.Background(), sampleRequest("2", core.ProcessID())); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("missing state silently initialized a later sequence", err)
	}
}

func TestUsageReceiptRPC(t *testing.T) {
	server, counter := receiptFixture(t)
	counter.Add(83)
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	registerUsageReceipts(grpcServer, server)
	go grpcServer.Serve(listener)
	t.Cleanup(func() {
		grpcServer.GracefulStop()
		_ = listener.Close()
	})
	connection, err := grpc.NewClient("passthrough:///usage", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	response := new(structpb.Struct)
	if err := connection.Invoke(context.Background(), UsageReceiptMethod, sampleRequest("1", core.ProcessID()), response); err != nil {
		t.Fatal(err)
	}
	if receiptValue(t, response) != strconv.Itoa(83) {
		t.Fatal("RPC changed decimal usage")
	}
}

func TestUsageReceiptRejectsLinkedAncestorsBeforeCreatingDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix symlinks")
	}
	server, counter := receiptFixture(t)
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	server.usagePath = filepath.Join(root, "linked", "not-created", "receipt.json")
	counter.Add(17)
	if _, err := server.exchangeUsage(context.Background(), sampleRequest("1", core.ProcessID())); err == nil {
		t.Fatal("linked state ancestor accepted")
	}
	if counter.Value() != 17 {
		t.Fatal("untrusted path reset counters")
	}
	if _, err := os.Stat(filepath.Join(outside, "not-created")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected journal created an outside directory", err)
	}
}

func TestUsageReceiptRealProcessRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "process-receipt.json")
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(phase string, exitCode int) map[string]any {
		t.Helper()
		command := exec.Command(os.Args[0], "-test.run=^TestUsageReceiptProcessHelper$")
		command.Env = append(os.Environ(), "REMNACUST_USAGE_PROCESS_TEST_FILE="+path, "REMNACUST_USAGE_PROCESS_TEST_PHASE="+phase)
		output, err := command.Output()
		if exitCode != 0 {
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != exitCode {
				t.Fatalf("unexpected helper termination: %v %s", err, output)
			}
		} else if err != nil {
			t.Fatalf("helper failed: %v %s", err, output)
		}
		observation, readErr := os.ReadFile(filepath.Join(filepath.Dir(path), "observation-"+phase+".json"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		var value map[string]any
		if err := json.Unmarshal(observation, &value); err != nil {
			t.Fatalf("invalid helper output: %v %s", err, observation)
		}
		return value
	}
	crashed := run("crash", 79)
	recovered := run("recover", 0)
	if crashed["core"] == recovered["core"] {
		t.Fatal("restarted process reused its core identity")
	}
	if recovered["recoveredCore"] != crashed["core"] || recovered["recoveredValue"] != "91" || recovered["nextValue"] != "13" {
		t.Fatalf("process restart did not preserve the exact pending frame: %v", recovered)
	}
}

func TestUsageReceiptProcessHelper(t *testing.T) {
	file := os.Getenv("REMNACUST_USAGE_PROCESS_TEST_FILE")
	if file == "" {
		t.Skip("subprocess helper")
	}
	if !filepath.IsAbs(file) || filepath.Base(file) != "process-receipt.json" {
		t.Fatal("refuse non-fixture journal path")
	}
	manager, err := stats.NewManager(context.Background(), &stats.Config{})
	if err != nil {
		t.Fatal(err)
	}
	counter, err := manager.RegisterCounter(receiptCounter)
	if err != nil {
		t.Fatal(err)
	}
	server := NewStatsServer(manager).(*statsServer)
	server.usagePath = file
	// The helper intentionally terminates without deferred cleanup to model a
	// process death after the durable commit, before any RAM subtraction.
	write := func(value any) {
		output, err := json.Marshal(value)
		if err != nil {
			os.Exit(81)
		}
		if err := os.WriteFile(filepath.Join(filepath.Dir(file), "observation-"+os.Getenv("REMNACUST_USAGE_PROCESS_TEST_PHASE")+".json"), output, 0600); err != nil {
			os.Exit(82)
		}
	}
	if os.Getenv("REMNACUST_USAGE_PROCESS_TEST_PHASE") == "crash" {
		counter.Add(91)
		server.usageSave = func(path string, state *usageJournal) error {
			if err := saveUsageJournal(path, state); err != nil {
				return err
			}
			write(map[string]any{"core": core.ProcessID()})
			os.Exit(79)
			return nil
		}
		receiptSample(t, server, "1")
		t.Fatal("crash fixture returned")
	}
	counter.Add(13)
	state, err := readUsageJournal(file)
	if err != nil || state.Pending == nil {
		t.Fatalf("pending frame missing: %v", err)
	}
	old, err := server.exchangeUsage(context.Background(), sampleRequest("1", state.Pending.CoreProcessID))
	if err != nil {
		t.Fatal(err)
	}
	if counter.Value() != 13 {
		t.Fatal("replay touched the new process counters")
	}
	if _, err := server.exchangeUsage(context.Background(), acknowledgeRequest(old)); err != nil {
		t.Fatal(err)
	}
	next := receiptSample(t, server, "2")
	write(map[string]any{"core": core.ProcessID(), "recoveredCore": state.Pending.CoreProcessID, "recoveredValue": receiptValue(t, old), "nextValue": receiptValue(t, next)})
	os.Exit(0)
}
