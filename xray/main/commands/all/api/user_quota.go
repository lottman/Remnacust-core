//go:build !wasm

package api

import (
	"encoding/json"
	"fmt"

	"github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/main/commands/base"
	"google.golang.org/protobuf/types/known/structpb"
)

var cmdRequireUserQuota = &base.Command{
	CustomFlags: true,
	UsageLine:   "{{.Exec}} api require-user-quota [--server=unix:/private.sock] [--core=UUID]",
	Short:       "Persist the applied server quota requirement with the current native credentials",
	Long:        "Require the saved user quota policy on restart. This cannot disable an existing requirement or replace live users with a stale node-side configuration.",
	Run: func(cmd *base.Command, args []string) {
		setSharedFlags(cmd)
		process := cmd.Flag.String("core", "", "")
		cmd.Flag.Parse(args)
		connection, ctx, close := dialAPIServer()
		defer close()
		request, _ := structpb.NewStruct(map[string]any{"coreProcessId": *process})
		response := new(structpb.Struct)
		if err := connection.Invoke(ctx, command.RuntimeRequireUserQuotaMethod, request, response); err != nil {
			base.Fatalf("user quota requirement commit failed: %v", err)
		}
		output, err := json.Marshal(response.AsMap())
		if err != nil {
			base.Fatalf("invalid user quota acknowledgement")
		}
		fmt.Println(string(output))
	},
}
