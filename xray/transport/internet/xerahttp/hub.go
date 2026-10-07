package xerahttp

import (
	"bytes"
	"context"
	"crypto/rand"
	gotls "crypto/tls"
	"encoding/base64"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/http3"
	goreality "github.com/xtls/reality"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	http_proto "github.com/xtls/xray-core/common/protocol/http"
	"github.com/xtls/xray-core/common/signal/done"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/hysteria/congestion"
	"github.com/xtls/xray-core/transport/internet/hysteria/congestion/bbr"
	"github.com/xtls/xray-core/transport/internet/reality"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/internet/tls"
)

type requestHandler struct {
	config          *Config
	host            string
	path            string
	ln              *Listener
	sessionMu       *sync.Mutex
	sessions        sync.Map
	localAddr       net.Addr
	socketSettings  *internet.SocketConfig
	budget          payloadBudget
	sessionCount    atomic.Int64
	pendingRequests atomic.Int64
	closed          atomic.Bool
	stopped         *done.Instance
}

type httpSession struct {
	uploadQueue *uploadQueue

	isFullyConnected *done.Instance
}

func isValidRequestPath(requestPath, configuredPath string) bool {
	if configuredPath == "/" {
		return strings.HasPrefix(requestPath, "/")
	}
	if strings.HasSuffix(configuredPath, "/") {
		return strings.HasPrefix(requestPath, configuredPath)
	}
	return requestPath == configuredPath
}

func (h *requestHandler) upsertSession(sessionId string) *httpSession {
	if h.closed.Load() {
		return nil
	}

	currentSessionAny, ok := h.sessions.Load(sessionId)
	if ok {
		return currentSessionAny.(*httpSession)
	}

	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	if h.closed.Load() {
		return nil
	}

	currentSessionAny, ok = h.sessions.Load(sessionId)
	if ok {
		return currentSessionAny.(*httpSession)
	}
	if !acquireSlot(&h.sessionCount, h.config.serverSessionLimit()) {
		return nil
	}

	s := &httpSession{
		uploadQueue:      NewUploadQueue(h.ln.config.GetNormalizedScMaxBufferedPosts()),
		isFullyConnected: done.New(),
	}

	h.sessions.Store(sessionId, s)

	go func() {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			h.sessionMu.Lock()
			if !s.isFullyConnected.Done() && h.deleteSession(sessionId, s) {
				s.uploadQueue.Close()
			}
			h.sessionMu.Unlock()
		case <-s.isFullyConnected.Wait():
		case <-h.stopped.Wait():
		}
	}()

	return s
}

func (h *requestHandler) deleteSession(id string, session *httpSession) bool {
	if h.sessions.CompareAndDelete(id, session) {
		h.sessionCount.Add(-1)
		return true
	}
	return false
}

func (h *requestHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if len(h.host) > 0 && !internet.IsValidHTTPHost(request.Host, h.host) {
		errors.LogInfo(context.Background(), "failed to validate host, request:", request.Host, ", config:", h.host)
		writer.WriteHeader(http.StatusNotFound)
		return
	}

	if !isValidRequestPath(request.URL.Path, h.path) {
		errors.LogInfo(context.Background(), "failed to validate path, request:", request.URL.Path, ", config:", h.path)
		writer.WriteHeader(http.StatusNotFound)
		return
	}

	h.config.WriteResponseHeader(writer, request.Method, request.Header)
	length := int(h.config.GetNormalizedXPaddingBytes().rand())
	config := XPaddingConfig{Length: length}

	if h.config.XPaddingObfsMode {
		config.Placement = XPaddingPlacement{
			Placement: h.config.GetNormalizedXPaddingPlacement(),
			Key:       h.config.GetNormalizedXPaddingKey(),
			Header:    h.config.GetNormalizedXPaddingHeader(),
		}
		config.Method = h.config.GetNormalizedXPaddingMethod()
	} else {
		config.Placement = XPaddingPlacement{
			Placement: PlacementHeader,
			Header:    h.config.GetNormalizedXPaddingHeader(),
		}
	}

	if h.config.XPaddingObfsMode {
		h.config.ApplyXPaddingToResponse(writer, config)
	}

	if request.Method == "OPTIONS" {
		writer.WriteHeader(http.StatusOK)
		return
	}

	validRange := h.config.GetNormalizedXPaddingBytes()
	paddingValue, paddingPlacement := h.config.ExtractXPaddingFromRequest(request, h.config.XPaddingObfsMode)

	if !h.config.IsPaddingValid(paddingValue, validRange.From, validRange.To, h.config.GetNormalizedXPaddingMethod()) {
		errors.LogInfo(context.Background(), "invalid padding ("+paddingPlacement+") length:", int32(len(paddingValue)))
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	obfsPaddingAccepted := h.config.XPaddingObfsMode && paddingValue != ""
	customPadding := h.config.CustomDownlinkPadding
	useCustomPadding := false
	if customPadding != nil {
		value := request.Header.Get(customPadding.Header)
		if value != "" && value != customPadding.wireValue() {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		useCustomPadding = value != ""
	}
	useCustomUplink := useCustomPadding && customPadding.Uplink
	if useCustomPadding {
		writer.Header().Set(customPadding.Header, customPadding.wireValue())
	}

	sessionId, seqStr := h.config.ExtractMetaFromRequest(request, h.path)

	if sessionId == "" && !isModeAllowed(h.config.Mode, ModeStreamOne) && !isModeAllowed(h.config.Mode, ModeStreamUp) {
		errors.LogInfo(context.Background(), "stream-one mode is not allowed")
		writer.WriteHeader(http.StatusBadRequest)
		return
	}

	var remoteAddr net.Addr
	var err error
	remoteAddr, err = net.ResolveTCPAddr("tcp", request.RemoteAddr)
	if err != nil {
		remoteAddr = &net.TCPAddr{
			IP:   []byte{0, 0, 0, 0},
			Port: 0,
		}
	}
	if request.ProtoMajor == 3 {
		remoteAddr = &net.UDPAddr{
			IP:   remoteAddr.(*net.TCPAddr).IP,
			Port: remoteAddr.(*net.TCPAddr).Port,
		}
	}
	var trustedXFF []string
	if h.socketSettings != nil {
		trustedXFF = h.socketSettings.TrustedXForwardedFor
	}
	remoteAddr = http_proto.ApplyTrustedXForwardedFor(request.Header, trustedXFF, remoteAddr)

	var currentSession *httpSession
	if sessionId != "" {
		currentSession = h.upsertSession(sessionId)
		if currentSession == nil {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}
	scMaxEachPostBytes := int(h.ln.config.GetNormalizedScMaxEachPostBytes().To)
	isUplinkRequest := false

	switch request.Method {
	case "GET":
		isUplinkRequest = seqStr != ""
	default:
		isUplinkRequest = true
	}

	uplinkDataKey := h.config.UplinkDataKey

	if isUplinkRequest && sessionId != "" {
		if seqStr == "" {
			if !isModeAllowed(h.config.Mode, ModeStreamUp) {
				errors.LogInfo(context.Background(), "stream-up mode is not allowed")
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			if useCustomUplink {
				request.Body = &paddedDownlinkReader{ReadCloser: request.Body}
			}
			httpSC := &httpServerConn{
				Instance:       done.New(),
				Reader:         request.Body,
				ResponseWriter: writer,
			}
			err = currentSession.uploadQueue.PushContext(request.Context(), Packet{
				Reader: httpSC,
			})
			if err != nil {
				errors.LogInfoInner(context.Background(), err, "failed to upload (PushReader)")
				writer.WriteHeader(http.StatusConflict)
			} else {
				writer.Header().Set("Cache-Control", "no-store")
				writer.WriteHeader(http.StatusOK)
				scStreamUpServerSecs := h.config.GetNormalizedScStreamUpServerSecs()
				if obfsPaddingAccepted && scStreamUpServerSecs.To > 0 {
					go func() {
						for {
							_, err := httpSC.Write(bytes.Repeat([]byte{'X'}, int(h.config.GetNormalizedXPaddingBytes().rand())))
							if err != nil {
								break
							}
							timer := time.NewTimer(time.Duration(scStreamUpServerSecs.rand()) * time.Second)
							select {
							case <-timer.C:
							case <-httpSC.Wait():
								timer.Stop()
								return
							case <-request.Context().Done():
								timer.Stop()
								return
							}
						}
					}()
				}
				select {
				case <-request.Context().Done():
				case <-httpSC.Wait():
				}
			}
			httpSC.Close()
			return
		}

		if !isModeAllowed(h.config.Mode, ModePacketUp) {
			errors.LogInfo(context.Background(), "packet-up mode is not allowed")
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		dataPlacement := h.config.GetNormalizedUplinkDataPlacement()
		if !acquireSlot(&h.pendingRequests, 256) {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		defer h.pendingRequests.Add(-1)
		if useCustomUplink && dataPlacement != PlacementBody {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		reservation := int64(scMaxEachPostBytes)*4 + int64(h.config.GetNormalizedServerMaxHeaderBytes())*4 + 65536
		lease := h.budget.acquire(reservation)
		if lease == nil {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		defer func() {
			if lease != nil {
				lease.release()
			}
		}()
		var headerPayload []byte
		if dataPlacement == PlacementAuto || dataPlacement == PlacementHeader {
			var headerPayloadChunks []string
			for i := 0; true; i++ {
				chunk := request.Header.Get(fmt.Sprintf("%s-%d", uplinkDataKey, i))
				if chunk == "" {
					break
				}
				headerPayloadChunks = append(headerPayloadChunks, chunk)
			}
			headerPayloadEncoded := strings.Join(headerPayloadChunks, "")
			headerPayload, err = base64.RawURLEncoding.DecodeString(headerPayloadEncoded)
			if err != nil {
				errors.LogInfo(context.Background(), "Invalid base64 in header's payload: ", err.Error())
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
		}

		var cookiePayload []byte
		if dataPlacement == PlacementAuto || dataPlacement == PlacementCookie {
			var cookiePayloadChunks []string
			for i := 0; true; i++ {
				cookieName := fmt.Sprintf("%s_%d", uplinkDataKey, i)
				if c, _ := request.Cookie(cookieName); c != nil {
					cookiePayloadChunks = append(cookiePayloadChunks, c.Value)
				} else {
					break
				}
			}
			cookiePayloadEncoded := strings.Join(cookiePayloadChunks, "")
			cookiePayload, err = base64.RawURLEncoding.DecodeString(cookiePayloadEncoded)
			if err != nil {
				errors.LogInfo(context.Background(), "Invalid base64 in cookies' payload: ", err.Error())
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
		}

		var bodyPayload []byte
		if dataPlacement == PlacementAuto || dataPlacement == PlacementBody {
			var readErr error
			if useCustomUplink {
				wireLimit := int64(scMaxEachPostBytes)*4 + 65536
				if request.ContentLength > wireLimit {
					writer.WriteHeader(http.StatusRequestEntityTooLarge)
					return
				}
				request.Body = &paddedDownlinkReader{ReadCloser: &limitedUploadBody{Reader: io.LimitReader(request.Body, wireLimit), Closer: request.Body}}
				request.ContentLength = -1
			}
			if request.ContentLength > int64(scMaxEachPostBytes) {
				errors.LogInfo(context.Background(), "Too large upload. scMaxEachPostBytes is set to ", scMaxEachPostBytes, "but request size exceed it. Adjust scMaxEachPostBytes on the server to be at least as large as client.")
				writer.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
			if request.ContentLength > 0 {
				bodyPayload = make([]byte, request.ContentLength)
				_, readErr = io.ReadFull(request.Body, bodyPayload)
			} else {
				bodyPayload, readErr = buf.ReadAllToBytes(io.LimitReader(request.Body, int64(scMaxEachPostBytes)+1))
			}
			if readErr != nil {
				errors.LogInfoInner(context.Background(), readErr, "failed to read body payload")
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
		}

		var payload []byte
		switch dataPlacement {
		case PlacementHeader:
			payload = headerPayload
		case PlacementCookie:
			payload = cookiePayload
		case PlacementBody:
			payload = bodyPayload
		case PlacementAuto:
			switch {
			case len(headerPayload) == 0 && len(cookiePayload) == 0:
				payload = bodyPayload
			case len(bodyPayload) == 0 && len(cookiePayload) == 0:
				payload = headerPayload
			case len(headerPayload) == 0 && len(bodyPayload) == 0:
				payload = cookiePayload
			default:
				payload = slices.Concat(headerPayload, cookiePayload, bodyPayload)
			}
		}

		if len(payload) > scMaxEachPostBytes {
			errors.LogInfo(context.Background(), "Too large upload. scMaxEachPostBytes is set to ", scMaxEachPostBytes, "but request size exceed it. Adjust scMaxEachPostBytes on the server to be at least as large as client.")
			writer.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}

		seq, err := strconv.ParseUint(seqStr, 10, 64)
		if err != nil {
			errors.LogInfoInner(context.Background(), err, "failed to upload (ParseUint)")
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}

		lease.shrink(int64(cap(payload)) + int64(cap(headerPayload)) + int64(cap(cookiePayload)) + int64(cap(bodyPayload)))
		packet := Packet{
			Payload: payload,
			Seq:     seq,
			lease:   lease,
		}
		lease = nil
		err = currentSession.uploadQueue.PushContext(request.Context(), packet)
		if err != nil {
			errors.LogInfoInner(context.Background(), err, "failed to upload (PushPayload)")
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}

		if len(bodyPayload) == 0 {

			writer.Header().Set("Cache-Control", "no-store")
		}

		writer.WriteHeader(http.StatusOK)
	} else if request.Method == "GET" || sessionId == "" {
		if sessionId == "" {
			if h.closed.Load() || !acquireSlot(&h.sessionCount, h.config.serverSessionLimit()) {
				writer.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			defer h.sessionCount.Add(-1)
		}
		if sessionId != "" {

			h.sessionMu.Lock()
			stored, exists := h.sessions.Load(sessionId)
			if !exists || stored != currentSession || currentSession.isFullyConnected.Done() {
				h.sessionMu.Unlock()
				writer.WriteHeader(http.StatusConflict)
				return
			}
			currentSession.isFullyConnected.Close()
			h.sessionMu.Unlock()
			defer h.deleteSession(sessionId, currentSession)
		}

		writer.Header().Set("Cache-Control", "no-store")

		if useCustomPadding {
			writer.Header().Set(customPadding.Header, customPadding.wireValue())
			writer.Header().Set("Cache-Control", "no-store, no-transform")
		}

		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()

		httpSC := &httpServerConn{
			Instance:       done.New(),
			Reader:         request.Body,
			ResponseWriter: writer,
		}
		if useCustomUplink && sessionId == "" {
			httpSC.Reader = &paddedDownlinkReader{ReadCloser: request.Body}
		}
		localAddr := h.localAddr
		if la, ok := request.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && la != nil {
			localAddr = la
		}
		conn := transportConn{
			writer:     httpSC,
			reader:     httpSC,
			remoteAddr: remoteAddr,
			localAddr:  localAddr,
		}
		if sessionId != "" {
			conn.reader = currentSession.uploadQueue
		}
		if useCustomPadding {
			conn.writer = &paddedDownlinkWriter{WriteCloser: httpSC, config: customPadding}
		}

		h.ln.addConn(stat.Connection(&conn))

		select {
		case <-request.Context().Done():
		case <-httpSC.Wait():
		}

		conn.Close()
	} else {
		errors.LogInfo(context.Background(), "unsupported method: ", request.Method)
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

type httpServerConn struct {
	sync.Mutex
	closeOnce sync.Once
	*done.Instance
	io.Reader
	http.ResponseWriter
}

func (c *httpServerConn) Write(b []byte) (int, error) {
	c.Lock()
	defer c.Unlock()
	if c.Done() {
		return 0, io.ErrClosedPipe
	}
	n, err := c.ResponseWriter.Write(b)
	if err == nil {
		err = http.NewResponseController(c.ResponseWriter).Flush()
	}
	return n, err
}

func (c *httpServerConn) Close() error {
	c.closeOnce.Do(func() {
		c.Instance.Close()
		if !c.TryLock() {
			http.NewResponseController(c.ResponseWriter).SetWriteDeadline(time.Now())
			c.Lock()
		}
		c.Unlock()
	})
	return nil
}

type Listener struct {
	sync.Mutex
	server     http.Server
	h3server   *http3.Server
	listener   net.Listener
	h3listener http3.QUICListener
	config     *Config
	addConn    internet.ConnHandler
	isH3       bool
	handler    *requestHandler
}

func ListenXH(ctx context.Context, address net.Address, port net.Port, streamSettings *internet.MemoryStreamConfig, addConn internet.ConnHandler) (internet.Listener, error) {
	l := &Listener{
		addConn: addConn,
	}
	l.config = streamSettings.ProtocolSettings.(*Config)
	if err := l.config.Validate(); err != nil {
		return nil, err
	}
	if l.config != nil {
		if streamSettings.SocketSettings == nil {
			streamSettings.SocketSettings = &internet.SocketConfig{}
		}
	}
	handler := &requestHandler{
		config:         l.config,
		host:           l.config.Host,
		path:           l.config.GetNormalizedPath(),
		ln:             l,
		sessionMu:      &sync.Mutex{},
		sessions:       sync.Map{},
		socketSettings: streamSettings.SocketSettings,
		stopped:        done.New(),
	}
	handler.budget.limit = l.config.serverMemoryBudget()
	l.handler = handler
	tlsConfig := getTLSConfig(streamSettings)
	l.isH3 = len(tlsConfig.NextProtos) == 1 && tlsConfig.NextProtos[0] == "h3"

	listen := func(addr net.Addr) (net.Listener, error) {
		if streamSettings.FinalMask != nil {
			return streamSettings.FinalMask.Listen(ctx, addr)
		}
		return internet.ListenSystem(ctx, addr, streamSettings.SocketSettings)
	}
	var err error
	if port == net.Port(0) {
		l.listener, err = listen(&net.UnixAddr{
			Name: address.Domain(),
			Net:  "unix",
		})
		if err != nil {
			return nil, errors.New("failed to listen UNIX domain socket for XERA-HTTP on ", address).Base(err)
		}
		errors.LogInfo(ctx, "listening UNIX domain socket for XERA-HTTP on ", address)
	} else if l.isH3 {
		var Conn net.PacketConn
		addr := &net.UDPAddr{IP: address.IP(), Port: int(port)}
		if streamSettings.FinalMask != nil {
			Conn, err = streamSettings.FinalMask.ListenPacket(ctx, addr)
		} else {
			Conn, err = internet.ListenSystemPacket(ctx, addr, streamSettings.SocketSettings)
		}
		if err != nil {
			return nil, errors.New("failed to listen UDP for XERA-HTTP/3 on ", address, ":", port).Base(err)
		}

		quicParams := streamSettings.QuicParams
		if quicParams == nil {
			quicParams = &internet.QuicParams{
				BbrProfile: string(bbr.ProfileStandard),
			}
		}

		quicConfig := &quic.Config{
			InitialStreamReceiveWindow:     quicParams.InitStreamReceiveWindow,
			MaxStreamReceiveWindow:         quicParams.MaxStreamReceiveWindow,
			InitialConnectionReceiveWindow: quicParams.InitConnReceiveWindow,
			MaxConnectionReceiveWindow:     quicParams.MaxConnReceiveWindow,
			MaxIdleTimeout:                 time.Duration(quicParams.MaxIdleTimeout) * time.Second,
			MaxIncomingStreams:             quicParams.MaxIncomingStreams,
			DisablePathMTUDiscovery:        quicParams.DisablePathMtuDiscovery || (runtime.GOOS != "linux" && runtime.GOOS != "windows" && runtime.GOOS != "darwin"),
		}

		var k *quic.StatelessResetKey
		if !quicParams.DisableStatelessReset {
			k = &quic.StatelessResetKey{}
			common.Must2(rand.Read((*k)[:]))
		}

		tr := &quic.Transport{Conn: Conn, DisableGSO: quicParams.DisableGSO, StatelessResetKey: k}

		l.h3listener, err = tr.ListenEarly(tlsConfig, quicConfig)
		if err != nil {
			return nil, errors.New("failed to listen QUIC for XERA-HTTP/3 on ", address, ":", port).Base(err)
		}
		l.h3listener = &QListener{
			QUICListener: l.h3listener,
			quicParams:   quicParams,
		}
		errors.LogInfo(ctx, "listening QUIC for XERA-HTTP/3 on ", address, ":", port)

		handler.localAddr = l.h3listener.Addr()

		l.h3server = &http3.Server{
			Handler: handler,
		}
		go func() {
			if err := l.h3server.ServeListener(l.h3listener); err != nil {
				errors.LogErrorInner(ctx, err, "failed to serve HTTP/3 for XERA-HTTP/3")
			}
			_ = tr.Close()
			_ = Conn.Close()
		}()
	} else {
		l.listener, err = listen(&net.TCPAddr{
			IP:   address.IP(),
			Port: int(port),
		})
		if err != nil {
			return nil, errors.New("failed to listen TCP for XERA-HTTP on ", address, ":", port).Base(err)
		}
		errors.LogInfo(ctx, "listening TCP for XERA-HTTP on ", address, ":", port)
	}

	if l.listener != nil {
		if config := tls.ConfigFromStreamSettings(streamSettings); config != nil {
			if tlsConfig := config.GetTLSConfig(); tlsConfig != nil {
				l.listener = gotls.NewListener(l.listener, tlsConfig)
			}
		}
		if config := reality.ConfigFromStreamSettings(streamSettings); config != nil {
			l.listener = goreality.NewListener(l.listener, config.GetREALITYConfig())
		}

		handler.localAddr = l.listener.Addr()

		protocols := new(http.Protocols)
		protocols.SetHTTP1(true)
		protocols.SetUnencryptedHTTP2(true)
		l.server = http.Server{
			Handler:           handler,
			ReadHeaderTimeout: time.Second * 4,
			IdleTimeout:       net.ConnIdleTimeout,
			MaxHeaderBytes:    l.config.GetNormalizedServerMaxHeaderBytes(),
			Protocols:         protocols,
		}
		go func() {
			if err := l.server.Serve(l.listener); err != nil && !stderrors.Is(err, http.ErrServerClosed) {
				errors.LogErrorInner(ctx, err, "failed to serve HTTP for XERA-HTTP")
			}
		}()
	}

	return l, err
}

func (ln *Listener) Addr() net.Addr {
	if ln.h3listener != nil {
		return ln.h3listener.Addr()
	}
	if ln.listener != nil {
		return ln.listener.Addr()
	}
	return nil
}

func (ln *Listener) Close() error {
	if ln.handler != nil {
		ln.handler.sessionMu.Lock()
		ln.handler.closed.Store(true)
		ln.handler.stopped.Close()
		ln.handler.sessionMu.Unlock()
		defer ln.handler.sessions.Range(func(key, value any) bool {
			session := value.(*httpSession)
			if ln.handler.deleteSession(key.(string), session) {
				session.uploadQueue.Close()
			}
			return true
		})
	}
	if ln.h3server != nil {
		return ln.h3server.Close()
	} else if ln.listener != nil {
		defer ln.listener.Close()
		return ln.server.Close()
	}
	return errors.New("listener does not have an HTTP/3 server or a net.listener")
}

func getTLSConfig(streamSettings *internet.MemoryStreamConfig) *gotls.Config {
	config := tls.ConfigFromStreamSettings(streamSettings)
	if config == nil {
		return &gotls.Config{}
	}
	return config.GetTLSConfig()
}

func init() {
	common.Must(internet.RegisterTransportListener(protocolName, ListenXH))
}

type QListener struct {
	http3.QUICListener
	quicParams *internet.QuicParams
}

func (l *QListener) Accept(ctx context.Context) (*quic.Conn, error) {
	conn, err := l.QUICListener.Accept(ctx)
	if err != nil {
		return nil, err
	}
	switch l.quicParams.Congestion {
	case "reno":
	case "", "bbr":
		congestion.UseBBR(conn, bbr.Profile(l.quicParams.BbrProfile))
	case "force-brutal":
		congestion.UseBrutal(conn, l.quicParams.BrutalUp, l.quicParams.BrutalDisableLossCompensation)
	default:
		_ = conn.CloseWithError(0, "unsupported congestion profile")
		return nil, errors.New("unsupported congestion profile")
	}
	return conn, nil
}
