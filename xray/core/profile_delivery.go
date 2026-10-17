package core

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/xtls/xray-core/common/serial"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

const ProfileDeliveryType = "remnacust.core.profile-delivery.v1"

func canonicalProfileMetadata(config *Config) {
	var metadata []*serial.TypedMessage
	apps := config.App[:0]
	for _, app := range config.App {
		if app != nil && (app.Type == ProfileDeliveryType || app.Type == ProfilePlanType || app.Type == UserQuotaRequirementType) {
			metadata = append(metadata, app)
		} else {
			apps = append(apps, app)
		}
	}
	sort.Slice(metadata, func(i, j int) bool { return metadata[i].Type < metadata[j].Type })
	config.App = append(apps, metadata...)
}

// The source plan digest is distinct from the current runtime snapshot: later
// imperative credential changes must survive an identical delivery retry.
type ProfileDelivery struct {
	Epoch, Owner, Revision, InputsHash, PlanHash string
	Enabled                                      bool
}

func profileDecimal(value string) (uint64, bool) {
	number, err := strconv.ParseUint(value, 10, 63)
	return number, err == nil && number > 0 && strconv.FormatUint(number, 10) == value
}

func profileHash(value string) bool {
	data, err := hex.DecodeString(value)
	return err == nil && len(data) == 32 && strings.ToLower(value) == value
}

func (d ProfileDelivery) validate() error {
	_, epoch := profileDecimal(d.Epoch)
	_, revision := profileDecimal(d.Revision)
	if !epoch || !revision || !canonicalProfileUUID(d.Owner) || !profileHash(d.InputsHash) || !profileHash(d.PlanHash) {
		return errors.New("invalid profile delivery identity")
	}
	return nil
}

func (d ProfileDelivery) Fields() map[string]any {
	return map[string]any{"epoch": d.Epoch, "owner": d.Owner, "revision": d.Revision, "enabled": d.Enabled, "inputsHash": d.InputsHash, "planHash": d.PlanHash}
}

func ConfigProfileDelivery(config *Config) (*ProfileDelivery, error) {
	if config == nil {
		return nil, errors.New("profile delivery configuration is missing")
	}
	var result *ProfileDelivery
	for _, app := range config.App {
		if app == nil || app.Type != ProfileDeliveryType {
			continue
		}
		if result != nil || len(app.Value) > 2048 {
			return nil, errors.New("invalid profile delivery metadata")
		}
		value := new(structpb.Struct)
		if err := proto.Unmarshal(app.Value, value); err != nil {
			return nil, errors.New("invalid profile delivery encoding")
		}
		fields := value.AsMap()
		epoch, e := fields["epoch"].(string)
		owner, o := fields["owner"].(string)
		revision, r := fields["revision"].(string)
		enabled, b := fields["enabled"].(bool)
		inputs, i := fields["inputsHash"].(string)
		plan, p := fields["planHash"].(string)
		if len(fields) != 6 || !e || !o || !r || !b || !i || !p {
			return nil, errors.New("invalid profile delivery fields")
		}
		result = &ProfileDelivery{Epoch: epoch, Owner: owner, Revision: revision, Enabled: enabled, InputsHash: inputs, PlanHash: plan}
		if err := result.validate(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func WithProfileDelivery(config *Config, delivery ProfileDelivery) (*Config, error) {
	if err := delivery.validate(); err != nil {
		return nil, err
	}
	bindings, found, err := ConfigProfileBindings(config)
	if err != nil || !found {
		return nil, errors.New("profile delivery requires a valid enrolled plan")
	}
	if !delivery.Enabled && len(bindings) != 0 {
		return nil, errors.New("a disabled profile delivery cannot expose managed graphs")
	}
	config = proto.Clone(config).(*Config)
	apps := config.App[:0]
	for _, app := range config.App {
		if app.Type != ProfileDeliveryType {
			apps = append(apps, app)
		}
	}
	value, _ := structpb.NewStruct(delivery.Fields())
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(value)
	if err != nil {
		return nil, err
	}
	config.App = append(apps, &serial.TypedMessage{Type: ProfileDeliveryType, Value: data})
	canonicalProfileMetadata(config)
	if proto.Size(config) > RuntimeJournalLimit {
		return nil, errors.New("profile delivery checkpoint exceeds its limit")
	}
	return config, nil
}

// Identical epochs are immutable. A newer lease cannot roll back the logical
// port-plan revision, and a clocked plan cannot become an unversioned plan.
func CheckProfileDeliveryAdvance(previous, target *ProfileDelivery) error {
	if target == nil {
		if previous != nil {
			return errors.New("a versioned profile delivery cannot be removed")
		}
		return nil
	}
	if err := target.validate(); err != nil {
		return err
	}
	if previous == nil {
		return nil
	}
	if err := previous.validate(); err != nil {
		return err
	}
	oldEpoch, _ := profileDecimal(previous.Epoch)
	nextEpoch, _ := profileDecimal(target.Epoch)
	oldRevision, _ := profileDecimal(previous.Revision)
	nextRevision, _ := profileDecimal(target.Revision)
	if nextEpoch < oldEpoch || nextRevision < oldRevision || (nextRevision == oldRevision && previous.Enabled != target.Enabled) || (nextEpoch == oldEpoch && *previous != *target) {
		return errors.New("stale or conflicting profile delivery")
	}
	return nil
}

// Mandatory quota markers are server policy, not a new source delivery. The
// digest still includes every credential, routing/transport field and UUID.
func ProfileBindingsDigest(bindings []ProfileBinding) (string, error) {
	return profileSourceDigest(&Config{}, bindings)
}

// ProfilePlanDigest also binds the root control/legacy graph. A maintenance
// replacement cannot reuse a lease with different root settings and the same
// managed bindings. The delivery record itself is excluded to avoid recursion.
func ProfilePlanDigest(config *Config) (string, error) {
	bindings, found, err := ConfigProfileBindings(config)
	if err != nil || !found {
		return "", errors.New("a valid profile source plan is required")
	}
	if _, err := ConfigRequiresUserQuota(config); err != nil {
		return "", err
	}
	root := proto.Clone(config).(*Config)
	apps := root.App[:0]
	for _, app := range root.App {
		if app.Type != ProfilePlanType && app.Type != ProfileDeliveryType && app.Type != UserQuotaRequirementType {
			apps = append(apps, app)
		}
	}
	root.App = apps
	return profileSourceDigest(root, bindings)
}

func profileSourceDigest(root *Config, bindings []ProfileBinding) (string, error) {
	if err := validateProfileBindings(bindings); err != nil {
		return "", err
	}
	copy := make([]ProfileBinding, len(bindings))
	for i, binding := range bindings {
		if binding.Config == nil {
			return "", errors.New("profile configuration is missing")
		}
		copy[i] = binding
		config := proto.Clone(binding.Config).(*Config)
		apps := config.App[:0]
		for _, app := range config.App {
			if app == nil || app.Type != UserQuotaRequirementType {
				apps = append(apps, app)
			}
		}
		config.App = apps
		copy[i].Config = config
	}
	config, err := WithProfileBindings(root, copy)
	if err != nil {
		return "", err
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(config)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}
