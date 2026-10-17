//go:build !wasm

package api

import (
	"encoding/json"
	"fmt"

	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/main/commands/base"
)

func inspectCheckpoint(path string) (bool, error) {
	config, err := core.LoadRuntimeJournal(path)
	if err != nil {
		return false, err
	}
	return core.ConfigRequiresUserQuota(config)
}

var cmdInspectCheckpoint = &base.Command{
	CustomFlags: true,
	UsageLine:   "{{.Exec}} api inspect-checkpoint [--input=/private/checkpoint.pb]",
	Short:       "Verify a private restart checkpoint without opening listeners",
	Long:        "Validate the private file, checksum, compiled configuration and mandatory quota metadata without changing disk or contacting the running core. Only the requirement flag is printed.",
	Run: func(cmd *base.Command, args []string) {
		input := cmd.Flag.String("input", "", "Private restart checkpoint")
		cmd.Flag.Parse(args)
		if *input == "" || len(cmd.Flag.Args()) != 0 {
			base.Fatalf("one input checkpoint is required")
		}
		required, err := inspectCheckpoint(*input)
		if err != nil {
			base.Fatalf("private checkpoint verification failed; recover its saved state before restarting")
		}
		output, _ := json.Marshal(map[string]any{"userQuotaRequired": required})
		fmt.Println(string(output))
	},
}
