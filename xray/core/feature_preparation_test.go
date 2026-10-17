package core

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type preparationFeature struct {
	id      string
	trace   *[]string
	failure error
}

func (*preparationFeature) Type() interface{} { return (*preparationFeature)(nil) }
func (f *preparationFeature) PrepareStart() error {
	*f.trace = append(*f.trace, "prepare:"+f.id)
	return f.failure
}
func (f *preparationFeature) Start() error {
	*f.trace = append(*f.trace, "start:"+f.id)
	return nil
}
func (*preparationFeature) Close() error { return nil }

func TestFeaturePreparationPrecedesAllRootListeners(t *testing.T) {
	for _, reject := range []bool{false, true} {
		trace := []string{}
		server := &Instance{ctx: context.Background()}
		listener := &preparationFeature{id: "listener", trace: &trace}
		policy := &preparationFeature{id: "policy", trace: &trace}
		if reject {
			policy.failure = errors.New("policy unavailable")
		}
		if err := server.AddFeature(listener); err != nil {
			t.Fatal(err)
		}
		if err := server.AddFeature(policy); err != nil {
			t.Fatal(err)
		}
		err := server.Start()
		want := []string{"prepare:listener", "prepare:policy"}
		if reject {
			if err == nil || server.IsRunning() {
				t.Fatal("failed policy preparation started a server")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, "start:listener", "start:policy")
		}
		if !reflect.DeepEqual(trace, want) {
			t.Fatalf("wrong startup order: %v", trace)
		}
		if err := server.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFeaturePreparationPrecedesAllStagedListeners(t *testing.T) {
	for _, reject := range []bool{false, true} {
		trace := []string{}
		parent := &Instance{ctx: context.Background()}
		scope := NewFeatureScope(parent)
		listener := &preparationFeature{id: "listener", trace: &trace}
		policy := &preparationFeature{id: "policy", trace: &trace}
		if reject {
			policy.failure = errors.New("policy unavailable")
		}
		if err := scope.Add(listener); err != nil {
			t.Fatal(err)
		}
		if err := scope.Add(policy); err != nil {
			t.Fatal(err)
		}
		err := scope.Start()
		want := []string{"prepare:listener", "prepare:policy"}
		if reject {
			if err == nil {
				t.Fatal("failed policy preparation started a staged listener")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, "start:listener", "start:policy")
		}
		if !reflect.DeepEqual(trace, want) {
			t.Fatalf("wrong staged order: %v", trace)
		}
		if err := scope.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
