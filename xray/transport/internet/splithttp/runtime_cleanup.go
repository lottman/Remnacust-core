package splithttp

import (
	"context"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"io"
	stdnet "net"
)

type retiredDialer struct{}

func (*retiredDialer) IsClosed() bool { return true }
func (*retiredDialer) OpenStream(context.Context, string, string, io.Reader, bool) (io.ReadCloser, net.Addr, net.Addr, error) {
	return nil, nil, nil, stdnet.ErrClosed
}
func (*retiredDialer) PostPacket(_ context.Context, _, _, _ string, payload buf.MultiBuffer) error {
	buf.ReleaseMulti(payload)
	return stdnet.ErrClosed
}
