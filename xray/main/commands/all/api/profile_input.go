//go:build !wasm

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/xtls/xray-core/core"
	configserial "github.com/xtls/xray-core/infra/conf/serial"
)

const profileInputVersion = "remnacust-profile-input-v1"

type profileInput struct {
	Version  string          `json:"version"`
	Root     json.RawMessage `json:"root"`
	Delivery struct {
		Epoch      string `json:"epoch"`
		Owner      string `json:"owner"`
		Revision   string `json:"revision"`
		InputsHash string `json:"inputsHash"`
		Enabled    *bool  `json:"enabled"`
	} `json:"delivery"`
	Graphs []struct {
		BindingID string          `json:"bindingUuid"`
		ProfileID string          `json:"configProfileUuid"`
		InboundID string          `json:"configProfileInboundUuid"`
		Config    json.RawMessage `json:"config"`
	} `json:"graphs"`
}

// Reject duplicate members before decoding structs or the Xray JSON builder.
// encoding/json merges duplicate map members, including process environment.
func profileJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("profile JSON nesting exceeds the limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return errors.New("invalid profile JSON delimiter")
	}
	seen := make(map[string]bool)
	for decoder.More() {
		if delimiter == '{' {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate or invalid profile JSON field")
			}
			seen[name] = true
		}
		if err := profileJSONValue(decoder, depth+1); err != nil {
			return err
		}
	}
	end, err := decoder.Token()
	if err != nil {
		return err
	}
	if (delimiter == '{' && end != json.Delim('}')) || (delimiter == '[' && end != json.Delim(']')) {
		return errors.New("invalid profile JSON ending")
	}
	return nil
}

func profileNoEnvironment(raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return errors.New("profile configuration must be a JSON object")
	}
	for key, value := range fields {
		if !strings.EqualFold(key, "env") {
			continue
		}
		var environment map[string]string
		if err := json.Unmarshal(value, &environment); err != nil || environment == nil || len(environment) != 0 {
			return errors.New("process environment is not profile-local")
		}
	}
	return nil
}

func compileProfileInput(data []byte, quotaRequired bool) (*core.Config, error) {
	if len(data) > core.RuntimeJournalLimit {
		return nil, errors.New("profile input exceeds its limit")
	}
	tokens := json.NewDecoder(bytes.NewReader(data))
	tokens.UseNumber()
	if err := profileJSONValue(tokens, 0); err != nil {
		return nil, err
	}
	if _, err := tokens.Token(); err != io.EOF {
		return nil, errors.New("profile input has trailing JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var input profileInput
	if err := decoder.Decode(&input); err != nil {
		return nil, errors.New("invalid profile input envelope")
	}
	if input.Version != profileInputVersion || input.Delivery.Enabled == nil || input.Graphs == nil || len(input.Graphs) > 512 {
		return nil, errors.New("unsupported or incomplete profile input")
	}
	clock := core.ProfileDelivery{Epoch: input.Delivery.Epoch, Owner: input.Delivery.Owner, Revision: input.Delivery.Revision, Enabled: *input.Delivery.Enabled, InputsHash: input.Delivery.InputsHash, PlanHash: strings.Repeat("0", 64)}
	if err := core.CheckProfileDeliveryAdvance(nil, &clock); err != nil {
		return nil, err
	}
	if !clock.Enabled && len(input.Graphs) != 0 {
		return nil, errors.New("a disabled profile plan cannot expose graphs")
	}
	// Validate every environment and UUID before running ANY JSON Build method.
	if len(input.Root) > 4*1024*1024 {
		return nil, errors.New("root configuration exceeds its limit")
	}
	if err := profileNoEnvironment(input.Root); err != nil {
		return nil, err
	}
	bindings := make([]core.ProfileBinding, len(input.Graphs))
	for i, graph := range input.Graphs {
		if len(graph.Config) > 4*1024*1024 {
			return nil, errors.New("profile configuration exceeds its limit")
		}
		if err := profileNoEnvironment(graph.Config); err != nil {
			return nil, err
		}
		bindings[i] = core.ProfileBinding{BindingID: graph.BindingID, ProfileID: graph.ProfileID, InboundID: graph.InboundID, Config: &core.Config{}}
	}
	if _, err := core.WithProfileBindings(&core.Config{}, bindings); err != nil {
		return nil, err
	}
	build := func(raw json.RawMessage) (*core.Config, error) {
		config, err := configserial.DecodeJSONConfig(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		return config.Build()
	}
	root, err := build(input.Root)
	if err != nil {
		return nil, err
	}
	if quotaRequired {
		if err := core.RequireUserQuotaConfig(root); err != nil {
			return nil, err
		}
	}
	for i, graph := range input.Graphs {
		config, err := build(graph.Config)
		if err != nil {
			return nil, err
		}
		bindings[i].Config = config
	}
	root, err = core.WithProfileBindings(root, bindings)
	if err != nil {
		return nil, err
	}
	clock.PlanHash, err = core.ProfilePlanDigest(root)
	if err != nil {
		return nil, err
	}
	return core.WithProfileDelivery(root, clock)
}

func readProfileInput(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open local profile input")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, core.RuntimeJournalLimit+1))
	if err != nil || len(data) > core.RuntimeJournalLimit {
		return nil, errors.New("profile input exceeds its limit")
	}
	return data, nil
}
