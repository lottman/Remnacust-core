//go:build !wasm

package dispatcher

import (
	"context"
	"time"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
)

// CheckUserPacket applies the same authenticated policy to packet paths that
// do not create a dispatched stream, including MASQUE traffic between tunnels.
func CheckUserPacket(ctx context.Context, tag, email string, target net.Address, direction string, size int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	activeUserLinks.Lock()
	until, revoked := activeUserLinks.revoked[email]
	activeUserLinks.Unlock()
	if revoked && time.Now().Before(until) {
		return errors.New("user access revoked")
	}
	ctx = session.ContextWithInbound(ctx, &session.Inbound{Tag: tag})
	ctx = session.ContextWithOutbounds(ctx, []*session.Outbound{{Target: net.UDPDestination(target, 0)}})
	if err := waitHostPolicy(ctx, email, direction, size); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	activeUserLinks.Lock()
	until, revoked = activeUserLinks.revoked[email]
	activeUserLinks.Unlock()
	if revoked && time.Now().Before(until) {
		return errors.New("user access revoked")
	}
	return nil
}
