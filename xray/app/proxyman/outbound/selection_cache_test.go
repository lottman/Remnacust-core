package outbound

import (
	"context"
	"reflect"
	"testing"

	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/features/outbound"
)

type selectionHandler struct {
	outbound.Handler
	tag string
}

func (h selectionHandler) Tag() string { return h.tag }

func TestSelectionCacheSeparatesCommaTagsAndOwnsItsSlices(t *testing.T) {
	manager, err := New(context.Background(), &proxyman.OutboundConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"a-one", "b-one", "a,b-one"} {
		if err := manager.AddHandler(context.Background(), selectionHandler{tag: tag}); err != nil {
			t.Fatal(err)
		}
	}
	wantComma := []string{"a,b-one"}
	wantSeparate := []string{"a,b-one", "a-one", "b-one"}
	if got := manager.Select([]string{"a,b"}); !reflect.DeepEqual(got, wantComma) {
		t.Fatalf("comma prefix: %v", got)
	}
	if got := manager.Select([]string{"a", "b"}); !reflect.DeepEqual(got, wantSeparate) {
		t.Fatalf("different selectors shared a cache key: %v", got)
	}
	for range 2 {
		got := manager.Select([]string{"a,b"})
		got[0] = "corrupted-by-caller"
		if got := manager.Select([]string{"a,b"}); !reflect.DeepEqual(got, wantComma) {
			t.Fatalf("cached selection was changed by a caller: %v", got)
		}
	}
	if err := manager.RemoveHandler(context.Background(), "a,b-one"); err != nil {
		t.Fatal(err)
	}
	if got := manager.Select([]string{"a,b"}); len(got) != 0 {
		t.Fatalf("removed outbound remained cached: %v", got)
	}
}
