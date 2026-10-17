//go:build !wasm

package api

import (
	"bytes"
	"errors"
	"io"
	"os"

	"github.com/xtls/xray-core/core"
	configserial "github.com/xtls/xray-core/infra/conf/serial"
	"github.com/xtls/xray-core/main/commands/base"
)

var cmdCacheRuntime = &base.Command{
	CustomFlags: true,
	UsageLine:   "{{.Exec}} api cache-runtime [--output=/private/checkpoint.pb] configuration.json",
	Short:       "Prepare a verified local checkpoint for an explicit maintenance restart",
	Long:        "Compile a local configuration and atomically write a private checkpoint. This command does not contact a running core or change live sessions.",
	Run: func(cmd *base.Command, args []string) {
		output := cmd.Flag.String("output", "", "Private output checkpoint")
		cmd.Flag.Parse(args)
		if *output == "" || len(cmd.Flag.Args()) != 1 {
			base.Fatalf("one local configuration and output checkpoint are required")
		}
		file, err := os.Open(cmd.Flag.Args()[0])
		if err != nil {
			base.Fatalf("cannot open local configuration")
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 4*1024*1024+1))
		if err != nil || len(data) > 4*1024*1024 {
			base.Fatalf("local configuration exceeds the limit")
		}
		config, err := configserial.DecodeJSONConfig(bytes.NewReader(data))
		if err != nil {
			base.Fatalf("configuration decode failed: %v", err)
		}
		built, err := config.Build()
		if err != nil {
			base.Fatalf("configuration validation failed: %v", err)
		}
		previous, previousErr := core.LoadRuntimeJournal(*output)
		if previousErr == nil {
			if err := validateLegacyRuntimeCache(previous); err != nil {
				base.Fatalf("%v", err)
			}
			required, err := core.ConfigRequiresUserQuota(previous)
			if err != nil {
				base.Fatalf("private checkpoint requirements cannot be recovered")
			}
			if required {
				if err := core.RequireUserQuotaConfig(built); err != nil {
					base.Fatalf("cannot preserve private quota requirements")
				}
			}
		} else if !os.IsNotExist(previousErr) {
			base.Fatalf("existing private checkpoint requires recovery before replacement")
		}
		if err := core.SaveRuntimeJournal(*output, built); err != nil {
			base.Fatalf("private checkpoint write failed: %v", err)
		}
	},
}

func validateLegacyRuntimeCache(previous *core.Config) error {
	_, enrolled, err := core.ConfigProfileBindings(previous)
	if err != nil {
		return err
	}
	if enrolled {
		return errors.New("an independent profile checkpoint requires versioned profile-plan replacement; legacy cache-runtime cannot erase it")
	}
	return nil
}
