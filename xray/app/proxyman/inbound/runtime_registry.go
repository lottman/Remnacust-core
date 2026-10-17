package inbound

import (
	"context"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/features/inbound"
)

func (m *Manager) CloseRetired(expected inbound.Handler) error {
	m.access.RLock()
	present := m.retiredHandlers[expected.Tag()] == expected
	m.access.RUnlock()
	if !present {
		return nil
	}
	err := expected.Close()
	m.access.Lock()
	if m.retiredHandlers[expected.Tag()] == expected {
		delete(m.retiredHandlers, expected.Tag())
	}
	m.access.Unlock()
	return err
}

// Caller holds the instance admission barrier during shutdown.
func (m *Manager) QuiesceRuntimeLocked() {
	m.access.RLock()
	defer m.access.RUnlock()
	for _, handlers := range []map[string]inbound.Handler{m.taggedHandlers, m.retiredHandlers} {
		for _, handler := range handlers {
			if always, ok := handler.(*AlwaysOnInboundHandler); ok {
				always.RuntimeSlot().Prepare()
			}
		}
	}
}

// Stage creates native listeners with admission disabled. They enter the public
// registry only at commit; a bind failure cannot leave a visible half-handler.
func (m *Manager) Stage(handler *AlwaysOnInboundHandler) error {
	m.access.Lock()
	defer m.access.Unlock()
	if m.taggedHandlers[handler.Tag()] != nil || m.retiredHandlers[handler.Tag()] != nil {
		return errors.New("inbound tag is active or draining: ", handler.Tag())
	}
	handler.RuntimeSlot().Prepare()
	return handler.Start()
}

// CommitRuntimeLocked requires the instance admission barrier. Cleanup closures
// execute after releasing that barrier, including listener drain completion.
func (m *Manager) CommitRuntimeLocked(ctx context.Context, staged []*AlwaysOnInboundHandler, retained []*AlwaysOnInboundHandler, removed []string, generation *RuntimeGeneration) []func() {
	m.access.Lock()
	defer m.access.Unlock()
	var cleanup []func()
	for _, handler := range staged {
		m.taggedHandlers[handler.Tag()] = handler
		if close := handler.RuntimeSlot().SwapLocked(generation); close != nil {
			cleanup = append(cleanup, close)
		}
		handler.RuntimeSlot().ActivateLocked()
	}
	for _, handler := range retained {
		if close := handler.RuntimeSlot().SwapLocked(generation); close != nil {
			cleanup = append(cleanup, close)
		}
	}
	for _, tag := range removed {
		handler := m.taggedHandlers[tag]
		if handler == nil {
			continue
		}
		delete(m.taggedHandlers, tag)
		m.retiredHandlers[tag] = handler
		close := func(tag string, handler inbound.Handler) func() {
			return func() {
				_ = handler.Close()
				m.access.Lock()
				if m.retiredHandlers[tag] == handler {
					delete(m.retiredHandlers, tag)
				}
				m.access.Unlock()
			}
		}(tag, handler)
		if always, ok := handler.(*AlwaysOnInboundHandler); ok {
			if drain := always.RuntimeSlot().DrainLocked(close); drain != nil {
				cleanup = append(cleanup, drain)
			}
		} else {
			cleanup = append(cleanup, close)
		}
	}
	return cleanup
}
