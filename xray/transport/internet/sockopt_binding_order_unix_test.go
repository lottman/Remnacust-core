//go:build linux || darwin

package internet

import (
	"context"
	"errors"
	"net"
	"runtime"
	"slices"
	"testing"
	"time"

	xnet "github.com/xtls/xray-core/common/net"
	"golang.org/x/sys/unix"
)

func TestSystemDialerMissingInterfaceFailsClosed(t *testing.T) {
	const missing = "xray_missing0"
	if _, err := net.InterfaceByName(missing); err == nil {
		t.Fatalf("test interface unexpectedly exists: %s", missing)
	}
	for _, network := range []string{"tcp", "udp"} {
		t.Run(network, func(t *testing.T) {
			options := &SocketConfig{Interface: missing}
			if runtime.GOOS == "linux" {
				options.Mark = 1
			} else {
				options.Tfo = 1
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var dest xnet.Destination
			var tcp *net.TCPListener
			var udp *net.UDPConn
			if network == "tcp" {
				var err error
				tcp, err = net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if err != nil {
					t.Fatal(err)
				}
				defer tcp.Close()
				dest = xnet.DestinationFromAddr(tcp.Addr())
			} else {
				var err error
				udp, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if err != nil {
					t.Fatal(err)
				}
				defer udp.Close()
				dest = xnet.DestinationFromAddr(udp.LocalAddr())
			}
			var dialer DefaultSystemDialer
			conn, err := dialer.Dial(ctx, nil, dest, options)
			if conn != nil {
				if network == "udp" {
					conn.Write([]byte("unexpected unbound local datagram"))
				}
				conn.Close()
				t.Error("mandatory interface binding returned a usable connection")
			}
			var bindErr *BindError
			if !errors.As(err, &bindErr) {
				t.Errorf("missing-interface dial did not fail with BindError: %v", err)
			}
			if tcp != nil {
				if err := tcp.SetDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
					t.Fatal(err)
				}
				accepted, err := tcp.Accept()
				if accepted != nil {
					accepted.Close()
					t.Error("TCP reached the listener before mandatory binding failed")
				}
				var timeout net.Error
				if !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Errorf("unexpected local TCP accept result: %v", err)
				}
			} else {
				if err := udp.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
					t.Fatal(err)
				}
				var data [64]byte
				_, _, err := udp.ReadFromUDP(data[:])
				var timeout net.Error
				if !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Errorf("UDP reached the local listener or failed unexpectedly: %v", err)
				}
			}
		})
	}
}

func TestOutboundOptionalFailureRemainsBestEffort(t *testing.T) {
	for _, network := range []string{"tcp4", "udp4"} {
		err := applyOutboundSocketOptions(network, "127.0.0.1:1", ^uintptr(0), &SocketConfig{CustomSockopt: []*CustomSockopt{{}}})
		var bindErr *BindError
		if err == nil || errors.As(err, &bindErr) {
			t.Errorf("optional socket option became a mandatory bind failure: %v", err)
		}
	}
}

func TestSystemDialerOptionalFailureAfterBinding(t *testing.T) {
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	var loopback *net.Interface
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 && iface.Flags&net.FlagUp != 0 {
			loopback = &iface
			break
		}
	}
	if loopback == nil {
		t.Fatal("no active loopback interface")
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	options := &SocketConfig{Interface: loopback.Name, CustomSockopt: []*CustomSockopt{{}}}
	err = applyOutboundSocketOptions("udp4", "127.0.0.1:1", uintptr(fd), options)
	var bindErr *BindError
	if errors.As(err, &bindErr) && (errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES)) {
		t.Skipf("loopback interface binding is not permitted: %v", err)
	}
	if err == nil || errors.As(err, &bindErr) {
		t.Fatalf("expected a best-effort option error after successful binding: %v", err)
	}
	checkBindingOrderSocketInterface(t, fd, loopback)
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	deadline := time.Now().Add(time.Second)
	if err := server.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	var dialer DefaultSystemDialer
	conn, err := dialer.Dial(ctx, nil, xnet.DestinationFromAddr(server.LocalAddr()), options)
	if err != nil {
		t.Fatalf("best-effort option failure rejected a bound socket: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	payload := []byte("bound UDP remains usable after an optional error")
	if n, err := conn.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("bound UDP write: n=%d, error=%v", n, err)
	}
	var data [64]byte
	n, source, err := server.ReadFromUDP(data[:])
	if err != nil || !slices.Equal(data[:n], payload) {
		t.Fatalf("bound UDP did not reach the local listener: %v", err)
	}
	if n, err := server.WriteToUDP(data[:n], source); err != nil || n != len(payload) {
		t.Fatalf("local UDP echo: n=%d, error=%v", n, err)
	}
	n, err = conn.Read(data[:])
	if err != nil || !slices.Equal(data[:n], payload) {
		t.Fatalf("bound UDP did not receive its local echo: %v", err)
	}
}
