package xerahttp

import (
	"context"
	gotls "crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
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
	globalDialerMap     map[dialerConf]*XmuxManager
	globalDialerAccess  sync.Mutex
	globalDialerCleanup *time.Timer
)

func removeIdleHTTPClients(now time.Time) {
	for key, manager := range globalDialerMap {
		manager.pruneIdle(now)
		if now.Sub(manager.lastUsed) < net.ConnIdleTimeout {
			continue
		}
		busy := false
		for _, client := range manager.xmuxClients {
			if client.Running.Load() > 0 {
				busy = true
				break
			}
		}
		if busy {
			continue
		}
		delete(globalDialerMap, key)
		for _, client := range manager.xmuxClients {
			client.NotUsed.Store(true)
			client.maybeClose()
		}
		manager.xmuxClients = nil
	}
}

func scheduleHTTPClientCleanup() {
	if globalDialerCleanup != nil || len(globalDialerMap) == 0 {
		return
	}
	globalDialerCleanup = time.AfterFunc(min(30*time.Second, net.ConnIdleTimeout), func() {
		globalDialerAccess.Lock()
		defer globalDialerAccess.Unlock()
		globalDialerCleanup = nil
		removeIdleHTTPClients(time.Now())
		scheduleHTTPClientCleanup()
	})
}

func getHTTPClient(ctx context.Context, dest net.Destination, streamSettings *internet.MemoryStreamConfig) (DialerClient, *XmuxClient) {
	realityConfig := reality.ConfigFromStreamSettings(streamSettings)

	if browser_dialer.HasBrowserDialer() && realityConfig == nil {
		return &BrowserDialerClient{transportConfig: streamSettings.ProtocolSettings.(*Config)}, nil
	}

	globalDialerAccess.Lock()
	defer globalDialerAccess.Unlock()

	if globalDialerMap == nil {
		globalDialerMap = make(map[dialerConf]*XmuxManager)
	}

	key := dialerConf{dest, streamSettings}

	xmuxManager, found := globalDialerMap[key]

	if !found {
		transportConfig := streamSettings.ProtocolSettings.(*Config)
		xmuxConfig := transportConfig.Xmux
		if xmuxConfig == nil {
			xmuxConfig = &XmuxConfig{}
		}

		xmuxManager = NewXmuxManager(xmuxConfig, func() XmuxConn {
			return createHTTPClient(dest, streamSettings)
		})
		globalDialerMap[key] = xmuxManager
	}

	xmuxClient := xmuxManager.GetXmuxClient(ctx)
	xmuxClient.AddRunning()
	xmuxManager.lastUsed = time.Now()
	scheduleHTTPClientCleanup()
	return xmuxClient.XmuxConn.(DialerClient), xmuxClient
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
		dest.Network = net.Network_UDP
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
			secured, err := reality.UClient(conn, realityConfig, ctxInner, dest)
			if err != nil {
				conn.Close()
				return nil, err
			}
			if err := validateRealityHTTPALPN(httpVersion, secured.(*reality.UConn).ConnectionState().NegotiatedProtocol); err != nil {
				secured.Close()
				return nil, err
			}
			return secured, nil
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
			if err := validateHTTPALPN(httpVersion, conn.(tls.Interface).NegotiatedProtocol()); err != nil {
				conn.Close()
				return nil, err
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

			quicConfig.MaxIncomingStreams = -1
		}

		transport = &http3.Transport{
			QUICConfig:      quicConfig,
			TLSClientConfig: gotlsConfig,
			Dial: func(ctx context.Context, addr string, tlsCfg *gotls.Config, cfg *quic.Config) (*quic.Conn, error) {
				var pktConn net.PacketConn
				var udpAddr net.Addr
				var raw net.Conn
				var err error
				if streamSettings.FinalMask != nil {
					raw, err = streamSettings.FinalMask.DialUDP(ctx, dest)
				} else {
					raw, err = internet.DialSystem(ctx, dest, streamSettings.SocketSettings)
				}
				if err != nil {
					return nil, errors.New("failed to dial to dest").Base(err)
				}
				switch c := raw.(type) {
				case *net.PacketConnWrapper:
					pktConn = c.PacketConn
					udpAddr = c.RemoteAddr()
				case *cnc.Connection:
					pktConn = &internet.FakePacketConn{Conn: c}
					udpAddr = &net.UDPAddr{IP: []byte{0, 0, 0, 0}}
				default:
					_ = raw.Close()
					return nil, errors.New("unsupported packet connection type")
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
					_ = conn.CloseWithError(0, "unsupported congestion profile")
					_ = tr.Close()
					_ = pktConn.Close()
					return nil, errors.New("unsupported congestion profile")
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

func validateHTTPALPN(version, negotiated string) error {
	if version == "2" && negotiated != "h2" || version == "1.1" && negotiated != "" && negotiated != "http/1.1" {
		return errors.New("XERA-HTTP ALPN mismatch: HTTP ", version, ", negotiated ", negotiated)
	}
	return nil
}

func validateRealityHTTPALPN(version, negotiated string) error {
	if version == "2" && negotiated == "" {
		return nil
	}
	return validateHTTPALPN(version, negotiated)
}

func prepareDownloadSettings(streamSettings *internet.MemoryStreamConfig, config *Config) (*internet.MemoryStreamConfig, error) {
	if config.DownloadSettings == nil {
		return nil, nil
	}
	globalDialerAccess.Lock()
	defer globalDialerAccess.Unlock()
	if streamSettings.DownloadSettings != nil {
		return streamSettings.DownloadSettings, nil
	}
	memory, err := internet.ToMemoryStreamConfig(config.DownloadSettings)
	if err != nil {
		return nil, errors.New("invalid XERA-HTTP download settings").Base(err)
	}
	if memory.Destination == nil || memory.Destination.Address == nil || memory.Destination.Port == 0 {
		return nil, errors.New("XERA-HTTP download settings require an address and a nonzero port")
	}
	download, ok := memory.ProtocolSettings.(*Config)
	if !ok {
		return nil, errors.New("invalid XERA-HTTP download transport settings")
	}
	if err := download.Validate(); err != nil {
		return nil, errors.New("invalid XERA-HTTP download transport settings").Base(err)
	}
	if streamSettings.SocketSettings != nil && streamSettings.SocketSettings.Penetrate {
		memory.SocketSettings = streamSettings.SocketSettings
	}
	streamSettings.DownloadSettings = memory
	return memory, nil
}

func Dial(ctx context.Context, dest net.Destination, streamSettings *internet.MemoryStreamConfig) (stat.Connection, error) {
	tlsConfig := tls.ConfigFromStreamSettings(streamSettings)
	realityConfig := reality.ConfigFromStreamSettings(streamSettings)

	httpVersion := decideHTTPVersion(tlsConfig, realityConfig)
	if httpVersion == "3" {
		dest.Network = net.Network_UDP
	}

	transportConfiguration := streamSettings.ProtocolSettings.(*Config)
	if err := transportConfiguration.Validate(); err != nil {
		return nil, err
	}
	if download := transportConfiguration.DownloadSettings; download != nil && download.ProtocolName != protocolName {
		return nil, errors.New("XERA-HTTP download transport must use ", protocolName)
	}
	memory2, err := prepareDownloadSettings(streamSettings, transportConfiguration)
	if err != nil {
		return nil, err
	}
	if p := transportConfiguration.CustomDownlinkPadding; p != nil {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if browser_dialer.HasBrowserDialer() && realityConfig == nil {
			return nil, errors.New("custom downlink padding is not supported by Browser Dialer")
		}
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

		if !(requestURL.Scheme == "http" && dest.Port == 80) && !(requestURL.Scheme == "https" && dest.Port == 443) {
			requestURL.Host += ":" + dest.Port.String()
		}
	}

	requestURL.Path = transportConfiguration.GetNormalizedPath()
	requestURL.RawQuery = transportConfiguration.GetNormalizedQuery()

	mode := resolveMode(transportConfiguration.Mode, httpVersion, realityConfig != nil, transportConfiguration.DownloadSettings != nil)
	if browser_dialer.HasBrowserDialer() && realityConfig == nil {
		if transportConfiguration.Mode == "" || transportConfiguration.Mode == ModeAuto || transportConfiguration.Mode == ModeStreamAuto {
			mode = ModePacketUp
		} else if mode != ModePacketUp {
			return nil, errors.New("Browser Dialer requires packet-up, auto, or stream-auto mode")
		}
	}
	httpClient, xmuxClient := getHTTPClient(ctx, dest, streamSettings)

	sessionId := ""
	if mode != ModeStreamOne {
		sessionId = transportConfiguration.GenerateSessionID()
	}

	errors.LogInfo(ctx, fmt.Sprintf("XERA-HTTP is dialing to %s, mode %s, HTTP version %s, host %s", dest, mode, httpVersion, requestURL.Host))

	requestURL2 := requestURL
	httpClient2 := httpClient
	xmuxClient2 := xmuxClient
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

			if !(requestURL2.Scheme == "http" && dest2.Port == 80) && !(requestURL2.Scheme == "https" && dest2.Port == 443) {
				requestURL2.Host += ":" + dest2.Port.String()
			}
		}
		requestURL2.Path = config2.GetNormalizedPath()
		requestURL2.RawQuery = config2.GetNormalizedQuery()
		httpClient2, xmuxClient2 = getHTTPClient(ctx, dest2, memory2)
		errors.LogInfo(ctx, fmt.Sprintf("XERA-HTTP is downloading from %s, mode %s, HTTP version %s, host %s", dest2, "stream-down", httpVersion2, requestURL2.Host))
	}

	var closed atomic.Int32

	reader, writer := io.Pipe()
	conn := transportConn{
		writer: writer,
		onClose: func() {
			if closed.Add(1) > 1 {
				return
			}
			if xmuxClient != nil {
				xmuxClient.DoneRunning()
			}
			if xmuxClient2 != nil && xmuxClient2 != xmuxClient {
				xmuxClient2.DoneRunning()
			}
		},
	}
	succeeded := false
	defer func() {
		if !succeeded {
			conn.Close()
			reader.Close()
		}
	}()

	if mode == ModeStreamOne {
		requestURL.Path = transportConfiguration.GetNormalizedPath()
		if xmuxClient != nil {
			xmuxClient.LeftRequests.Add(-1)
		}
		conn.reader, conn.remoteAddr, conn.localAddr, err = httpClient.OpenStream(ctx, requestURL.String(), sessionId, reader, false)
		if err != nil {
			return nil, err
		}
		succeeded = true
		return stat.Connection(&conn), nil
	} else {
		if xmuxClient2 != nil {
			xmuxClient2.LeftRequests.Add(-1)
		}
		conn.reader, conn.remoteAddr, conn.localAddr, err = httpClient2.OpenStream(ctx, requestURL2.String(), sessionId, nil, false)
		if err != nil {
			return nil, err
		}
	}
	if mode == ModeStreamUp {
		if xmuxClient != nil {
			xmuxClient.LeftRequests.Add(-1)
		}
		conn.extra, _, _, err = httpClient.OpenStream(ctx, requestURL.String(), sessionId, reader, true)
		if err != nil {
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

	maxUploadSize := scMaxEachPostBytes.To

	uploadPipeReader, uploadPipeWriter := pipe.New(pipe.WithSizeLimit(max(0, maxUploadSize-buf.Size)))

	conn.writer = uploadWriter{
		uploadPipeWriter,
		maxUploadSize,
	}
	uploadCtx, cancelUploads := context.WithCancel(context.WithoutCancel(ctx))
	if p := transportConfiguration.CustomDownlinkPadding; p != nil && p.Uplink {
		uploadCtx = context.WithValue(uploadCtx, uploadPaddingKey{}, &sharedPaddingAllowance{})
	}
	conn.cancel = cancelUploads
	stopUploads := context.AfterFunc(uploadCtx, uploadPipeReader.Interrupt)
	if xmuxClient != nil {
		xmuxClient.AddRunning()
	}

	go func() {
		defer stopUploads()
		defer uploadPipeReader.Interrupt()
		pending := make(chan struct{}, 16)
		var seq int64
		var lastWrite time.Time

		dynamicHTTPClient := httpClient
		dynamicXmuxClient := xmuxClient
		defer func() {
			if dynamicXmuxClient != nil {
				dynamicXmuxClient.DoneRunning()
			}
		}()
		for {

			remainder, err := uploadPipeReader.ReadMultiBuffer()
			if err != nil {
				break
			}

			hasPending := atomic.Bool{}
			for hasPending.Store(true); hasPending.Load(); {
				select {
				case pending <- struct{}{}:
				case <-uploadCtx.Done():
					hasPending.Store(false)
					continue
				}
				var chunk buf.MultiBuffer
				postSize := scMaxEachPostBytes.rand()
				remainder, chunk = buf.SplitSize(remainder, postSize)
				if scMaxEachPostBytes.From != scMaxEachPostBytes.To && !remainder.IsEmpty() {
					if missing := postSize - chunk.Len(); missing > 0 {
						var tail buf.MultiBuffer
						remainder, tail = buf.SplitSize(remainder, missing)
						chunk = append(chunk, tail...)
					}
				}
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

				if scMinPostsIntervalMs.To > 0 {
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
					dynamicHTTPClient, dynamicXmuxClient = getHTTPClient(ctx, dest, streamSettings)
					previous.DoneRunning()
				}
				if dynamicXmuxClient != nil {
					dynamicXmuxClient.AddRunning()
				}

				go func(hClient DialerClient, lease *XmuxClient) {
					if lease != nil {
						defer lease.DoneRunning()
					}
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
						hasPending.Store(false)
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

type uploadWriter struct {
	*pipe.Writer
	maxLen int32
}

func (w uploadWriter) Write(b []byte) (int, error) {
	var writed int
	for len(b) > 0 {
		n := min(len(b), buf.Size, int(w.maxLen))
		if n <= 0 {
			return writed, errors.New("invalid maximum upload size")
		}
		buff := buf.New()
		buff.Write(b[:n])
		err := w.Writer.WriteMultiBuffer(buf.MultiBuffer{buff})
		if err != nil {
			return writed, err
		}
		writed += n
		b = b[n:]
	}
	return writed, nil
}
