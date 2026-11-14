package browser_dialer

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func browserRequestAllowed(r *http.Request, address string) bool {
	boundHost, boundPort, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, "80"
		if r.TLS != nil {
			port = "443"
		}
	}
	if port != boundPort {
		return false
	}
	boundIP, hostIP := net.ParseIP(boundHost), net.ParseIP(host)
	if boundHost == "" || boundIP != nil && boundIP.IsUnspecified() {
		local, ok := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
		if !ok || !(hostIP != nil && hostIP.Equal(local.IP) || strings.EqualFold(host, "localhost") && local.IP.IsLoopback()) {
			return false
		}
	} else if strings.EqualFold(boundHost, "localhost") || boundIP != nil && boundIP.IsLoopback() {
		if !strings.EqualFold(host, "localhost") && (hostIP == nil || !hostIP.IsLoopback()) {
			return false
		}
	} else if !strings.EqualFold(boundHost, host) && (boundIP == nil || !boundIP.Equal(hostIP)) {
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	origin, err := url.Parse(origins[0])
	if err != nil || origin.User != nil || origin.Opaque != "" || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || strings.Contains(origins[0], "#") {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	originPort := origin.Port()
	if originPort == "" {
		originPort = "80"
		if origin.Scheme == "https" {
			originPort = "443"
		}
	}
	originHost := origin.Hostname()
	originIP := net.ParseIP(originHost)
	return origin.Scheme == scheme && originPort == port && (strings.EqualFold(originHost, host) || originIP != nil && originIP.Equal(hostIP))
}

func newBrowserHandler(address, token string, page []byte, available chan *websocket.Conn) http.Handler {
	upgrader := websocket.Upgrader{
		HandshakeTimeout: 4 * time.Second,
		CheckOrigin:      func(r *http.Request) bool { return browserRequestAllowed(r, address) },
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !browserRequestAllowed(r, address) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/websocket" {
			values := r.URL.Query()["token"]
			if len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), []byte(token)) != 1 {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			select {
			case available <- conn:
			default:
				conn.Close()
			}
			return
		}
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		w.Write(page)
	})
}
