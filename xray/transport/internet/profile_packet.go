package internet

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
)

const profilePacketQueueBytes = 16 * 1024 * 1024
const profilePacketPendingLimit = 65536

type profilePacketBypassKey struct{}

type profileDatagram struct {
	data   []byte
	remote net.Addr
}

type profilePacketInitial struct {
	owner   *profilePacketConn
	key     string
	expires time.Time
	pending bool
	remotes map[string]struct{}
}

type profilePacketEndpoint struct {
	pool            *TCPListenerPool
	key             string
	addr            *net.UDPAddr
	maskID          string
	options         *SocketConfig
	physical        net.PacketConn
	active          *profilePacketConn
	listeners       map[*profilePacketConn]struct{}
	initial         map[string]*profilePacketInitial
	peers           map[string]*profilePacketPeer
	failed          bool
	cancel          context.CancelFunc
	ctx             context.Context
	writes          chan struct{}
	closeOnce       sync.Once
	stopPool        func() bool
	sessionProtocol string
	sessionKey      func([]byte) string
}

type profilePacketConn struct {
	profile                     *TCPListenerProfile
	endpoint                    *profilePacketEndpoint
	queue                       chan profileDatagram
	done                        chan struct{}
	closed                      bool
	quic                        bool
	readDeadline, writeDeadline time.Time
	deadlineChanged             chan struct{}
}

// ListenProfileQUICPacket places the decoder below shared packet ownership.
// A single physical mask instance survives retirement of individual graphs.
func ListenProfileQUICPacket(ctx context.Context, addr *net.UDPAddr, stream *MemoryStreamConfig) (net.PacketConn, error) {
	return listenProfilePacket(ctx, addr, stream, true, "", nil)
}

func ListenProfileUDPPacket(ctx context.Context, addr *net.UDPAddr, stream *MemoryStreamConfig) (net.PacketConn, error) {
	return listenProfilePacket(ctx, addr, stream, false, "", nil)
}

// Session keys are extracted after the shared packet mask. Protocol versions
// must identify the same bounded parser across generations of the listener.
func ListenProfileSessionPacket(ctx context.Context, addr *net.UDPAddr, stream *MemoryStreamConfig, protocol string, key func([]byte) string) (net.PacketConn, error) {
	if protocol == "" || len(protocol) > 32 || key == nil {
		return nil, fmt.Errorf("invalid managed packet session protocol")
	}
	return listenProfilePacket(ctx, addr, stream, false, protocol, key)
}

func listenProfilePacket(ctx context.Context, addr *net.UDPAddr, stream *MemoryStreamConfig, quic bool, sessionProtocol string, sessionKey func([]byte) string) (net.PacketConn, error) {
	if addr == nil || stream == nil {
		return nil, fmt.Errorf("QUIC packet address and stream settings are required")
	}
	bind := func(scope context.Context) (net.PacketConn, error) {
		if stream.FinalMask != nil {
			return stream.FinalMask.ListenPacket(scope, addr)
		}
		return ListenSystemPacket(scope, addr, stream.SocketSettings)
	}
	provider, managed := ctx.Value(listenerProfileKey{}).(ListenerProfileProvider)
	if !managed {
		return bind(ctx)
	}
	// Original-destination sockets need their ancillary-message path. Unknown
	// hand-built mask instances cannot be proved compatible with an old decoder.
	if stream.SocketSettings.GetReceiveOriginalDestAddress() || stream.FinalMask != nil && stream.PacketMaskID == "" {
		if err := CheckPacketListenerProfile(ctx); err != nil {
			return nil, err
		}
		return bind(ctx)
	}
	v := provider.ListenerProfile()
	if v == nil {
		return nil, fmt.Errorf("QUIC packet profile is missing")
	}
	p := v.pool
	p.packetBind.Lock()
	defer p.packetBind.Unlock()
	p.mu.Lock()
	if p.closed || v.closed || v.retired {
		p.mu.Unlock()
		return nil, net.ErrClosed
	}
	key := addr.String()
	e := p.packetEndpoints[key]
	maskID := stream.PacketMaskID
	if maskID == "" {
		maskID = "none"
	}
	if e != nil {
		if e.failed || !v.staged || e.maskID != maskID || e.sessionProtocol != sessionProtocol || !proto.Equal(e.options, stream.SocketSettings) || len(e.listeners) >= 16 {
			p.mu.Unlock()
			return nil, fmt.Errorf("packet socket is owned or its decoder/socket options differ on %s", key)
		}
		for existing := range v.packets {
			if existing.endpoint == e {
				p.mu.Unlock()
				return nil, fmt.Errorf("duplicate profile packet listener")
			}
		}
	} else {
		for _, existing := range p.packetEndpoints {
			if tcpEndpointsOverlap(&net.TCPAddr{IP: addr.IP, Port: addr.Port, Zone: addr.Zone}, &net.TCPAddr{IP: existing.addr.IP, Port: existing.addr.Port, Zone: existing.addr.Zone}) {
				p.mu.Unlock()
				return nil, fmt.Errorf("profile UDP address overlaps existing listener %s", existing.key)
			}
		}
		p.mu.Unlock()
		physicalContext := ctx
		if owner, ok := provider.(interface{ PacketSocketContext() context.Context }); ok {
			physicalContext = owner.PacketSocketContext()
		}
		physicalContext, cancel := context.WithCancel(context.WithoutCancel(physicalContext))
		stopPool := context.AfterFunc(p.packetLifetime, cancel)
		physicalContext = context.WithValue(physicalContext, profilePacketBypassKey{}, true)
		physical, err := bind(physicalContext)
		if err != nil {
			stopPool()
			cancel()
			return nil, err
		}
		actual := *addr
		actual.IP = append(net.IP(nil), addr.IP...)
		switch a := physical.LocalAddr().(type) {
		case *net.UDPAddr:
			actual = *a
		case *net.TCPAddr:
			actual = net.UDPAddr{IP: a.IP, Port: a.Port, Zone: a.Zone}
		}
		actual.IP = append(net.IP(nil), actual.IP...)
		if addr.Port == 0 {
			if actual.Port == 0 {
				stopPool()
				cancel()
				_ = physical.Close()
				return nil, fmt.Errorf("packet mask did not identify its anonymous port")
			}
			key = actual.String()
		}
		p.mu.Lock()
		if p.closed || v.closed || v.retired {
			p.mu.Unlock()
			stopPool()
			cancel()
			_ = physical.Close()
			return nil, net.ErrClosed
		}
		e = &profilePacketEndpoint{pool: p, key: key, addr: &actual, maskID: maskID, physical: physical, sessionProtocol: sessionProtocol, sessionKey: sessionKey,
			listeners: make(map[*profilePacketConn]struct{}), initial: make(map[string]*profilePacketInitial), peers: make(map[string]*profilePacketPeer),
			ctx: physicalContext, cancel: cancel, stopPool: stopPool, writes: make(chan struct{}, 1)}
		if stream.SocketSettings != nil {
			e.options = proto.Clone(stream.SocketSettings).(*SocketConfig)
		}
		if p.packetEndpoints == nil {
			p.packetEndpoints = make(map[string]*profilePacketEndpoint)
		}
		p.packetEndpoints[key] = e
		p.packetWorkers.Add(2)
		go e.readPackets()
		go e.expireInitials()
	}
	c := &profilePacketConn{profile: v, endpoint: e, quic: quic, queue: make(chan profileDatagram, 64), done: make(chan struct{}), deadlineChanged: make(chan struct{})}
	v.packets[c] = struct{}{}
	e.listeners[c] = struct{}{}
	if !v.staged {
		e.active = c
	}
	p.mu.Unlock()
	return c, nil
}

func (r *profilePacketInitial) addRemoteLocked(remote net.Addr) {
	if !r.pending || remote == nil || len(r.remotes) >= 4 {
		return
	}
	key := remote.String()
	if _, exists := r.remotes[key]; exists {
		return
	}
	r.remotes[key] = struct{}{}
	v := r.owner.profile
	if v.quicPending[key] == nil {
		v.quicPending[key] = make(map[*profilePacketInitial]struct{})
	}
	v.quicPending[key][r] = struct{}{}
}

// Caller decides when to run finishLocked: a connection claim replaces the
// pending pin with a real connection in one critical section.
func (r *profilePacketInitial) clearPendingLocked() {
	if !r.pending {
		return
	}
	r.pending = false
	v := r.owner.profile
	v.packetConnections--
	for key := range r.remotes {
		delete(v.quicPending[key], r)
		if len(v.quicPending[key]) == 0 {
			delete(v.quicPending, key)
		}
	}
	r.remotes = nil
}

func (e *profilePacketEndpoint) deleteInitialLocked(r *profilePacketInitial) {
	if e.initial[r.key] != r {
		return
	}
	delete(e.initial, r.key)
	e.pool.packetPendingRoutes--
	r.clearPendingLocked()
	r.owner.profile.finishLocked()
}

func packetInitialKey(packet []byte) string {
	if len(packet) < 6 || packet[0]&0xc0 != 0xc0 || packet[1]|packet[2]|packet[3]|packet[4] == 0 {
		return ""
	}
	length := int(packet[5])
	if length == 0 || length > 20 || len(packet) < 6+length {
		return ""
	}
	return string(packet[1:5]) + string(packet[6:6+length])
}

func (e *profilePacketEndpoint) targetLocked(packet []byte, remote net.Addr) *profilePacketConn {
	session := ""
	if e.sessionKey != nil {
		session = e.sessionKey(packet)
		if session == "" || len(session) > 64 {
			return nil
		}
	}
	if owner := e.pool.profileQUICPacketOwnerLocked(packet); owner != nil {
		for c := range e.listeners {
			if c.profile == owner && c.quic && !c.closed {
				return c
			}
		}
	}
	if previous := e.existingPacketPeerLocked(remote, session); previous != nil {
		return previous.owner
	}
	key := packetInitialKey(packet)
	if key != "" {
		if previous := e.initial[key]; previous != nil {
			if time.Now().Before(previous.expires) && !previous.owner.closed {
				previous.addRemoteLocked(remote)
				return previous.owner
			}
			e.deleteInitialLocked(previous)
		}
	}
	c := e.active
	if c == nil || c.closed || c.profile.closed || c.profile.staged || c.profile.retired {
		return nil
	}
	if !c.quic {
		return e.newPacketPeerLocked(c, remote, session)
	}
	if key != "" {
		if e.pool.packetPendingRoutes >= profilePacketPendingLimit {
			return nil
		}
		r := &profilePacketInitial{owner: c, key: key, expires: time.Now().Add(10 * time.Second), pending: true, remotes: make(map[string]struct{})}
		e.initial[key] = r
		e.pool.packetPendingRoutes++
		c.profile.packetConnections++
		r.addRemoteLocked(remote)
	}
	return c
}

func (e *profilePacketEndpoint) readPackets() {
	defer e.pool.packetWorkers.Done()
	packet := make([]byte, 65535)
	for {
		n, remote, err := e.physical.ReadFrom(packet)
		if err != nil {
			e.pool.mu.Lock()
			e.failed = true
			listeners := make([]*profilePacketConn, 0, len(e.listeners))
			for c := range e.listeners {
				listeners = append(listeners, c)
			}
			e.pool.mu.Unlock()
			for _, c := range listeners {
				_ = c.Close()
			}
			return
		}
		if n <= 0 || n > len(packet) || remote == nil {
			continue
		}
		p := e.pool
		p.mu.Lock()
		if !p.closed && !e.failed && p.packetQueuedBytes+n <= profilePacketQueueBytes {
			if target := e.targetLocked(packet[:n], remote); target != nil && len(target.queue) < cap(target.queue) {
				if udp, ok := remote.(*net.UDPAddr); ok {
					copyAddr := *udp
					copyAddr.IP = append(net.IP(nil), udp.IP...)
					remote = &copyAddr
				}
				select {
				case target.queue <- profileDatagram{data: append([]byte(nil), packet[:n]...), remote: remote}:
					p.packetQueuedBytes += n
				default:
				}
			}
		}
		p.mu.Unlock()
	}
}

func (e *profilePacketEndpoint) expireInitials() {
	defer e.pool.packetWorkers.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case now := <-ticker.C:
			e.pool.mu.Lock()
			for _, r := range e.initial {
				if !now.Before(r.expires) {
					e.deleteInitialLocked(r)
				}
			}
			for _, peer := range e.peers {
				if peer.references == 0 && !now.Before(peer.expires) {
					e.deletePacketPeerLocked(peer)
				}
			}
			e.pool.mu.Unlock()
		}
	}
}

func (e *profilePacketEndpoint) closePhysical() {
	e.closeOnce.Do(func() { e.stopPool(); e.cancel(); _ = e.physical.Close() })
}

func (c *profilePacketConn) Close() error {
	p := c.profile.pool
	p.mu.Lock()
	if c.closed {
		p.mu.Unlock()
		return nil
	}
	c.closed = true
	close(c.done)
	delete(c.profile.packets, c)
	delete(c.endpoint.listeners, c)
	if c.endpoint.active == c {
		c.endpoint.active = nil
	}
	for _, r := range c.endpoint.initial {
		if r.owner == c {
			c.endpoint.deleteInitialLocked(r)
		}
	}
	for _, peer := range c.endpoint.peers {
		if peer.owner == c {
			c.endpoint.deletePacketPeerLocked(peer)
		}
	}
	for draining := true; draining; {
		select {
		case packet := <-c.queue:
			p.packetQueuedBytes -= len(packet.data)
		default:
			draining = false
		}
	}
	last := len(c.endpoint.listeners) == 0
	if last {
		c.endpoint.failed = true
		delete(p.packetEndpoints, c.endpoint.key)
	}
	c.profile.finishLocked()
	p.mu.Unlock()
	if last {
		c.endpoint.closePhysical()
	}
	return nil
}

func (c *profilePacketConn) LocalAddr() net.Addr { return c.endpoint.physical.LocalAddr() }

func (c *profilePacketConn) ReadFrom(dst []byte) (int, net.Addr, error) {
	for {
		p := c.profile.pool
		p.mu.Lock()
		closed, deadline, changed := c.closed, c.readDeadline, c.deadlineChanged
		p.mu.Unlock()
		if closed {
			return 0, nil, net.ErrClosed
		}
		var timer *time.Timer
		var expired <-chan time.Time
		if !deadline.IsZero() {
			if !time.Now().Before(deadline) {
				return 0, nil, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(time.Until(deadline))
			expired = timer.C
		}
		select {
		case packet := <-c.queue:
			if timer != nil {
				timer.Stop()
			}
			p.mu.Lock()
			p.packetQueuedBytes -= len(packet.data)
			closed = c.closed
			p.mu.Unlock()
			if closed {
				return 0, nil, net.ErrClosed
			}
			return copy(dst, packet.data), packet.remote, nil
		case <-c.done:
			if timer != nil {
				timer.Stop()
			}
			return 0, nil, net.ErrClosed
		case <-changed:
			if timer != nil {
				timer.Stop()
			}
		case <-expired:
			p.mu.Lock()
			sameDeadline := c.readDeadline.Equal(deadline)
			p.mu.Unlock()
			if sameDeadline {
				return 0, nil, os.ErrDeadlineExceeded
			}
		}
	}
}

func (c *profilePacketConn) WriteTo(packet []byte, remote net.Addr) (int, error) {
	p := c.profile.pool
	p.mu.Lock()
	closed, deadline := c.closed, c.writeDeadline
	p.mu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	maximum := time.Now().Add(15 * time.Second)
	if deadline.IsZero() || deadline.After(maximum) {
		deadline = maximum
	}
	if !time.Now().Before(deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case c.endpoint.writes <- struct{}{}:
	case <-c.done:
		return 0, net.ErrClosed
	case <-timer.C:
		return 0, os.ErrDeadlineExceeded
	}
	defer func() { <-c.endpoint.writes }()
	p.mu.Lock()
	closed = c.closed
	p.mu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	if err := c.endpoint.physical.SetWriteDeadline(deadline); err != nil {
		return 0, err
	}
	n, err := c.endpoint.physical.WriteTo(packet, remote)
	_ = c.endpoint.physical.SetWriteDeadline(time.Time{})
	return n, err
}

func (c *profilePacketConn) SetDeadline(deadline time.Time) error {
	p := c.profile.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	c.readDeadline, c.writeDeadline = deadline, deadline
	close(c.deadlineChanged)
	c.deadlineChanged = make(chan struct{})
	return nil
}
func (c *profilePacketConn) SetReadDeadline(deadline time.Time) error {
	p := c.profile.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	c.readDeadline = deadline
	close(c.deadlineChanged)
	c.deadlineChanged = make(chan struct{})
	return nil
}
func (c *profilePacketConn) SetWriteDeadline(deadline time.Time) error {
	p := c.profile.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	c.writeDeadline = deadline
	return nil
}
