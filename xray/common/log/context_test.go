package log

import (
	"context"
	"sync"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

type contextRecorder struct {
	mu       sync.Mutex
	messages []Message
	level    Severity
}

func (h *contextRecorder) Handle(m Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, m)
}
func (h *contextRecorder) Severity() Severity { return h.level }
func (h *contextRecorder) count() int         { h.mu.Lock(); defer h.mu.Unlock(); return len(h.messages) }

func TestContextHandlersKeepIndependentSeverityAndDoNotChangeProcessDefault(t *testing.T) {
	previous := logHandler.Load()
	t.Cleanup(func() { logHandler.Store(previous) })
	root := &contextRecorder{level: Severity_Error}
	RegisterHandler(root)
	a, b := &contextRecorder{level: Severity_Debug}, &contextRecorder{level: Severity_Warning}
	ctxA := ContextWithHandler(ContextWithIsolatedHandler(context.Background()), a)
	ctxB := ContextWithHandler(ContextWithIsolatedHandler(context.Background()), b)
	if GetSeverityContext(ctxA) != Severity_Debug || GetSeverityContext(ctxB) != Severity_Warning || GetSeverity() != Severity_Error {
		t.Fatal("profile severity changed a neighbour")
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); RecordContext(ctxA, &GeneralMessage{Content: "A"}) }()
		go func() { defer wg.Done(); RecordContext(ctxB, &GeneralMessage{Content: "B"}) }()
	}
	wg.Wait()
	Record(&GeneralMessage{Content: "root"})
	RecordContext(ContextWithIsolatedHandler(context.Background()), &GeneralMessage{Content: "unresolved profile"})
	if a.count() != 100 || b.count() != 100 || root.count() != 1 {
		t.Fatalf("context routing escaped: A=%d B=%d root=%d", a.count(), b.count(), root.count())
	}
	RecordContext(nil, &GeneralMessage{Content: "legacy"})
	if root.count() != 2 {
		t.Fatal("legacy logging lost its process default")
	}
}

func TestLuaLogUsesOwningContextForHandlerAndSeverity(t *testing.T) {
	previous := logHandler.Load()
	t.Cleanup(func() { logHandler.Store(previous) })
	root := &contextRecorder{level: Severity_Debug}
	RegisterHandler(root)
	profile := &contextRecorder{level: Severity_Warning}
	L := lua.NewState()
	defer L.Close()
	L.SetContext(ContextWithHandler(ContextWithIsolatedHandler(context.Background()), profile))
	RegisterLua(L)
	if err := L.DoString(`local log=require("xray.log"); log.Info("hidden"); log.Warning("owned")`); err != nil {
		t.Fatal(err)
	}
	if root.count() != 0 || profile.count() != 1 {
		t.Fatal("Lua logging used the process handler or severity")
	}
}
