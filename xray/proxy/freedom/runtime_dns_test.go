package freedom

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/transport/internet"
)

type packetScope struct{ calls atomic.Int32 }

func (s *packetScope) DialerDNSClient() dns.Client           { return s }
func (*packetScope) DialerOutboundManager() outbound.Manager { return nil }
func (*packetScope) Type() interface{}                       { return dns.ClientType() }
func (*packetScope) Start() error                            { return nil }
func (*packetScope) Close() error                            { return nil }
func (s *packetScope) LookupIP(domain string, _ dns.IPOption) ([]net.IP, uint32, error) {
	s.calls.Add(1)
	return []net.IP{net.LocalHostIP.IP()}, 60, nil
}

func TestPacketWriterUsesAndCachesOwningProfileDNS(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.LocalHostIP.IP()})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := server.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.LocalHostIP.IP()})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	scope := new(packetScope)
	ctx := internet.ContextWithDialerFeatures(context.Background(), scope)
	port := net.Port(server.LocalAddr().(*net.UDPAddr).Port)
	writer := newPacketWriter(ctx, &net.PacketConnWrapper{PacketConn: conn}, &Handler{resolveStrategy: internet.DomainStrategy_FORCE_IP4}, nil, net.Destination{}, net.UDPDestination(net.LocalHostIP, port), nil)
	for range 2 {
		packet := buf.New()
		packet.WriteString("profile UDP")
		destination := net.UDPDestination(net.DomainAddress("profile.invalid"), port)
		packet.UDP = &destination
		if err := writer.WriteMultiBuffer(buf.MultiBuffer{packet}); err != nil {
			t.Fatal(err)
		}
		var data [32]byte
		n, _, err := server.ReadFrom(data[:])
		if err != nil || string(data[:n]) != "profile UDP" {
			t.Fatalf("UDP did not reach profile target: data=%q err=%v", data[:n], err)
		}
	}
	if scope.calls.Load() != 1 {
		t.Fatalf("UDP DNS cache was not scoped: queries=%d", scope.calls.Load())
	}
}
