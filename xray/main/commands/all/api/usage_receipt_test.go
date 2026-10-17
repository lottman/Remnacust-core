package api

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	statsService "github.com/xtls/xray-core/app/stats/command"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

type usageCLIControl interface {
	exchange(context.Context, *structpb.Struct) (*structpb.Struct, error)
}
type usageCLIFixture struct{ requests chan map[string]any }

func (s *usageCLIFixture) exchange(_ context.Context, request *structpb.Struct) (*structpb.Struct, error) {
	fields := request.AsMap()
	s.requests <- fields
	if fields["action"] == "ack" {
		return structpb.NewStruct(map[string]any{"stream": fields["stream"], "sequence": fields["sequence"], "acknowledged": true})
	}
	return structpb.NewStruct(map[string]any{"stream": fields["stream"], "sequence": fields["sequence"], "coreProcessId": fields["coreProcessId"], "capturedAt": "1", "capability": statsService.UsageReceiptServiceName,
		"digest": "8a36f1c97332306024c3fbd25101e639271c84e0091abe904e143b5118f903eb", "stats": []any{map[string]any{"name": "user>>>77>>>traffic>>>uplink", "value": "9007199254740993"}}})
}

func TestUsageReceiptCLIProtocol(t *testing.T) {
	var listener net.Listener
	var err error
	var address string
	if runtime.GOOS == "windows" {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			address = listener.Addr().String()
		}
	} else {
		socket := filepath.Join(t.TempDir(), "control.sock")
		listener, err = net.Listen("unix", socket)
		address = "unix:" + socket
	}
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	fixture := &usageCLIFixture{requests: make(chan map[string]any, 2)}
	server.RegisterService(&grpc.ServiceDesc{ServiceName: statsService.UsageReceiptServiceName, HandlerType: (*usageCLIControl)(nil), Methods: []grpc.MethodDesc{{MethodName: "Exchange", Handler: func(s any, ctx context.Context, decode func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
		request := new(structpb.Struct)
		if err := decode(request); err != nil {
			return nil, err
		}
		return s.(usageCLIControl).exchange(ctx, request)
	}}}}, fixture)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	for _, action := range []string{"sample", "ack"} {
		command := exec.Command(os.Args[0], "-test.run=^TestUsageReceiptCLIHelper$")
		command.Env = append(os.Environ(), "REMNACUST_USAGE_CLI_TEST_SERVER="+address, "REMNACUST_USAGE_CLI_TEST_ACTION="+action)
		output, err := command.Output()
		if err != nil {
			t.Fatalf("CLI failed: %v %s", err, output)
		}
		var response map[string]any
		if err := json.Unmarshal(output, &response); err != nil {
			t.Fatalf("CLI did not return clean JSON: %v %s", err, output)
		}
		request := <-fixture.requests
		if request["action"] != action || request["sequence"] != "1" || request["stream"] != "6bde8fbb-6d28-4d50-99a2-28074f3c235b" {
			t.Fatalf("CLI request changed: %v", request)
		}
		if action == "sample" {
			if len(request) != 4 || response["stats"].([]any)[0].(map[string]any)["value"] != "9007199254740993" {
				t.Fatal("CLI lost decimal precision or added reset fields")
			}
		} else if len(request) != 5 || request["digest"] != "8a36f1c97332306024c3fbd25101e639271c84e0091abe904e143b5118f903eb" || response["acknowledged"] != true {
			t.Fatal("CLI changed the acknowledgement")
		}
	}
}

func TestUsageReceiptCLIHelper(t *testing.T) {
	address := os.Getenv("REMNACUST_USAGE_CLI_TEST_SERVER")
	if address == "" {
		t.Skip("CLI subprocess helper")
	}
	if !strings.HasPrefix(address, "unix:") && !strings.HasPrefix(address, "127.0.0.1:") {
		t.Fatal("refuse a nonlocal control fixture")
	}
	action := os.Getenv("REMNACUST_USAGE_CLI_TEST_ACTION")
	args := []string{"--server=" + address, "--timeout=5", "--action=" + action, "--stream=6bde8fbb-6d28-4d50-99a2-28074f3c235b", "--sequence=1", "--core=ff29ee68-a625-4160-9573-cc17de317a5d"}
	if action == "ack" {
		args = append(args, "--digest=8a36f1c97332306024c3fbd25101e639271c84e0091abe904e143b5118f903eb")
	}
	executeUsageReceipt(cmdUsageReceipt, args)
	os.Exit(0)
}
