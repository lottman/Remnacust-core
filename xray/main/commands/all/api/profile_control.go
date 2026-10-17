//go:build !wasm

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/main/commands/base"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func profileCheckpointHash(config *core.Config) string {
	data, _ := proto.MarshalOptions{Deterministic: true}.Marshal(config)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func prepareProfileCheckpoint(output string, data []byte) (map[string]any, error) {
	previous, err := core.LoadRuntimeJournal(output)
	if err != nil && !os.IsNotExist(err) {
		return nil, errors.New("existing profile checkpoint needs recovery")
	}
	required := false
	if previous != nil {
		required, err = core.ConfigRequiresUserQuota(previous)
		if err != nil {
			return nil, err
		}
	}
	target, err := compileProfileInput(data, required)
	if err != nil {
		return nil, err
	}
	if previous != nil {
		oldBindings, _, err := core.ConfigProfileBindings(previous)
		if err != nil {
			return nil, err
		}
		bindings, _, err := core.ConfigProfileBindings(target)
		if err != nil {
			return nil, err
		}
		requiredByBinding := make(map[string]bool, len(oldBindings))
		for _, binding := range oldBindings {
			required, err := core.ConfigRequiresUserQuota(binding.Config)
			if err != nil {
				return nil, err
			}
			requiredByBinding[binding.BindingID] = required
		}
		for _, binding := range bindings {
			if requiredByBinding[binding.BindingID] {
				if err := core.RequireUserQuotaConfig(binding.Config); err != nil {
					return nil, err
				}
			}
		}
		target, err = core.WithProfileBindings(target, bindings)
		if err != nil {
			return nil, err
		}
	}
	delivery, err := core.ConfigProfileDelivery(target)
	if err != nil {
		return nil, err
	}
	var oldDelivery *core.ProfileDelivery
	if previous != nil {
		oldDelivery, err = core.ConfigProfileDelivery(previous)
		if err != nil {
			return nil, err
		}
	}
	if err := core.CheckProfileDeliveryAdvance(oldDelivery, delivery); err != nil {
		return nil, err
	}
	unchanged := oldDelivery != nil && delivery != nil && *oldDelivery == *delivery
	if unchanged {
		target = previous
	} else if err := core.SaveRuntimeJournal(output, target); err != nil {
		return nil, err
	}
	bindings, _, err := core.ConfigProfileBindings(target)
	if err != nil {
		return nil, err
	}
	return map[string]any{"hash": profileCheckpointHash(target), "delivery": delivery.Fields(), "graphCount": len(bindings), "unchanged": unchanged}, nil
}

func profileRPCInspect(ctx context.Context, connection *grpc.ClientConn) (*structpb.Struct, error) {
	response := new(structpb.Struct)
	if err := connection.Invoke(ctx, command.ProfileControlInspectMethod, &structpb.Struct{}, response, grpc.MaxCallRecvMsgSize(512*1024)); err != nil {
		return nil, err
	}
	return response, nil
}

func profileRPCApply(ctx context.Context, connection *grpc.ClientConn, target *core.Config) (map[string]any, error) {
	current, err := profileRPCInspect(ctx, connection)
	if err != nil {
		return nil, err
	}
	hash, ok := current.AsMap()["hash"].(string)
	if !ok || len(hash) != 64 {
		return nil, errors.New("invalid owning profile revision")
	}
	response := new(structpb.Struct)
	ctx = metadata.AppendToOutgoingContext(ctx, "if-match", hash)
	if err := connection.Invoke(ctx, command.ProfileControlApplyMethod, target, response, grpc.MaxCallSendMsgSize(core.RuntimeJournalLimit), grpc.MaxCallRecvMsgSize(512*1024)); err != nil {
		return nil, err
	}
	fields := response.AsMap()
	graphs, ok := fields["graphs"].([]interface{})
	if !ok {
		return nil, errors.New("invalid profile acknowledgement")
	}
	return map[string]any{"hash": fields["hash"], "delivery": fields["delivery"], "coreProcessId": fields["coreProcessId"], "unchanged": fields["unchanged"], "graphCount": len(graphs)}, nil
}

func printProfileResult(value map[string]any) {
	output, err := json.Marshal(value)
	if err != nil {
		base.Fatalf("invalid profile control response")
	}
	fmt.Println(string(output))
}

var cmdProfileInspect = &base.Command{
	CustomFlags: true, UsageLine: "{{.Exec}} api profile-inspect [--server=unix:/private.sock]", Short: "Inspect independent profiles on the private control transport",
	Run: func(cmd *base.Command, args []string) {
		setSharedFlags(cmd)
		if err := cmd.Flag.Parse(args); err != nil {
			base.Fatalf("invalid profile inspection flags")
		}
		if len(cmd.Flag.Args()) != 0 {
			base.Fatalf("profile-inspect does not accept positional arguments")
		}
		connection, ctx, close := dialAPIServer()
		defer close()
		response, err := profileRPCInspect(ctx, connection)
		if err != nil {
			base.Fatalf("profile inspection failed: %v", err)
		}
		printProfileResult(response.AsMap())
	},
}

var cmdProfileApply = &base.Command{
	CustomFlags: true, UsageLine: "{{.Exec}} api profile-apply [--server=unix:/private.sock] profile-input.json", Short: "Apply a versioned compiled profile delivery without restarting the core",
	Run: func(cmd *base.Command, args []string) {
		setSharedFlags(cmd)
		if err := cmd.Flag.Parse(args); err != nil {
			base.Fatalf("invalid profile application flags")
		}
		if len(cmd.Flag.Args()) != 1 {
			base.Fatalf("one local profile input is required")
		}
		data, err := readProfileInput(cmd.Flag.Args()[0])
		if err != nil {
			base.Fatalf("%v", err)
		}
		target, err := compileProfileInput(data, false)
		if err != nil {
			base.Fatalf("profile compilation failed: %v", err)
		}
		connection, ctx, close := dialAPIServer()
		defer close()
		response, err := profileRPCApply(ctx, connection, target)
		if err != nil {
			base.Fatalf("profile application failed: %v", err)
		}
		printProfileResult(response)
	},
}

var cmdCacheProfiles = &base.Command{
	CustomFlags: true, UsageLine: "{{.Exec}} api cache-profiles [--output=/private/checkpoint.pb] profile-input.json", Short: "Prepare a complete versioned profile checkpoint for an explicit restart",
	Run: func(cmd *base.Command, args []string) {
		output := cmd.Flag.String("output", "", "Private native checkpoint")
		if err := cmd.Flag.Parse(args); err != nil {
			base.Fatalf("invalid profile cache flags")
		}
		if *output == "" || len(cmd.Flag.Args()) != 1 {
			base.Fatalf("one local profile input and output checkpoint are required")
		}
		data, err := readProfileInput(cmd.Flag.Args()[0])
		if err != nil {
			base.Fatalf("%v", err)
		}
		response, err := prepareProfileCheckpoint(*output, data)
		if err != nil {
			base.Fatalf("profile checkpoint preparation failed: %v", err)
		}
		printProfileResult(response)
	},
}
