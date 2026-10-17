package api

import (
	"encoding/json"
	"fmt"

	statsService "github.com/xtls/xray-core/app/stats/command"
	"github.com/xtls/xray-core/main/commands/base"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

var cmdUsageReceipt = &base.Command{
	CustomFlags: true,
	UsageLine:   "{{.Exec}} api usage-receipt [--server=unix:/path/to/private.sock] [--action=sample] [--stream=UUID] [--sequence=1] [--core=UUID] [--digest=SHA256]",
	Short:       "Read or acknowledge a durable node usage frame",
	Long:        "Read one replayable traffic frame, or acknowledge it only after committing all billing effects. Decimal counters and sequences remain strings. This command does not reset counters on a failed journal commit.",
	Run:         executeUsageReceipt,
}

func executeUsageReceipt(cmd *base.Command, args []string) {
	setSharedFlags(cmd)
	action := cmd.Flag.String("action", "sample", "")
	stream := cmd.Flag.String("stream", "", "")
	sequence := cmd.Flag.String("sequence", "", "")
	process := cmd.Flag.String("core", "", "")
	digest := cmd.Flag.String("digest", "", "")
	cmd.Flag.Parse(args)
	fields := map[string]any{"action": *action, "stream": *stream, "sequence": *sequence, "coreProcessId": *process}
	if *action == "ack" {
		fields["digest"] = *digest
	}
	request, err := structpb.NewStruct(fields)
	if err != nil {
		base.Fatalf("invalid usage receipt request")
	}
	conn, ctx, close := dialAPIServer()
	defer close()
	response := new(structpb.Struct)
	if err := conn.Invoke(ctx, statsService.UsageReceiptMethod, request, response, grpc.MaxCallRecvMsgSize(16*1024*1024)); err != nil {
		base.Fatalf("usage receipt exchange failed: %v", err)
	}
	output, err := json.Marshal(response.AsMap())
	if err != nil {
		base.Fatalf("invalid usage receipt response")
	}
	fmt.Println(string(output))
}
