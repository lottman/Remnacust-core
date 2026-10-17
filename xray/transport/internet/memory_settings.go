package internet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"sync"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/net/cnc"
	"github.com/xtls/xray-core/transport/internet/finalmask"
	"google.golang.org/protobuf/proto"
)

// MemoryStreamConfig is a parsed form of StreamConfig. It is used to reduce the number of Protobuf parses.
type MemoryStreamConfig struct {
	closeAccess      sync.Mutex
	closed           bool
	closeHooks       []func()
	Destination      *net.Destination
	ProtocolName     string
	ProtocolSettings interface{}
	SecurityType     string
	SecuritySettings interface{}
	FinalMask        *finalmask.FinalMask
	PacketMaskID     string
	QuicParams       *QuicParams
	SocketSettings   *SocketConfig
	DownloadSettings *MemoryStreamConfig
}

// Transport pools belong to the outbound configuration that created them.
// Generation retirement closes them only after its last stream has drained.
func (s *MemoryStreamConfig) AddCloseHook(hook func()) bool {
	s.closeAccess.Lock()
	defer s.closeAccess.Unlock()
	if s.closed {
		return false
	}
	s.closeHooks = append(s.closeHooks, hook)
	return true
}

func (s *MemoryStreamConfig) IsClosed() bool {
	if s == nil {
		return false
	}
	s.closeAccess.Lock()
	defer s.closeAccess.Unlock()
	return s.closed
}

func (s *MemoryStreamConfig) Close() error {
	if s == nil {
		return nil
	}
	s.closeAccess.Lock()
	if s.closed {
		s.closeAccess.Unlock()
		return nil
	}
	s.closed = true
	hooks := s.closeHooks
	s.closeHooks = nil
	s.closeAccess.Unlock()
	for _, hook := range hooks {
		hook()
	}
	if s.DownloadSettings != nil {
		_ = s.DownloadSettings.Close()
	}
	return nil
}

// ToMemoryStreamConfig converts a StreamConfig to MemoryStreamConfig. It returns a default non-nil MemoryStreamConfig for nil input.
func ToMemoryStreamConfig(s *StreamConfig) (*MemoryStreamConfig, error) {
	ets, err := s.GetEffectiveTransportSettings()
	if err != nil {
		return nil, err
	}

	mss := &MemoryStreamConfig{
		ProtocolName:     s.GetEffectiveProtocol(),
		ProtocolSettings: ets,
	}

	if s != nil {
		if s.Address != nil {
			mss.Destination = &net.Destination{
				Address: s.Address.AsAddress(),
				Port:    net.Port(s.Port),
				Network: net.Network_TCP,
			}
		}
		mss.SocketSettings = s.SocketSettings
	}

	if s != nil && s.HasSecuritySettings() {
		ess, err := s.GetEffectiveSecuritySettings()
		if err != nil {
			return nil, err
		}
		mss.SecurityType = s.SecurityType
		mss.SecuritySettings = ess
	}
	if s != nil && (len(s.Tcpmasks) != 0 || len(s.Udpmasks) != 0) {
		mss.PacketMaskID = "none"
		if len(s.Udpmasks) != 0 {
			encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(&StreamConfig{Udpmasks: s.Udpmasks})
			if err != nil {
				return nil, err
			}
			digest := sha256.Sum256(encoded)
			mss.PacketMaskID = hex.EncodeToString(digest[:])
		}
		var tcpMasks []finalmask.TCPMask
		var udpMasks []finalmask.UDPMask

		for i := range s.Tcpmasks {
			instance := common.Must2(s.Tcpmasks[i].GetInstance())
			tcpMasks = append(tcpMasks, instance.(finalmask.TCPMask))
		}
		for i := range s.Udpmasks {
			instance := common.Must2(s.Udpmasks[i].GetInstance())
			udpMasks = append(udpMasks, instance.(finalmask.UDPMask))
		}

		dialTCP := func(ctx context.Context, dest net.Destination) (net.Conn, error) {
			return DialSystem(ctx, dest, mss.SocketSettings)
		}
		listen := func(ctx context.Context, addr net.Addr) (net.Listener, error) {
			return ListenSystem(ctx, addr, mss.SocketSettings)
		}
		dialUDP := func(ctx context.Context, dest net.Destination) (net.PacketConn, net.Addr, error) {
			conn, err := DialSystem(ctx, dest, mss.SocketSettings)
			if err != nil {
				return nil, nil, err
			}
			var newConn net.PacketConn
			var udpAddr net.Addr
			switch c := conn.(type) {
			case *net.PacketConnWrapper:
				newConn = c.PacketConn
				udpAddr = conn.RemoteAddr()
			case *cnc.Connection:
				newConn = &FakePacketConn{Conn: c}
				udpAddr = &net.UDPAddr{IP: []byte{0, 0, 0, 0}, Port: 0}
			default:
				panic(reflect.TypeOf(c))
			}
			return newConn, udpAddr, nil
		}
		listenPacket := func(ctx context.Context, addr net.Addr) (net.PacketConn, error) {
			return ListenSystemPacket(ctx, addr, mss.SocketSettings)
		}
		mss.FinalMask = finalmask.NewFinalMask(tcpMasks, udpMasks, dialTCP, listen, dialUDP, listenPacket)
	}

	if s != nil && s.QuicParams != nil {
		mss.QuicParams = s.QuicParams
	}

	return mss, nil
}
