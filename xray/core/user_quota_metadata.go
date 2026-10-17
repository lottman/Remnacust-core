package core

import (
	"errors"

	"github.com/xtls/xray-core/common/serial"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// This server-only record is carried by the native checkpoint that also
// contains the credentials. It is not a client transport or subscription field.
const UserQuotaRequirementType = "remnacust.core.requirements.v1"

func ConfigRequiresUserQuota(config *Config) (bool, error) {
	if config == nil {
		return false, errors.New("configuration is missing")
	}
	found := false
	for _, app := range config.App {
		if app == nil || app.Type != UserQuotaRequirementType {
			continue
		}
		if found || len(app.Value) > 1024 {
			return false, errors.New("invalid core quota requirements")
		}
		value := new(structpb.Struct)
		if err := proto.Unmarshal(app.Value, value); err != nil {
			return false, errors.New("invalid core quota requirements")
		}
		fields := value.AsMap()
		if len(fields) != 2 || fields["version"] != "remnacust-core-requirements-v1" || fields["userQuota"] != true {
			return false, errors.New("unknown core quota requirements")
		}
		found = true
	}
	return found, nil
}

func RequireUserQuotaConfig(config *Config) error {
	required, err := ConfigRequiresUserQuota(config)
	if err != nil || required {
		return err
	}
	value, _ := structpb.NewStruct(map[string]any{"version": "remnacust-core-requirements-v1", "userQuota": true})
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(value)
	if err != nil {
		return err
	}
	config.App = append(config.App, &serial.TypedMessage{Type: UserQuotaRequirementType, Value: data})
	canonicalProfileMetadata(config)
	return nil
}

func (s *Instance) RequiresUserQuota() bool {
	if s.scopeParent != nil {
		return s.scopeParent.RequiresUserQuota()
	}
	return s.userQuotaRequired.Load() || (s.quotaRequirementParent != nil && s.quotaRequirementParent.RequiresUserQuota())
}

func (s *Instance) RuntimeCheckpointEnabled() bool {
	if s.scopeParent != nil {
		return s.scopeParent.RuntimeCheckpointEnabled()
	}
	if s.boundProfilePlan != nil {
		return s.boundProfilePlan.owner.RuntimeCheckpointEnabled()
	}
	return s.runtimeJournal != ""
}
