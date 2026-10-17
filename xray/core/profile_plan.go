package core

import (
	"encoding/base64"
	"errors"
	"sort"
	"sync"

	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/common/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

const ProfilePlanType = "remnacust.core.profile-plan.v1"

// ProfileBinding preserves server provenance independently of local Xray tags.
type ProfileBinding struct {
	BindingID, ProfileID, InboundID string
	Config                          *Config
}

func canonicalProfileUUID(value string) bool {
	id, err := uuid.ParseString(value)
	return err == nil && id.String() == value && id[6]>>4 >= 1 && id[6]>>4 <= 8 && id[8]&0xc0 == 0x80
}

func validateProfileBindings(bindings []ProfileBinding) error {
	if len(bindings) > 512 {
		return errors.New("profile plan exceeds 512 graphs")
	}
	seen := make(map[string]bool, len(bindings))
	bytes := 0
	for _, binding := range bindings {
		if !canonicalProfileUUID(binding.BindingID) || !canonicalProfileUUID(binding.ProfileID) || !canonicalProfileUUID(binding.InboundID) || seen[binding.BindingID] || binding.Config == nil {
			return errors.New("invalid profile binding provenance")
		}
		seen[binding.BindingID] = true
		if len(binding.Config.App) > 64 || len(binding.Config.Inbound) > 512 || len(binding.Config.Outbound) > 4096 {
			return errors.New("profile graph exceeds native feature or handler limits")
		}
		tags := make(map[string]bool, len(binding.Config.Inbound))
		for _, handler := range binding.Config.Inbound {
			if handler == nil || handler.Tag == "" || tags[handler.Tag] || handler.ReceiverSettings == nil || handler.ProxySettings == nil {
				return errors.New("invalid profile inbound settings")
			}
			tags[handler.Tag] = true
		}
		for _, handler := range binding.Config.Outbound {
			if handler == nil || handler.ProxySettings == nil {
				return errors.New("invalid profile outbound settings")
			}
		}
		for _, app := range binding.Config.App {
			if app == nil {
				return errors.New("invalid profile feature settings")
			}
			if app.Type == ProfilePlanType || app.Type == ProfileDeliveryType {
				return errors.New("nested profile plans are not supported")
			}
		}
		if _, err := ConfigRequiresUserQuota(binding.Config); err != nil {
			return err
		}
		size := proto.Size(binding.Config)
		bytes += size
		if size > 4*1024*1024 || bytes > RuntimeJournalLimit {
			return errors.New("profile plan configuration exceeds the limit")
		}
	}
	return nil
}

// WithProfileBindings carries the entire fleet in the existing checksummed,
// private native checkpoint. No process environment or JSON builder is run.
func WithProfileBindings(root *Config, bindings []ProfileBinding) (*Config, error) {
	if root == nil {
		return nil, errors.New("profile plan root is missing")
	}
	if _, _, err := ConfigProfileBindings(root); err != nil {
		return nil, err
	}
	if err := validateProfileBindings(bindings); err != nil {
		return nil, err
	}
	if delivery, err := ConfigProfileDelivery(root); err != nil {
		return nil, err
	} else if delivery != nil && !delivery.Enabled && len(bindings) != 0 {
		return nil, errors.New("enable the delivery before adding managed graphs")
	}
	root = proto.Clone(root).(*Config)
	apps := root.App[:0]
	for _, app := range root.App {
		if app == nil {
			return nil, errors.New("invalid root feature settings")
		}
		if app.Type != ProfilePlanType {
			apps = append(apps, app)
		}
	}
	root.App = apps
	graphs := make(map[string]any, len(bindings))
	for _, binding := range bindings {
		data, err := proto.MarshalOptions{Deterministic: true}.Marshal(binding.Config)
		if err != nil {
			return nil, err
		}
		graphs[binding.BindingID] = map[string]any{"profile": binding.ProfileID, "inbound": binding.InboundID, "config": base64.StdEncoding.EncodeToString(data)}
	}
	value, err := structpb.NewStruct(map[string]any{"version": "remnacust-profile-plan-v1", "graphs": graphs})
	if err != nil {
		return nil, err
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(value)
	if err != nil {
		return nil, err
	}
	root.App = append(root.App, &serial.TypedMessage{Type: ProfilePlanType, Value: data})
	canonicalProfileMetadata(root)
	if proto.Size(root) > RuntimeJournalLimit {
		return nil, errors.New("complete profile checkpoint exceeds the limit")
	}
	return root, nil
}

func ConfigProfileBindings(root *Config) ([]ProfileBinding, bool, error) {
	if root == nil {
		return nil, false, errors.New("profile plan root is missing")
	}
	var bindings []ProfileBinding
	found := false
	for _, app := range root.App {
		if app == nil {
			return nil, false, errors.New("invalid root feature settings")
		}
		if app.Type != ProfilePlanType {
			continue
		}
		if found || len(app.Value) > RuntimeJournalLimit {
			return nil, false, errors.New("invalid profile plan metadata")
		}
		found = true
		value := new(structpb.Struct)
		if err := proto.Unmarshal(app.Value, value); err != nil {
			return nil, false, errors.New("invalid profile plan encoding")
		}
		fields := value.AsMap()
		graphs, ok := fields["graphs"].(map[string]any)
		if len(fields) != 2 || fields["version"] != "remnacust-profile-plan-v1" || !ok || len(graphs) > 512 {
			return nil, false, errors.New("invalid profile plan fields")
		}
		for id, raw := range graphs {
			graph, ok := raw.(map[string]any)
			if !ok || len(graph) != 3 {
				return nil, false, errors.New("invalid profile graph fields")
			}
			profile, pOK := graph["profile"].(string)
			inbound, iOK := graph["inbound"].(string)
			encoded, cOK := graph["config"].(string)
			if !pOK || !iOK || !cOK || len(encoded) > base64.StdEncoding.EncodedLen(4*1024*1024) {
				return nil, false, errors.New("invalid profile graph encoding")
			}
			data, err := base64.StdEncoding.Strict().DecodeString(encoded)
			if err != nil {
				return nil, false, errors.New("invalid profile graph configuration")
			}
			config := new(Config)
			if err := proto.Unmarshal(data, config); err != nil {
				return nil, false, errors.New("invalid profile graph protobuf")
			}
			bindings = append(bindings, ProfileBinding{BindingID: id, ProfileID: profile, InboundID: inbound, Config: config})
		}
	}
	if err := validateProfileBindings(bindings); err != nil {
		return nil, false, err
	}
	if delivery, err := ConfigProfileDelivery(root); err != nil {
		return nil, false, err
	} else if delivery != nil && !found {
		return nil, false, errors.New("delivery metadata has no enrolled profile plan")
	} else if delivery != nil && !delivery.Enabled && len(bindings) != 0 {
		return nil, false, errors.New("a disabled profile delivery cannot expose managed graphs")
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].BindingID < bindings[j].BindingID })
	return bindings, found, nil
}

type profilePlanState struct {
	mu       sync.Mutex
	owner    *Instance
	bindings []ProfileBinding
	live     map[string]*Instance
	retiring map[*Instance]struct{}
}

// All graph mutations share the owning node's management lock, but keep their
// distinct admission barriers and runtime controller state.
func (s *Instance) ManagementAccess() *sync.Mutex { return &s.AccountingOwner().managementAccess }

func (s *Instance) RuntimeManagementReady() bool {
	owner := s.AccountingOwner()
	return s.startComplete.Load() && !s.ProfilePreparing() && !s.profileRetired.Load() && owner.startComplete.Load() && !owner.profilePlanStarting.Load() && !owner.RuntimeAdmissionFenced()
}

func (s *Instance) ProfilePreparing() bool {
	if s.scopeParent != nil {
		return s.scopeParent.ProfilePreparing()
	}
	return s.profilePreparing.Load() || s.profilePlanStarting.Load() || s.accountingParent != nil && s.accountingParent.ProfilePreparing()
}

func (s *Instance) profilePrepared() bool {
	return s.startComplete.Load() && !s.profileRetired.Load() && s.AccountingOwner().RuntimeManagementReady()
}

func (s *Instance) RuntimeAdmissionFenced() bool {
	if s.scopeParent != nil {
		return s.scopeParent.RuntimeAdmissionFenced()
	}
	if s.admissionFenced.Load() || s.ProfilePreparing() || s.profilePlanStarting.Load() {
		return true
	}
	return s.accountingParent != nil && s.accountingParent.RuntimeAdmissionFenced()
}

// Retirement stops new physical acceptance; already owned multiplex transports
// may continue opening authenticated streams until their connection drains.
func (s *Instance) RetiredProfile() bool {
	if s.scopeParent != nil {
		return s.scopeParent.RetiredProfile()
	}
	return s.profileRetired.Load()
}

// An uncertain durable decision requires restart/recovery, never automatic
// re-admission from a later request. Established traffic keeps its pinned graph.
func (s *Instance) FenceRuntimeAdmission() { s.AccountingOwner().admissionFenced.Store(true) }

func (s *Instance) ManagedProfile(bindingID string) *Instance {
	if s.profilePlan == nil {
		return nil
	}
	s.profilePlan.mu.Lock()
	defer s.profilePlan.mu.Unlock()
	return s.profilePlan.live[bindingID]
}

func (p *profilePlanState) save(bindingID string, config *Config) error {
	if bindingID == "" {
		return p.saveUpdates(config, nil)
	}
	return p.saveUpdates(nil, map[string]*Config{bindingID: config})
}

// The owning management transaction supplies the complete changed set; retained
// graphs are copied from the same manifest under its persistence lock.
func (s *Instance) SaveManagedProfileBatch(configs map[string]*Config) error {
	if s.profilePlan == nil || len(configs) == 0 {
		return errors.New("an enrolled profile plan and nonempty update set are required")
	}
	return s.profilePlan.saveUpdates(nil, configs)
}

// Caller holds ManagementAccess from revision validation through activation.
func (s *Instance) SaveManagedProfileDelivery(root *Config, configs map[string]*Config) error {
	if s.profilePlan == nil || root == nil {
		return errors.New("an enrolled owning plan is required")
	}
	return s.profilePlan.saveUpdates(root, configs)
}

func (p *profilePlanState) saveUpdates(rootOverride *Config, updates map[string]*Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.owner.profilePlanStarting.Load() && rootOverride == nil {
		// The root checkpoint was flushed before starting any child. Each
		// child's initial snapshot must match it (with inherited quota metadata),
		// not rewrite the entire fleet once per graph during boot.
		matched := 0
		for _, binding := range p.bindings {
			if config, found := updates[binding.BindingID]; found {
				expected := proto.Clone(binding.Config).(*Config)
				if p.owner.RequiresUserQuota() {
					if err := RequireUserQuotaConfig(expected); err != nil {
						return err
					}
				}
				if config == nil || !proto.Equal(expected, config) {
					return errors.New("profile configuration changed during plan startup")
				}
				matched++
			}
		}
		if matched != len(updates) {
			return errors.New("profile binding is not enrolled during startup")
		}
		return nil
	}
	root := p.owner.Configuration()
	previous := proto.Clone(root).(*Config)
	bindings := append([]ProfileBinding(nil), p.bindings...)
	if rootOverride != nil {
		previousDelivery, err := ConfigProfileDelivery(root)
		if err != nil {
			return err
		}
		nextDelivery, err := ConfigProfileDelivery(rootOverride)
		if err != nil {
			return err
		}
		if err := CheckProfileDeliveryAdvance(previousDelivery, nextDelivery); err != nil {
			return err
		}
		root = rootOverride
	}
	matched := 0
	for i := range bindings {
		if config, found := updates[bindings[i].BindingID]; found {
			if config == nil {
				return errors.New("profile configuration is missing")
			}
			bindings[i].Config = proto.Clone(config).(*Config)
			matched++
		}
	}
	if matched != len(updates) {
		return errors.New("profile binding is no longer enrolled")
	}
	target, err := WithProfileBindings(root, bindings)
	if err != nil {
		return err
	}
	if err := SaveRuntimeJournal(p.owner.runtimeJournal, target); err != nil {
		// Rename may have succeeded before directory fsync failed. Restore the
		// whole old manifest before returning an ordinary transaction failure.
		if rollback := SaveRuntimeJournal(p.owner.runtimeJournal, previous); rollback != nil {
			p.owner.FenceRuntimeAdmission()
			return errors.New("profile checkpoint outcome is uncertain; admission fenced until administrator recovery")
		}
		return err
	}
	p.bindings = bindings
	p.owner.RecordConfiguration(target)
	return nil
}

func (p *profilePlanState) start() error {
	var created []*Instance
	fail := func(err error) error {
		p.owner.FenceRuntimeAdmission()
		for _, profile := range created {
			_ = profile.Close()
		}
		p.mu.Lock()
		p.live = nil
		p.mu.Unlock()
		return err
	}
	for _, binding := range p.bindings {
		profile, err := NewManagedProfile(p.owner, binding.Config)
		if err != nil {
			return fail(err)
		}
		created = append(created, profile)
		p.owner.lifecycleAccess.Lock()
		profile.boundProfilePlan, profile.profileBindingID = p, binding.BindingID
		p.owner.lifecycleAccess.Unlock()
		p.mu.Lock()
		p.live[binding.BindingID] = profile
		p.mu.Unlock()
	}
	for _, profile := range created {
		if err := profile.Start(); err != nil {
			return fail(err)
		}
	}
	return nil
}
