package xerahttp

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
)

type trackedXmuxConn struct{ closed atomic.Int32 }

func TestIdlePoolShrinksAfterBurst(t *testing.T) {
	m := NewXmuxManager(&XmuxConfig{}, func() XmuxConn { return &trackedXmuxConn{} })
	now := time.Now()
	for range 16 {
		c := m.newXmuxClient()
		c.idleSince.Store(now.Add(-time.Minute).UnixNano())
	}
	active := m.xmuxClients[0]
	active.AddRunning()
	m.pruneIdle(now)
	if len(m.xmuxClients) != 1 || m.xmuxClients[0] != active || active.XmuxConn.IsClosed() {
		t.Fatal("idle cleanup retained excess clients or closed the active client")
	}
	active.DoneRunning()
	m.pruneIdle(now.Add(time.Minute))
	if len(m.xmuxClients) != 1 || active.XmuxConn.IsClosed() {
		t.Fatal("idle cleanup discarded the minimum reusable pool")
	}
}

func (c *trackedXmuxConn) IsClosed() bool { return c.closed.Load() != 0 }
func (c *trackedXmuxConn) Close() error   { c.closed.Add(1); return nil }

func TestXmuxRetirementWaitsForAllLeases(t *testing.T) {
	r := &trackedXmuxConn{}
	c := &XmuxClient{XmuxConn: r}
	for range 32 {
		c.AddRunning()
	}
	c.NotUsed.Store(true)
	c.maybeClose()
	if r.closed.Load() != 0 {
		t.Fatal("closed a busy transport")
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() { defer wg.Done(); c.DoneRunning() }()
	}
	wg.Wait()
	c.maybeClose()
	if r.closed.Load() != 1 {
		t.Fatalf("close count: %d", r.closed.Load())
	}
}

func TestIdleHTTPClientCleanupPreservesActiveClient(t *testing.T) {
	globalDialerAccess.Lock()
	defer globalDialerAccess.Unlock()
	saved := globalDialerMap
	defer func() { globalDialerMap = saved }()
	globalDialerMap = make(map[dialerConf]*XmuxManager)
	r := &trackedXmuxConn{}
	c := &XmuxClient{XmuxConn: r}
	c.AddRunning()
	m := &XmuxManager{xmuxClients: []*XmuxClient{c}, lastUsed: time.Now().Add(-2 * net.ConnIdleTimeout)}
	key := dialerConf{MemoryStreamConfig: &internet.MemoryStreamConfig{}}
	globalDialerMap[key] = m
	removeIdleHTTPClients(time.Now())
	if len(globalDialerMap) != 1 || r.closed.Load() != 0 {
		t.Fatal("active client removed")
	}
	c.DoneRunning()
	removeIdleHTTPClients(time.Now())
	if len(globalDialerMap) != 0 || r.closed.Load() != 1 || m.xmuxClients != nil {
		t.Fatal("idle client retained")
	}
}
