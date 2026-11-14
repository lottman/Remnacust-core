package xerahttp

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet/browser_dialer"
	"github.com/xtls/xray-core/transport/internet/websocket"
)

type BrowserDialerClient struct {
	transportConfig *Config
}

func validateBrowserConfig(config *Config) error {
	if config.CustomDownlinkPadding != nil {
		return errors.New("custom downlink padding is not supported by Browser Dialer")
	}
	if config.GetNormalizedSessionPlacement() == PlacementCookie || config.GetNormalizedSeqPlacement() == PlacementCookie || config.GetNormalizedXPaddingPlacement() == PlacementCookie || config.GetNormalizedUplinkDataPlacement() == PlacementCookie {
		return errors.New("Browser Dialer cannot set transport cookies on another origin; use path, query, headers, or body")
	}
	for name := range config.Headers {
		if strings.EqualFold(name, "Cookie") {
			return errors.New("Browser Dialer cannot set an explicit Cookie header")
		}
	}
	switch strings.ToUpper(config.GetNormalizedUplinkHTTPMethod()) {
	case "CONNECT", "TRACE", "TRACK":
		return errors.New("uplink HTTP method is forbidden by browser Fetch")
	case "GET", "HEAD":
		placement := config.GetNormalizedUplinkDataPlacement()
		if placement == PlacementBody || placement == PlacementAuto {
			return errors.New("browser Fetch cannot send a body with GET or HEAD; use POST or header placement")
		}
	}
	return nil
}

func (c *BrowserDialerClient) IsClosed() bool {
	return false
}

func (c *BrowserDialerClient) OpenStream(ctx context.Context, url string, sessionId string, body io.Reader, uploadOnly bool) (io.ReadCloser, net.Addr, net.Addr, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	if err := validateBrowserConfig(c.transportConfig); err != nil {
		return nil, nil, nil, err
	}
	if body != nil {
		return nil, nil, nil, errors.New("bidirectional streaming for browser dialer not implemented yet")
	}

	request, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, nil, nil, err
	}

	c.transportConfig.FillStreamRequest(request, sessionId, "")

	conn, err := browser_dialer.DialGetContext(ctx, request.URL.String(), request.Header, request.Cookies())
	dummyAddr := &net.IPAddr{}
	if err != nil {
		return nil, dummyAddr, dummyAddr, err
	}

	return websocket.NewConnection(conn, dummyAddr, nil, 0), conn.RemoteAddr(), conn.LocalAddr(), nil
}

func (c *BrowserDialerClient) PostPacket(ctx context.Context, url string, sessionId string, seqStr string, payload buf.MultiBuffer) error {
	if err := ctx.Err(); err != nil {
		buf.ReleaseMulti(payload)
		return err
	}
	if err := validateBrowserConfig(c.transportConfig); err != nil {
		buf.ReleaseMulti(payload)
		return err
	}
	method := c.transportConfig.GetNormalizedUplinkHTTPMethod()
	request, err := http.NewRequest(method, url, nil)
	if err != nil {
		buf.ReleaseMulti(payload)
		return err
	}

	err = c.transportConfig.FillPacketRequest(request, sessionId, seqStr, payload)
	if err != nil {
		return err
	}

	var bytes []byte
	if request.Body != nil {
		defer request.Body.Close()
		bytes, err = io.ReadAll(request.Body)
		if err != nil {
			return err
		}
	}

	err = browser_dialer.DialPacketContext(ctx, method, request.URL.String(), request.Header, request.Cookies(), bytes)
	if err != nil {
		return err
	}

	return nil
}
