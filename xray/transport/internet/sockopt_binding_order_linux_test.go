package internet

import (
	"errors"
	"net"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOutboundBindingPrecedesMark(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6", "udp4", "udp6"} {
		t.Run(network, func(t *testing.T) {
			err := applyOutboundSocketOptions(network, "127.0.0.1:1", ^uintptr(0), &SocketConfig{Mark: 1, Interface: "xray_missing0"})
			var bindErr *BindError
			if !errors.As(err, &bindErr) {
				t.Fatalf("SO_MARK bypassed mandatory interface binding: %v", err)
			}
			err = applyOutboundSocketOptions(network, "127.0.0.1:1", ^uintptr(0), &SocketConfig{Mark: 1})
			if err == nil || errors.As(err, &bindErr) {
				t.Fatalf("mark-only failure lost its best-effort classification: %v", err)
			}
		})
	}
}

func checkBindingOrderSocketInterface(t *testing.T, fd int, iface *net.Interface) {
	t.Helper()
	actual, err := unix.GetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE)
	if err != nil || strings.TrimRight(actual, "\x00") != iface.Name {
		t.Fatalf("best-effort error left an unbound socket: interface=%q, error=%v", actual, err)
	}
}
