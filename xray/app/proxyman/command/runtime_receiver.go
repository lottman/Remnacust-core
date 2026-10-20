//go:build !wasm

package command

import (
	"fmt"

	"github.com/xtls/xray-core/common/serial"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// TypedMessage contains encoded bytes. Comparing the envelope alone treats
// protobuf map order as a listener change, including inside transport settings.
func sameRuntimeReceiver(a, b *serial.TypedMessage) (bool, error) {
	if proto.Equal(a, b) {
		return true, nil
	}
	if a == nil || b == nil || a.Type != b.Type {
		return false, nil
	}
	left, err := a.GetInstance()
	if err != nil {
		return false, err
	}
	right, err := b.GetInstance()
	if err != nil {
		return false, err
	}
	for _, message := range []proto.Message{left, right} {
		if err := canonicalRuntimeReceiver(message, 0); err != nil {
			return false, err
		}
	}
	return proto.Equal(left, right), nil
}

func canonicalRuntimeReceiver(message proto.Message, depth int) error {
	if depth > 32 {
		return fmt.Errorf("receiver settings nesting exceeds the control limit")
	}
	if typed, ok := message.(*serial.TypedMessage); ok {
		nested, err := typed.GetInstance()
		if err != nil {
			return err
		}
		if err = canonicalRuntimeReceiver(nested, depth+1); err != nil {
			return err
		}
		typed.Value, err = (proto.MarshalOptions{Deterministic: true}).Marshal(nested)
		return err
	}
	var failure error
	message.ProtoReflect().Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		visit := func(child protoreflect.Message) bool {
			failure = canonicalRuntimeReceiver(child.Interface(), depth+1)
			return failure == nil
		}
		if field.IsMap() {
			if field.MapValue().Message() != nil {
				value.Map().Range(func(_ protoreflect.MapKey, entry protoreflect.Value) bool { return visit(entry.Message()) })
			}
		} else if field.Message() != nil {
			if field.IsList() {
				list := value.List()
				for i := 0; i < list.Len(); i++ {
					if !visit(list.Get(i).Message()) {
						return false
					}
				}
			} else {
				return visit(value.Message())
			}
		}
		return failure == nil
	})
	return failure
}
