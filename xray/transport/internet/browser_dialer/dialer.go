package browser_dialer

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/platform"
	"github.com/xtls/xray-core/common/uuid"
)

//go:embed dialer.html
var webpage []byte

type task struct {
	Method         string `json:"method"`
	URL            string `json:"url"`
	Extra          any    `json:"extra,omitempty"`
	StreamResponse bool   `json:"streamResponse"`
}

var (
	conns       chan *websocket.Conn
	server      *http.Server
	currentAddr string
	mu          sync.Mutex
)

// Used by external projects when using xray as a go module
func Reload() {
	addr := platform.NewEnvFlag(platform.BrowserDialerAddress).GetValue(func() string { return "" })
	mu.Lock()
	defer mu.Unlock()

	if addr == currentAddr && (addr == "" || server != nil) {
		return
	}

	if server != nil {
		server.Close()
		server = nil
	}
	if conns != nil {
		for len(conns) > 0 {
			select {
			case c := <-conns:
				c.Close()
			default:
			}
		}
		conns = nil
	}
	currentAddr = addr
	if addr != "" {
		token := uuid.New()
		csrfToken := token.String()
		webpage := bytes.ReplaceAll(webpage, []byte("csrfToken"), []byte(csrfToken))
		conns = make(chan *websocket.Conn, 256)
		server = &http.Server{Addr: addr, Handler: newBrowserHandler(addr, csrfToken, webpage, conns),
			ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
		go server.ListenAndServe()
	}
}

func HasBrowserDialer() bool {
	mu.Lock()
	defer mu.Unlock()
	return conns != nil
}

type webSocketExtra struct {
	Protocol string `json:"protocol,omitempty"`
}

func DialWS(uri string, ed []byte) (*websocket.Conn, error) {
	task := task{
		Method:         "WS",
		URL:            uri,
		StreamResponse: true,
	}

	task.Extra = webSocketExtra{
		Protocol: base64.RawURLEncoding.EncodeToString(ed),
	}

	return dialTask(task)
}

type httpExtra struct {
	Referrer string            `json:"referrer,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Cookies  map[string]string `json:"cookies,omitempty"`
}

func httpExtraFromHeadersAndCookies(headers http.Header, cookies []*http.Cookie) *httpExtra {
	if len(headers) == 0 {
		return nil
	}

	extra := httpExtra{}
	if referrer := headers.Get("Referer"); referrer != "" {
		extra.Referrer = referrer
		headers.Del("Referer")
	}

	if len(headers) > 0 {
		extra.Headers = make(map[string]string)
		for header := range headers {
			extra.Headers[header] = headers.Get(header)
		}
	}

	if len(cookies) > 0 {
		extra.Cookies = make(map[string]string)
		for _, cookie := range cookies {
			extra.Cookies[cookie.Name] = cookie.Value
		}
	}

	return &extra
}

func DialGet(uri string, headers http.Header, cookies []*http.Cookie) (*websocket.Conn, error) {
	task := task{
		Method:         "GET",
		URL:            uri,
		Extra:          httpExtraFromHeadersAndCookies(headers, cookies),
		StreamResponse: true,
	}

	return dialTask(task)
}

func DialPacket(method string, uri string, headers http.Header, cookies []*http.Cookie, payload []byte) error {
	return dialWithBody(method, uri, headers, cookies, payload)
}

func dialWithBody(method string, uri string, headers http.Header, cookies []*http.Cookie, payload []byte) error {
	task := task{
		Method:         method,
		URL:            uri,
		Extra:          httpExtraFromHeadersAndCookies(headers, cookies),
		StreamResponse: false,
	}

	conn, err := dialTask(task)
	if err != nil {
		return err
	}

	err = conn.WriteMessage(websocket.BinaryMessage, payload)
	if err != nil {
		return err
	}

	err = CheckOK(conn)
	if err != nil {
		return err
	}

	conn.Close()
	return nil
}

func dialTask(task task) (*websocket.Conn, error) {
	data, err := json.Marshal(task)
	if err != nil {
		return nil, err
	}

	mu.Lock()
	available := conns
	mu.Unlock()
	var conn *websocket.Conn
	for {
		conn = <-available
		if conn.WriteMessage(websocket.TextMessage, data) != nil {
			conn.Close()
		} else {
			break
		}
	}
	err = CheckOK(conn)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

func CheckOK(conn *websocket.Conn) error {
	if _, p, err := conn.ReadMessage(); err != nil {
		conn.Close()
		return err
	} else if s := string(p); s != "ok" {
		conn.Close()
		return errors.New(s)
	}

	return nil
}

func init() {
	platform.RegisterEnvReload(func() error {
		Reload()
		return nil
	})
}
