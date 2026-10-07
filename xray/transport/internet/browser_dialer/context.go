package browser_dialer

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gorilla/websocket"
)

func DialGetContext(ctx context.Context, uri string, headers http.Header, cookies []*http.Cookie) (*websocket.Conn, error) {
	return dialTaskContext(ctx, task{Method: "GET", URL: uri, Extra: contextHTTPExtra(headers, cookies), StreamResponse: true})
}

func DialPacketContext(ctx context.Context, method, uri string, headers http.Header, cookies []*http.Cookie, payload []byte) error {
	conn, err := dialTaskContext(ctx, task{Method: method, URL: uri, Extra: contextHTTPExtra(headers, cookies)})
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err = conn.WriteMessage(websocket.BinaryMessage, payload); err == nil {
		err = CheckOK(conn)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func dialTaskContext(ctx context.Context, task task) (*websocket.Conn, error) {
	data, err := json.Marshal(task)
	if err != nil {
		return nil, err
	}
	mu.Lock()
	available := conns
	mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var conn *websocket.Conn
		select {
		case conn = <-available:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			stop()
			conn.Close()
			continue
		}
		err := CheckOK(conn)
		stop()
		if ctx.Err() != nil {
			conn.Close()
			return nil, ctx.Err()
		}
		if err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}
}

func contextHTTPExtra(headers http.Header, cookies []*http.Cookie) *httpExtra {
	extra := httpExtraFromHeadersAndCookies(headers.Clone(), cookies)
	if extra == nil && len(cookies) > 0 {
		extra = &httpExtra{Cookies: make(map[string]string, len(cookies))}
		for _, cookie := range cookies {
			extra.Cookies[cookie.Name] = cookie.Value
		}
	}
	return extra
}
