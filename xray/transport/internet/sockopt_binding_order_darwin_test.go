package internet

import (
	"errors"
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOutboundBindingPrecedesTCPOptions(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			err := applyOutboundSocketOptions(network, "127.0.0.1:1", ^uintptr(0), &SocketConfig{Tfo: 1, Interface: "xray_missing0"})
			var bindErr *BindError
			if !errors.As(err, &bindErr) {
				t.Fatalf("TCP options bypassed mandatory interface binding: %v", err)
			}
			err = applyOutboundSocketOptions(network, "127.0.0.1:1", ^uintptr(0), &SocketConfig{Tfo: 1})
			if err == nil || errors.As(err, &bindErr) {
				t.Fatalf("TFO-only failure lost its best-effort classification: %v", err)
			}
		})
	}
}

func checkBindingOrderSocketInterface(t *testing.T, fd int, iface *net.Interface) {
	t.Helper()
	actual, err := unix.GetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_BOUND_IF)
	if err != nil || actual != iface.Index {
		t.Fatalf("best-effort error left an unbound socket: index=%d, error=%v", actual, err)
	}
}
