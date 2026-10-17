package core

import (
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func hostPolicyEmail(value string) bool {
	parts := strings.Split(value, "~")
	if len(parts) != 3 || len(parts[1]) != 24 || len(parts[2]) != 33 || parts[2][0] != 'h' || len(parts[0]) == 0 || parts[0][0] == '0' {
		return false
	}
	for _, ch := range parts[0] {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	for _, part := range []string{parts[1], parts[2][1:]} {
		for _, ch := range part {
			if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
				return false
			}
		}
	}
	return true
}

// The cached native configuration itself records which listeners use host keys.
// Losing the external policy file must not make legacy keys on those listeners
// unprotected during an autonomous restart.
func hostPolicyTags(config *Config) map[string]bool {
	tags := make(map[string]bool)
	if config == nil {
		return tags
	}
	for _, inbound := range config.Inbound {
		if inbound == nil || inbound.Tag == "" || inbound.ProxySettings == nil {
			continue
		}
		value, err := inbound.ProxySettings.GetInstance()
		message, ok := value.(proto.Message)
		if err != nil || !ok {
			tags[inbound.Tag] = true
			continue
		}
		visited := 0
		var inspect func(protoreflect.Message, int) bool
		inspect = func(message protoreflect.Message, depth int) bool {
			visited++
			if depth > 32 || visited > 100000 {
				return true
			}
			protected := false
			message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
				if field.Name() == "email" && field.Kind() == protoreflect.StringKind && !field.IsList() {
					protected = hostPolicyEmail(value.String())
				}
				if field.Kind() == protoreflect.MessageKind && !field.IsMap() {
					if field.IsList() {
						list := value.List()
						for i := 0; i < list.Len() && !protected; i++ {
							protected = inspect(list.Get(i).Message(), depth+1)
						}
					} else {
						protected = inspect(value.Message(), depth+1)
					}
				}
				return !protected
			})
			return protected
		}
		if inspect(message.ProtoReflect(), 0) {
			tags[inbound.Tag] = true
		}
	}
	return tags
}

func (s *Instance) RequiresHostPolicy(tag string) bool {
	if s.scopeParent != nil {
		return s.scopeParent.RequiresHostPolicy(tag)
	}
	value := s.hostPolicyRequired.Load()
	if value == nil {
		return false
	}
	return value.(map[string]bool)[tag]
}
