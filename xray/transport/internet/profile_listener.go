package internet

import (
	"context"
	"crypto/cipher"
	"fmt"
	"net"
	"sync"
	"syscall"

	"google.golang.org/protobuf/proto"
)

// TCPListenerPool owns physical sockets independently of feature graphs. A
// staged profile can prepare its transport without stealing live acceptance.
type TCPListenerPool struct {
	mu                  sync.Mutex
	closed              bool
	endpoints           map[string]*profileEndpoint
	profiles            map[*TCPListenerProfile]struct{}
	quicCipher          cipher.Block
	quicProfiles        map[uint64]*TCPListenerProfile
	quicNextID          uint64
	packetBind          sync.Mutex
	packetEndpoints     map[string]*profilePacketEndpoint
	packetWorkers       sync.WaitGroup
	packetQueuedBytes   int
	packetPendingRoutes int
	packetLifetime      context.Context
	packetCancel        context.CancelFunc
}

type TCPListenerProfile struct {
	pool                    *TCPListenerPool
	staged, retired, closed bool
	packetListeners         bool
	packetConnections       int
	quicProfileID           uint64
	quicCIDSequence         uint64
	packets                 map[*profilePacketConn]struct{}
	quicPending             map[string]map[*profilePacketInitial]struct{}
	listeners               map[*profileListener]struct{}
	connections             map[*profileConn]struct{}
	done                    chan struct{}
	doneOnce                sync.Once
}

type profileEndpoint struct {
	pool      *TCPListenerPool
	key       string
	addr      *net.TCPAddr
	options   *SocketConfig
	physical  net.Listener
	active    *profileListener
	listeners map[*profileListener]struct{}
}

type profileListener struct {
	profile  *TCPListenerProfile
	endpoint *profileEndpoint
	queue    chan net.Conn
	done     chan struct{}
	closed   bool
}

type profileConn struct {
	net.Conn
	profile *TCPListenerProfile
	once    sync.Once
}

func NewTCPListenerPool() *TCPListenerPool {
	lifetime, cancel := context.WithCancel(context.Background())
	return &TCPListenerPool{endpoints: make(map[string]*profileEndpoint), profiles: make(map[*TCPListenerProfile]struct{}), packetLifetime: lifetime, packetCancel: cancel}
}

func (p *TCPListenerPool) NewProfile(staged bool) *TCPListenerProfile {
	p.mu.Lock()
	defer p.mu.Unlock()
	v := &TCPListenerProfile{pool: p, staged: staged, listeners: make(map[*profileListener]struct{}), connections: make(map[*profileConn]struct{}), packets: make(map[*profilePacketConn]struct{}), quicPending: make(map[string]map[*profilePacketInitial]struct{}), done: make(chan struct{})}
	if p.closed {
		v.closed = true
		v.doneOnce.Do(func() { close(v.done) })
	}
	if !p.closed {
		p.profiles[v] = struct{}{}
	}
	return v
}

func tcpEndpointsOverlap(a, b *net.TCPAddr) bool {
	if a.Port != b.Port || a.Port == 0 {
		return false
	}
	dual := func(v *net.TCPAddr) bool { return len(v.IP) == 0 || v.IP.To4() == nil && v.IP.IsUnspecified() }
	if dual(a) || dual(b) {
		return true
	}
	if (a.IP.To4() != nil) != (b.IP.To4() != nil) {
		return false
	}
	return a.IP.IsUnspecified() || b.IP.IsUnspecified() || a.IP.Equal(b.IP) && a.Zone == b.Zone
}

// Listen prepares a TCP socket. Anonymous ports are recorded under the actual
// bound address; private Unix control sockets never enter this pool.
func (v *TCPListenerProfile) Listen(ctx context.Context, addr *net.TCPAddr, options *SocketConfig, bind func(context.Context, net.Addr, *SocketConfig) (net.Listener, error)) (net.Listener, error) {
	if addr == nil || bind == nil {
		return nil, fmt.Errorf("profile TCP address and binder are required")
	}
	p := v.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || v.closed || v.retired {
		return nil, net.ErrClosed
	}
	key := addr.String()
	e := p.endpoints[key]
	if e == nil {
		for _, existing := range p.endpoints {
			if tcpEndpointsOverlap(addr, existing.addr) {
				return nil, fmt.Errorf("profile TCP address overlaps existing listener %s", existing.key)
			}
		}
		physical, err := bind(ctx, addr, options)
		if err != nil {
			return nil, err
		}
		actual, ok := physical.Addr().(*net.TCPAddr)
		if !ok {
			_ = physical.Close()
			return nil, fmt.Errorf("profile listener is not TCP")
		}
		copyAddr := *actual
		copyAddr.IP = append(net.IP(nil), actual.IP...)
		if addr.Port == 0 {
			key = copyAddr.String()
		}
		e = &profileEndpoint{pool: p, key: key, addr: &copyAddr, physical: physical, listeners: make(map[*profileListener]struct{})}
		if options != nil {
			e.options = proto.Clone(options).(*SocketConfig)
		}
		p.endpoints[key] = e
		go e.serve()
	} else {
		if !proto.Equal(e.options, options) {
			return nil, fmt.Errorf("profile TCP socket options differ on %s", key)
		}
		if !v.staged {
			return nil, fmt.Errorf("TCP listener already owned on %s", key)
		}
		if len(e.listeners) >= 16 {
			return nil, fmt.Errorf("too many draining TCP listeners on %s", key)
		}
	}
	for l := range v.listeners {
		if l.endpoint == e {
			return nil, fmt.Errorf("duplicate profile TCP listener on %s", key)
		}
	}
	l := &profileListener{profile: v, endpoint: e, queue: make(chan net.Conn, 16), done: make(chan struct{})}
	v.listeners[l] = struct{}{}
	e.listeners[l] = struct{}{}
	if !v.staged {
		e.active = l
	}
	return l, nil
}

func (e *profileEndpoint) serve() {
	for {
		conn, err := e.physical.Accept()
		if err != nil {
			e.pool.mu.Lock()
			listeners := make([]*profileListener, 0, len(e.listeners))
			for l := range e.listeners {
				listeners = append(listeners, l)
			}
			e.pool.mu.Unlock()
			for _, l := range listeners {
				_ = l.Close()
			}
			return
		}
		e.pool.mu.Lock()
		l := e.active
		if l == nil || l.closed || l.profile.closed || l.profile.retired || e.pool.closed {
			e.pool.mu.Unlock()
			_ = conn.Close()
			continue
		}
		owned := &profileConn{Conn: conn, profile: l.profile}
		l.profile.connections[owned] = struct{}{}
		e.pool.mu.Unlock()
		select {
		case l.queue <- owned:
			// Close may win concurrently with the buffered send after draining
			// the queue. Recheck ownership so that race cannot strand a socket.
			e.pool.mu.Lock()
			closed := l.closed
			e.pool.mu.Unlock()
			if closed {
				_ = owned.Close()
			}
		case <-l.done:
			_ = owned.Close()
		}
	}
}

func (l *profileListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, net.ErrClosed
	default:
	}
	select {
	case conn := <-l.queue:
		select {
		case <-l.done:
			_ = conn.Close()
			return nil, net.ErrClosed
		default:
			return conn, nil
		}
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *profileListener) Addr() net.Addr { return l.endpoint.physical.Addr() }
func (l *profileListener) closeLocked() {
	if l.closed {
		return
	}
	l.closed = true
	close(l.done)
	delete(l.profile.listeners, l)
	delete(l.endpoint.listeners, l)
	if l.endpoint.active == l {
		l.endpoint.active = nil
	}
	if len(l.endpoint.listeners) == 0 {
		delete(l.endpoint.pool.endpoints, l.endpoint.key)
		_ = l.endpoint.physical.Close()
	}
}
func (l *profileListener) Close() error {
	p := l.profile.pool
	p.mu.Lock()
	l.closeLocked()
	p.mu.Unlock()
	for {
		select {
		case c := <-l.queue:
			_ = c.Close()
		default:
			return nil
		}
	}
}
func (c *profileConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		p := c.profile.pool
		p.mu.Lock()
		delete(c.profile.connections, c)
		c.profile.finishLocked()
		p.mu.Unlock()
	})
	return err
}

func (c *profileConn) SyscallConn() (syscall.RawConn, error) {
	if conn, ok := c.Conn.(syscall.Conn); ok {
		return conn.SyscallConn()
	}
	return nil, fmt.Errorf("profile socket does not expose a raw descriptor")
}

func (c *profileConn) CloseRead() error {
	if conn, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return conn.CloseRead()
	}
	return fmt.Errorf("profile socket does not support CloseRead")
}

func (c *profileConn) CloseWrite() error {
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return fmt.Errorf("profile socket does not support CloseWrite")
}
func (v *TCPListenerProfile) finishLocked() {
	if (v.retired || v.closed) && len(v.connections) == 0 && v.packetConnections == 0 {
		v.doneOnce.Do(func() { close(v.done) })
	}
	if v.closed && len(v.connections) == 0 && v.packetConnections == 0 {
		delete(v.pool.profiles, v)
		delete(v.pool.quicProfiles, v.quicProfileID)
	}
}
func (v *TCPListenerProfile) Done() <-chan struct{} { return v.done }

func (v *TCPListenerProfile) HasPacketListeners() bool {
	v.pool.mu.Lock()
	defer v.pool.mu.Unlock()
	return v.packetListeners
}

// Switch performs one acceptance decision across all prepared TCP endpoints.
// The caller must durably persist the graph decision before invoking Switch.
// Profiles that disappear are retired explicitly, so unaffected graphs remain.
func (p *TCPListenerPool) Switch(activate, retire []*TCPListenerProfile) error {
	return p.CommitSwitch(activate, retire, nil)
}

// CommitSwitch validates before persistence and keeps listener ownership locked
// through that durable decision. persist must not call back into this pool.
func (p *TCPListenerPool) CommitSwitch(activate, retire []*TCPListenerProfile, persist func() error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return net.ErrClosed
	}
	seen := make(map[*profileEndpoint]bool)
	packetSeen := make(map[*profilePacketEndpoint]bool)
	old := make(map[*TCPListenerProfile]bool)
	for _, v := range retire {
		if v == nil || v.pool != p || v.closed || v.retired || old[v] {
			return fmt.Errorf("invalid retiring TCP profile")
		}
		old[v] = true
	}
	for _, v := range activate {
		if v == nil || v.pool != p || v.closed || v.retired || !v.staged || old[v] {
			return fmt.Errorf("invalid staged TCP profile")
		}
		for l := range v.listeners {
			if l.closed || seen[l.endpoint] {
				return fmt.Errorf("conflicting staged TCP endpoint")
			}
			if a := l.endpoint.active; a != nil && !old[a.profile] {
				return fmt.Errorf("TCP endpoint still owned by another profile")
			}
			seen[l.endpoint] = true
		}
		for c := range v.packets {
			if c.closed || c.endpoint.failed || packetSeen[c.endpoint] {
				return fmt.Errorf("conflicting staged packet endpoint")
			}
			if a := c.endpoint.active; a != nil && !old[a.profile] {
				return fmt.Errorf("packet endpoint still owned by another profile")
			}
			packetSeen[c.endpoint] = true
		}
	}
	if persist != nil {
		if err := persist(); err != nil {
			return err
		}
	}
	for _, v := range retire {
		v.retired = true
		for l := range v.listeners {
			if l.endpoint.active == l {
				l.endpoint.active = nil
			}
		}
		for c := range v.packets {
			if c.endpoint.active == c {
				c.endpoint.active = nil
			}
		}
		v.finishLocked()
	}
	for _, v := range activate {
		v.staged = false
		for l := range v.listeners {
			l.endpoint.active = l
		}
		for c := range v.packets {
			c.endpoint.active = c
		}
	}
	return nil
}

func (v *TCPListenerProfile) Close() error {
	p := v.pool
	p.mu.Lock()
	v.closed = true
	listeners := make([]*profileListener, 0, len(v.listeners))
	for l := range v.listeners {
		listeners = append(listeners, l)
		l.closeLocked()
	}
	connections := make([]*profileConn, 0, len(v.connections))
	for c := range v.connections {
		connections = append(connections, c)
	}
	packets := make([]*profilePacketConn, 0, len(v.packets))
	for c := range v.packets {
		packets = append(packets, c)
	}
	v.finishLocked()
	p.mu.Unlock()
	for _, l := range listeners {
		_ = l.Close()
	}
	for _, c := range connections {
		_ = c.Close()
	}
	for _, c := range packets {
		_ = c.Close()
	}
	return nil
}

func (p *TCPListenerPool) Close() error {
	p.mu.Lock()
	p.closed = true
	profiles := make(map[*TCPListenerProfile]bool)
	for v := range p.profiles {
		profiles[v] = true
	}
	p.mu.Unlock()
	if p.packetCancel != nil {
		p.packetCancel()
	}
	for v := range profiles {
		_ = v.Close()
	}
	p.packetWorkers.Wait()
	return nil
}

// UnwrapProfileListenerConn exposes the original socket to existing native
// splice/original-destination inspection while Close ownership stays wrapped.
func UnwrapProfileListenerConn(conn net.Conn) net.Conn {
	if c, ok := conn.(*profileConn); ok {
		return c.Conn
	}
	return conn
}
