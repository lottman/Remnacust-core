package splithttp

import (
	"context"
	gotls "crypto/tls"
	"fmt"
	"io"
	stdnet "net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"reflect"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/http3"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/net/cnc"
	"github.com/xtls/xray-core/common/signal/done"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/browser_dialer"
	"github.com/xtls/xray-core/transport/internet/hysteria/congestion"
	"github.com/xtls/xray-core/transport/internet/hysteria/congestion/bbr"
	"github.com/xtls/xray-core/transport/internet/reality"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/internet/tls"
	"github.com/xtls/xray-core/transport/pipe"
	"golang.org/x/net/http2"
)

type dialerConf struct {
	net.Destination
	*internet.MemoryStreamConfig
}

var (
	globalDialerMap    map[dialerConf]*XmuxManager
	globalDialerAccess sync.Mutex
)

func getHTTPClient(ctx context.Context, dest net.Destination, streamSettings *internet.MemoryStreamConfig) (DialerClient, *XmuxClient) {
	return acquireHTTPClient(ctx, dest, streamSettings, nil, false)
}

func acquireHTTPClient(ctx context.Context, dest net.Destination, streamSettings, parent *internet.MemoryStreamConfig, reference bool) (DialerClient, *XmuxClient) {
	if ctx.Err() != nil || streamSettings.IsClosed() || parent.IsClosed() {
		return &retiredDialer{}, nil
	}
	realityConfig := reality.ConfigFromStreamSettings(streamSettings)

	if browser_dialer.HasBrowserDialer() && realityConfig == nil {
		return &BrowserDialerClient{transportConfig: streamSettings.ProtocolSettings.(*Config)}, nil
	}

	globalDialerAccess.Lock()
	defer globalDialerAccess.Unlock()
	if ctx.Err() != nil || streamSettings.IsClosed() || parent.IsClosed() {
		return &retiredDialer{}, nil
	}

	if globalDialerMap == nil {
		globalDialerMap = make(map[dialerConf]*XmuxManager)
	}

	key := dialerConf{dest, streamSettings}

	xmuxManager, found := globalDialerMap[key]

	if !found {
		transportConfig := streamSettings.ProtocolSettings.(*Config)
		xmuxManager = NewXmuxManager(transportConfig.Xmux, func() XmuxConn {
			return createHTTPClient(dest, streamSettings)
		})
		if !streamSettings.AddCloseHook(func() {
			globalDialerAccess.Lock()
			if globalDialerMap[key] == xmuxManager {
				delete(globalDialerMap, key)
			}
			clients := xmuxManager.xmuxClients
			xmuxManager.xmuxClients = nil
			globalDialerAccess.Unlock()
			for _, client := range clients {
				client.NotUsed.Store(true)
				client.maybeClose()
			}
		}) {
			return &retiredDialer{}, nil
		}
		globalDialerMap[key] = xmuxManager
	}

	xmuxClient := xmuxManager.GetXmuxClient(ctx)
	if reference {
		xmuxClient.AddReference()
	} else {
		xmuxClient.AddRunning()
	}
	return xmuxClient.XmuxConn.(DialerClient), xmuxClient
}

func addHTTPClientReference(client *XmuxClient) {
	if client == nil {
		return
	}
	globalDialerAccess.Lock()
	client.AddReference()
	globalDialerAccess.Unlock()
}

func releaseHTTPClient(client *XmuxClient, reference bool) {
	if client == nil {
		return
	}
	globalDialerAccess.Lock()
	if !reference {
		client.Running.Add(-1)
	}
	client.releaseReference()
	globalDialerAccess.Unlock()
	client.maybeClose()
}

func decideHTTPVersion(tlsConfig *tls.Config, realityConfig *reality.Config) string {
	if realityConfig != nil {
		return "2"
	}
	if tlsConfig == nil {
		return "1.1"
	}
	if len(tlsConfig.NextProtocol) != 1 {
		return "2"
	}
	if tlsConfig.NextProtocol[0] == "http/1.1" {
		return "1.1"
	}
	if tlsConfig.NextProtocol[0] == "h3" {
		return "3"
	}
	return "2"
}

func createHTTPClient(dest net.Destination, streamSettings *internet.MemoryStreamConfig) DialerClient {
	tlsConfig := tls.ConfigFromStreamSettings(streamSettings)
	realityConfig := reality.ConfigFromStreamSettings(streamSettings)

	httpVersion := decideHTTPVersion(tlsConfig, realityConfig)
	if httpVersion == "3" {
		dest.Network = net.Network_UDP // better to keep this line
	}

	var gotlsConfig *gotls.Config

	if tlsConfig != nil {
		gotlsConfig = tlsConfig.GetTLSConfig(tls.WithDestination(dest))
	}

	transportConfig := streamSettings.ProtocolSettings.(*Config)

	dialContext := func(ctxInner context.Context) (net.Conn, error) {
		ctxInner, cancel := context.WithTimeout(ctxInner, 16*time.Second)
		defer cancel()
		var conn net.Conn
		var err error
		if streamSettings.FinalMask != nil {
			conn, err = streamSettings.FinalMask.DialTCP(ctxInner, dest)
		} else {
			conn, err = internet.DialSystem(ctxInner, dest, streamSettings.SocketSettings)
		}
		if err != nil {
			return nil, err
		}

		if realityConfig != nil {
			return reality.UClient(conn, realityConfig, ctxInner, dest)
		}

		if gotlsConfig != nil {
			if fingerprint := tls.GetFingerprint(tlsConfig.Fingerprint); fingerprint != nil {
				conn = tls.UClient(conn, gotlsConfig, fingerprint)
				if err := conn.(*tls.UConn).HandshakeContext(ctxInner); err != nil {
					conn.Close()
					return nil, err
				}
			} else {
				conn = tls.Client(conn, gotlsConfig)
				if err := conn.(tls.Interface).HandshakeContext(ctxInner); err != nil {
					conn.Close()
					return nil, err
				}
			}
		}

		return conn, nil
	}

	var keepAlivePeriod time.Duration
	if streamSettings.ProtocolSettings.(*Config).Xmux != nil {
		keepAlivePeriod = time.Duration(streamSettings.ProtocolSettings.(*Config).Xmux.HKeepAlivePeriod) * time.Second
	}

	var transport http.RoundTripper

	if httpVersion == "3" {
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
			KeepAlivePeriod:                time.Duration(quicParams.KeepAlivePeriod) * time.Second,
			MaxIncomingStreams:             quicParams.MaxIncomingStreams,
			DisablePathMTUDiscovery:        quicParams.DisablePathMtuDiscovery || (runtime.GOOS != "linux" && runtime.GOOS != "windows" && runtime.GOOS != "darwin"),
			ChromeParrot:                   !quicParams.DisableChromeParrot,
		}
		if quicParams.MaxIdleTimeout == 0 {
			quicConfig.MaxIdleTimeout = net.ConnIdleTimeout
		}
		if quicParams.KeepAlivePeriod == 0 {
			if keepAlivePeriod == 0 {
				quicConfig.KeepAlivePeriod = net.QuicgoH3KeepAlivePeriod
			} else if keepAlivePeriod > 0 {
				quicConfig.KeepAlivePeriod = keepAlivePeriod
			}
		}
		if quicParams.MaxIncomingStreams == 0 {
			// these two are defaults of quic-go/http3. the default of quic-go (no
			// http3) is different, so it is hardcoded here for clarity.
			// https://github.com/quic-go/quic-go/blob/b8ea5c798155950fb5bbfdd06cad1939c9355878/http3/client.go#L36-L39
			quicConfig.MaxIncomingStreams = -1
		}

		transport = &http3.Transport{
			QUICConfig:      quicConfig,
			TLSClientConfig: gotlsConfig,
			Dial: func(ctx context.Context, addr string, tlsCfg *gotls.Config, cfg *quic.Config) (*quic.Conn, error) {
				var pktConn net.PacketConn
				var udpAddr net.Addr
				if streamSettings.FinalMask != nil {
					conn, err := streamSettings.FinalMask.DialUDP(ctx, dest)
					if err != nil {
						return nil, errors.New("failed to dial to dest").Base(err)
					}
					pktConn = conn.(*net.PacketConnWrapper).PacketConn
					udpAddr = conn.RemoteAddr()
				} else {
					conn, err := internet.DialSystem(ctx, dest, streamSettings.SocketSettings)
					if err != nil {
						return nil, errors.New("failed to dial to dest").Base(err)
					}
					switch c := conn.(type) {
					case *net.PacketConnWrapper:
						pktConn = c.PacketConn
						udpAddr = c.RemoteAddr()
					case *cnc.Connection:
						pktConn = &internet.FakePacketConn{Conn: c}
						udpAddr = &net.UDPAddr{IP: []byte{0, 0, 0, 0}}
					default:
						panic(reflect.TypeOf(c))
					}
				}

				tr := &quic.Transport{Conn: pktConn, DisableGSO: quicParams.DisableGSO}

				if !quicParams.DisableChromeParrot {
					tr.ConnectionIDGenerator = quic.ZeroLengthConnectionIDGenerator{}
					tlsCfg.GetCertificate = nil
				}

				conn, err := tr.DialEarly(ctx, udpAddr, tlsCfg, cfg)
				if err != nil {
					tr.Close()
					pktConn.Close()
					return nil, err
				}
				context.AfterFunc(conn.Context(), func() { tr.Close(); pktConn.Close() })

				switch quicParams.Congestion {
				case "reno":
				case "", "bbr":
					congestion.UseBBR(conn, bbr.Profile(quicParams.BbrProfile))
				case "force-brutal":
					congestion.UseBrutal(conn, quicParams.BrutalUp, quicParams.BrutalDisableLossCompensation)
				default:
					panic(quicParams.Congestion)
				}

				return conn, nil
			},
		}
	} else if httpVersion == "2" {
		if keepAlivePeriod == 0 {
			keepAlivePeriod = net.ChromeH2KeepAlivePeriod
		}
		if keepAlivePeriod < 0 {
			keepAlivePeriod = 0
		}
		transport = &http2.Transport{
			DialTLSContext: func(ctxInner context.Context, network string, addr string, cfg *gotls.Config) (net.Conn, error) {
				return dialContext(ctxInner)
			},
			IdleConnTimeout: net.ConnIdleTimeout,
			ReadIdleTimeout: keepAlivePeriod,
		}
	} else {
		httpDialContext := func(ctxInner context.Context, network string, addr string) (net.Conn, error) {
			return dialContext(ctxInner)
		}

		transport = &http.Transport{
			DialTLSContext:  httpDialContext,
			DialContext:     httpDialContext,
			IdleConnTimeout: net.ConnIdleTimeout,
			// chunked transfer download with KeepAlives is buggy with
			// http.Client and our custom dial context.
			DisableKeepAlives: true,
		}
	}

	client := &DefaultDialerClient{
		transportConfig: transportConfig,
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		httpVersion: httpVersion,
	}
	if httpVersion == "1.1" {
		uploadTransport := transport.(*http.Transport).Clone()
		uploadTransport.DisableKeepAlives = false
		uploadTransport.MaxIdleConns = 32
		uploadTransport.MaxIdleConnsPerHost = 32
		uploadTransport.MaxConnsPerHost = 32
		client.uploadClient = &http.Client{Transport: uploadTransport, CheckRedirect: client.client.CheckRedirect}
	}

	return client
}

func init() {
	common.Must(internet.RegisterTransportDialer(protocolName, Dial))
}

func prepareDownloadSettings(streamSettings *internet.MemoryStreamConfig, config *Config) (*internet.MemoryStreamConfig, error) {
	globalDialerAccess.Lock()
	defer globalDialerAccess.Unlock()
	memory, err := streamSettings.GetDownloadSettings()
	if err != nil {
		return nil, err
	}
	if config.DownloadSettings == nil {
		return nil, nil
	}
	validate := func(memory *internet.MemoryStreamConfig) error {
		if memory == streamSettings || memory.IsClosed() {
			return stdnet.ErrClosed
		}
		if memory.Destination == nil || memory.Destination.Address == nil || memory.Destination.Port == 0 {
			return errors.New("XHTTP download settings require an address and a nonzero port")
		}
		download, ok := memory.ProtocolSettings.(*Config)
		if !ok || download == nil {
			return errors.New("XHTTP download settings must use XHTTP")
		}
		return nil
	}
	if memory != nil {
		return memory, validate(memory)
	}
	candidate, err := internet.ToMemoryStreamConfig(config.DownloadSettings)
	if err != nil {
		return nil, errors.New("invalid XHTTP download settings").Base(err)
	}
	if err := validate(candidate); err != nil {
		_ = candidate.Close()
		return nil, err
	}
	if streamSettings.SocketSettings != nil && streamSettings.SocketSettings.Penetrate {
		candidate.SocketSettings = streamSettings.SocketSettings
	}
	memory, err = streamSettings.LoadOrStoreDownloadSettings(candidate)
	if err != nil {
		return nil, err
	}
	return memory, validate(memory)
}

func Dial(ctx context.Context, dest net.Destination, streamSettings *internet.MemoryStreamConfig) (stat.Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if streamSettings.IsClosed() {
		return nil, stdnet.ErrClosed
	}
	tlsConfig := tls.ConfigFromStreamSettings(streamSettings)
	realityConfig := reality.ConfigFromStreamSettings(streamSettings)

	httpVersion := decideHTTPVersion(tlsConfig, realityConfig)
	if httpVersion == "3" {
		dest.Network = net.Network_UDP
	}

	transportConfiguration := streamSettings.ProtocolSettings.(*Config)
	memory2, err := prepareDownloadSettings(streamSettings, transportConfiguration)
	if err != nil {
		return nil, err
	}
	var requestURL url.URL

	if tlsConfig != nil || realityConfig != nil {
		requestURL.Scheme = "https"
	} else {
		requestURL.Scheme = "http"
	}
	requestURL.Host = transportConfiguration.Host
	if requestURL.Host == "" && tlsConfig != nil {
		requestURL.Host = tlsConfig.ServerName
	}
	if requestURL.Host == "" && realityConfig != nil {
		requestURL.Host = realityConfig.ServerName
	}
	if requestURL.Host == "" {
		requestURL.Host = dest.Address.String()
	}
	if browser_dialer.HasBrowserDialer() && realityConfig == nil {
		// For Browser Dialer's optimized IP and non-standard port
		if !(requestURL.Scheme == "http" && dest.Port == 80) && !(requestURL.Scheme == "https" && dest.Port == 443) {
			requestURL.Host += ":" + dest.Port.String()
		}
	}

	requestURL.Path = transportConfiguration.GetNormalizedPath()
	requestURL.RawQuery = transportConfiguration.GetNormalizedQuery()

	httpClient, xmuxClient := getHTTPClient(ctx, dest, streamSettings)
	xmuxClient2 := xmuxClient
	downloadLease := false
	releaseClients := sync.OnceFunc(func() {
		releaseHTTPClient(xmuxClient, false)
		if downloadLease {
			releaseHTTPClient(xmuxClient2, false)
		}
	})
	succeeded := false
	defer func() {
		if !succeeded {
			releaseClients()
		}
	}()
	if httpClient.IsClosed() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, stdnet.ErrClosed
	}

	mode := transportConfiguration.Mode
	if mode == "" || mode == "auto" {
		mode = "packet-up"
		if realityConfig != nil {
			mode = "stream-one"
			if transportConfiguration.DownloadSettings != nil {
				mode = "stream-up"
			}
		}
	}

	sessionId := ""
	if mode != "stream-one" {
		sessionId = transportConfiguration.GenerateSessionID()
	}

	errors.LogInfo(ctx, fmt.Sprintf("XHTTP is dialing to %s, mode %s, HTTP version %s, host %s", dest, mode, httpVersion, requestURL.Host))

	requestURL2 := requestURL
	httpClient2 := httpClient
	if memory2 != nil {
		dest2 := *memory2.Destination
		tlsConfig2 := tls.ConfigFromStreamSettings(memory2)
		realityConfig2 := reality.ConfigFromStreamSettings(memory2)
		httpVersion2 := decideHTTPVersion(tlsConfig2, realityConfig2)
		if httpVersion2 == "3" {
			dest2.Network = net.Network_UDP
		}
		if tlsConfig2 != nil || realityConfig2 != nil {
			requestURL2.Scheme = "https"
		} else {
			requestURL2.Scheme = "http"
		}
		config2 := memory2.ProtocolSettings.(*Config)
		requestURL2.Host = config2.Host
		if requestURL2.Host == "" && tlsConfig2 != nil {
			requestURL2.Host = tlsConfig2.ServerName
		}
		if requestURL2.Host == "" && realityConfig2 != nil {
			requestURL2.Host = realityConfig2.ServerName
		}
		if requestURL2.Host == "" {
			requestURL2.Host = dest2.Address.String()
		}
		if browser_dialer.HasBrowserDialer() && realityConfig2 == nil {
			// For Browser Dialer's optimized IP and non-standard port
			if !(requestURL2.Scheme == "http" && dest2.Port == 80) && !(requestURL2.Scheme == "https" && dest2.Port == 443) {
				requestURL2.Host += ":" + dest2.Port.String()
			}
		}
		requestURL2.Path = config2.GetNormalizedPath()
		requestURL2.RawQuery = config2.GetNormalizedQuery()
		httpClient2, xmuxClient2 = acquireHTTPClient(ctx, dest2, memory2, streamSettings, false)
		downloadLease = true
		if httpClient2.IsClosed() {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, stdnet.ErrClosed
		}
		errors.LogInfo(ctx, fmt.Sprintf("XHTTP is downloading from %s, mode %s, HTTP version %s, host %s", dest2, "stream-down", httpVersion2, requestURL2.Host))
	}

	reader, writer := io.Pipe()
	conn := splitConn{
		writer:  writer,
		onClose: releaseClients,
	}
	defer func() {
		if !succeeded {
			conn.Close()
			reader.Close()
		}
	}()

	if mode == "stream-one" {
		requestURL.Path = transportConfiguration.GetNormalizedPath()
		if xmuxClient != nil {
			xmuxClient.LeftRequests.Add(-1)
		}
		conn.reader, conn.remoteAddr, conn.localAddr, err = httpClient.OpenStream(ctx, requestURL.String(), sessionId, reader, false)
		if err != nil { // browser dialer only
			return nil, err
		}
		succeeded = true
		return stat.Connection(&conn), nil
	} else { // stream-down
		if xmuxClient2 != nil {
			xmuxClient2.LeftRequests.Add(-1)
		}
		conn.reader, conn.remoteAddr, conn.localAddr, err = httpClient2.OpenStream(ctx, requestURL2.String(), sessionId, nil, false)
		if err != nil { // browser dialer only
			return nil, err
		}
	}
	if mode == "stream-up" {
		if xmuxClient != nil {
			xmuxClient.LeftRequests.Add(-1)
		}
		conn.extra, _, _, err = httpClient.OpenStream(ctx, requestURL.String(), sessionId, reader, true)
		if err != nil { // browser dialer only
			return nil, err
		}
		succeeded = true
		return stat.Connection(&conn), nil
	}

	scMaxEachPostBytes := transportConfiguration.GetNormalizedScMaxEachPostBytes()
	scMinPostsIntervalMs := transportConfiguration.GetNormalizedScMinPostsIntervalMs()

	if scMaxEachPostBytes.From <= 0 {
		return nil, errors.New("scMaxEachPostBytes should be bigger than 0")
	}
	if scMaxEachPostBytes.To < scMaxEachPostBytes.From {
		return nil, errors.New("scMaxEachPostBytes range is invalid")
	}

	maxUploadSize := scMaxEachPostBytes.rand()
	// WithSizeLimit(0) will still allow single bytes to pass, and a lot of
	// code relies on this behavior. Subtract 1 so that together with
	// uploadWriter wrapper, exact size limits can be enforced
	// uploadPipeReader, uploadPipeWriter := pipe.New(pipe.WithSizeLimit(maxUploadSize - 1))
	uploadPipeReader, uploadPipeWriter := pipe.New(pipe.WithSizeLimit(max(0, maxUploadSize-buf.Size)))

	conn.writer = uploadWriter{
		uploadPipeWriter,
		maxUploadSize,
	}
	uploadCtx, cancelUploads := context.WithCancel(context.WithoutCancel(ctx))
	conn.cancel = cancelUploads
	stopUploads := context.AfterFunc(uploadCtx, uploadPipeReader.Interrupt)
	addHTTPClientReference(xmuxClient)

	go func() {
		defer stopUploads()
		defer uploadPipeReader.Interrupt()
		pending := make(chan struct{}, 16)
		var seq int64
		var lastWrite time.Time

		dynamicHTTPClient := httpClient
		dynamicXmuxClient := xmuxClient
		defer func() { releaseHTTPClient(dynamicXmuxClient, true) }()
		for {
			// by offloading the uploads into a buffered pipe, multiple conn.Write
			// calls get automatically batched together into larger POST requests.
			// without batching, bandwidth is extremely limited.
			remainder, err := uploadPipeReader.ReadMultiBuffer()
			if err != nil {
				break
			}

			doSplit := atomic.Bool{}
			for doSplit.Store(true); doSplit.Load(); {
				select {
				case pending <- struct{}{}:
				case <-uploadCtx.Done():
					doSplit.Store(false)
					continue
				}
				var chunk buf.MultiBuffer
				remainder, chunk = buf.SplitSize(remainder, maxUploadSize)
				if chunk.IsEmpty() {
					<-pending
					break
				}

				wroteRequest := done.New()

				ctx := httptrace.WithClientTrace(uploadCtx, &httptrace.ClientTrace{
					WroteRequest: func(httptrace.WroteRequestInfo) {
						wroteRequest.Close()
					},
				})

				seqStr := strconv.FormatInt(seq, 10)
				seq += 1

				if scMinPostsIntervalMs.From > 0 {
					timer := time.NewTimer(time.Duration(scMinPostsIntervalMs.rand())*time.Millisecond - time.Since(lastWrite))
					select {
					case <-timer.C:
					case <-uploadCtx.Done():
					}
					timer.Stop()
				}

				lastWrite = time.Now()

				if dynamicXmuxClient != nil && (dynamicXmuxClient.LeftRequests.Add(-1) <= 0 ||
					(dynamicXmuxClient.UnreusableAt != time.Time{} && lastWrite.After(dynamicXmuxClient.UnreusableAt))) {
					previous := dynamicXmuxClient
					dynamicHTTPClient, dynamicXmuxClient = acquireHTTPClient(ctx, dest, streamSettings, nil, true)
					releaseHTTPClient(previous, true)
				}
				addHTTPClientReference(dynamicXmuxClient)

				go func(hClient DialerClient, lease *XmuxClient) {
					defer releaseHTTPClient(lease, true)
					defer func() { <-pending }()
					err := hClient.PostPacket(
						ctx,
						requestURL.String(),
						sessionId,
						seqStr,
						chunk,
					)
					wroteRequest.Close()
					if err != nil {
						cancelUploads()
						errors.LogInfoInner(ctx, err, "failed to send upload")
						uploadPipeReader.Interrupt()
						doSplit.Store(false)
					}
				}(dynamicHTTPClient, dynamicXmuxClient)

				if _, ok := dynamicHTTPClient.(*DefaultDialerClient); ok {
					select {
					case <-wroteRequest.Wait():
					case <-uploadCtx.Done():
					}
				}
			}
			buf.ReleaseMulti(remainder)
		}
	}()

	succeeded = true
	return stat.Connection(&conn), nil
}

// A wrapper around pipe that ensures the size limit is exactly honored.
//
// The MultiBuffer pipe accepts any single WriteMultiBuffer call even if that
// single MultiBuffer exceeds the size limit, and then starts blocking on the
// next WriteMultiBuffer call. This means that ReadMultiBuffer can return more
// bytes than the size limit. We work around this by splitting a potentially
// too large write up into multiple.
type uploadWriter struct {
	*pipe.Writer
	maxLen int32
}

func (w uploadWriter) Write(b []byte) (int, error) {
	/*
		capacity := int(w.maxLen - w.Len())
		if capacity > 0 && capacity < len(b) {
			b = b[:capacity]
		}
	*/

	buffer := buf.MultiBufferContainer{}
	common.Must2(buffer.Write(b))

	var writed int
	for _, buff := range buffer.MultiBuffer {
		n := int(buff.Len())
		err := w.WriteMultiBuffer(buf.MultiBuffer{buff})
		if err != nil {
			return writed, err
		}
		writed += n
	}
	return writed, nil
}
