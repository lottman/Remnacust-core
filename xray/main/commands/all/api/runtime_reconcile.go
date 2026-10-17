//go:build !wasm

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/core"
	configserial "github.com/xtls/xray-core/infra/conf/serial"
	"github.com/xtls/xray-core/main/commands/base"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"
)

var cmdReconcileRuntime = &base.Command{
	CustomFlags: true,
	UsageLine:   "{{.Exec}} api reconcile [--server=unix:/path/to/private.sock] configuration.json",
	Short:       "Apply native ports and routing without restarting the core",
	Long:        "Apply a local configuration with optimistic revision checking. Incompatible listener or global settings require a new port or an explicit maintenance restart.",
	Run:         executeReconcileRuntime,
}

func executeReconcileRuntime(cmd *base.Command, args []string) {
	setSharedFlags(cmd)
	cmd.Flag.Parse(args)
	files := cmd.Flag.Args()
	if len(files) != 1 {
		base.Fatalf("one local configuration file is required")
	}
	file, err := os.Open(files[0])
	if err != nil {
		base.Fatalf("cannot open local configuration")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4*1024*1024+1))
	if err != nil || len(data) > 4*1024*1024 {
		base.Fatalf("local configuration exceeds the control request limit")
	}
	config, err := configserial.DecodeJSONConfig(bytes.NewReader(data))
	if err != nil {
		base.Fatalf("invalid configuration: %v", err)
	}
	built, err := config.Build()
	if err != nil {
		base.Fatalf("configuration validation failed: %v", err)
	}
	conn, ctx, close := dialAPIServer()
	defer close()
	current := new(structpb.Struct)
	if err := conn.Invoke(ctx, command.RuntimeInspectMethod, &structpb.Struct{}, current); err != nil {
		base.Fatalf("runtime inspection failed: %v", err)
	}
	hash, ok := current.AsMap()["hash"].(string)
	if !ok || len(hash) != 64 {
		base.Fatalf("invalid runtime revision")
	}
	ctx = metadata.AppendToOutgoingContext(ctx, "if-match", hash)
	response := new(structpb.Struct)
	if err := conn.Invoke(ctx, command.RuntimeApplyMethod, built, response, grpc.MaxCallSendMsgSize(core.RuntimeJournalLimit)); err != nil {
		base.Fatalf("runtime apply failed: %v", err)
	}
	output, err := json.Marshal(response.AsMap())
	if err != nil {
		base.Fatalf("invalid runtime acknowledgement")
	}
	fmt.Println(string(output))
}

func runtimeControlCommands() []*base.Command {
	return []*base.Command{cmdReconcileRuntime, cmdCacheRuntime, cmdRequireUserQuota, cmdInspectCheckpoint, cmdProfileInspect, cmdProfileApply, cmdCacheProfiles}
}
