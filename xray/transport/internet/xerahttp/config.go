package xerahttp

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/crypto"
	"github.com/xtls/xray-core/common/utils"
	"github.com/xtls/xray-core/transport/internet"
)

const (
	maxXPaddingBytes        = 64 << 10
	maxScEachPostBytes      = 16 << 20
	maxUplinkChunkSize      = 1 << 20
	maxServerHeaderBytes    = 1 << 20
	maxSessionIDLength      = 256
	maxSessionIDTableLength = 256
	defaultSessionIDLength  = 24
	maxServerSessions       = 65536
	maxBufferedPosts        = 65536
	maxXMUXConnections      = 256
	maxXMUXConcurrency      = 4096
	maxXMUXRequests         = 1 << 20
	maxXMUXReusableSecs     = 7 * 24 * 60 * 60
)

func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("XERA-HTTP config is nil")
	}
	switch c.Mode {
	case "", ModeAuto, ModeStreamAuto, ModeStreamOne, ModeStreamUp, ModePacketUp:
	default:
		return fmt.Errorf("unsupported mode: %s", c.Mode)
	}
	if c.UplinkDataPlacement == PlacementHeader || c.UplinkDataPlacement == PlacementCookie {
		if c.Mode != ModePacketUp {
			return fmt.Errorf("uplink data placement %s requires packet-up mode", c.UplinkDataPlacement)
		}
	}
	if strings.EqualFold(c.UplinkHTTPMethod, "GET") && c.Mode != ModePacketUp {
		return fmt.Errorf("uplink HTTP method GET requires packet-up mode")
	}
	if c.ServerMaxBufferedBytes < 0 {
		return fmt.Errorf("serverMaxBufferedBytes must not be negative")
	}
	if c.ServerMaxHeaderBytes < 0 {
		return fmt.Errorf("serverMaxHeaderBytes must not be negative")
	}
	if c.ServerMaxHeaderBytes > maxServerHeaderBytes {
		return fmt.Errorf("serverMaxHeaderBytes exceeds the safe limit")
	}
	if c.ServerMaxSessions > maxServerSessions {
		return fmt.Errorf("serverMaxSessions exceeds the safe limit")
	}
	if r := c.XPaddingBytes; r != nil && !(r.From == 0 && r.To == 0) && (r.From <= 0 || r.To < r.From) {
		return fmt.Errorf("xPaddingBytes range is invalid")
	}
	if r := c.XPaddingBytes; r != nil && r.To > maxXPaddingBytes {
		return fmt.Errorf("xPaddingBytes exceeds the safe limit")
	}
	if r := c.ScMaxEachPostBytes; r != nil && !(r.From == 0 && r.To == 0) && (r.From <= 0 || r.To < r.From) {
		return fmt.Errorf("scMaxEachPostBytes range is invalid")
	}
	if r := c.ScMaxEachPostBytes; r != nil && r.To > maxScEachPostBytes {
		return fmt.Errorf("scMaxEachPostBytes exceeds the safe limit")
	}
	if r := c.ScMinPostsIntervalMs; r != nil && !(r.From == 0 && r.To == 0) && (r.From < -1 || r.To < r.From) {
		return fmt.Errorf("scMinPostsIntervalMs range is invalid")
	}
	if r := c.ScStreamUpServerSecs; r != nil && !(r.From == 0 && r.To == 0) && (r.From < 0 || r.To < r.From) {
		return fmt.Errorf("scStreamUpServerSecs range is invalid")
	}
	if r := c.UplinkChunkSize; r != nil && !(r.From == 0 && r.To == 0) && (r.From <= 0 || r.To < r.From) {
		return fmt.Errorf("uplinkChunkSize range is invalid")
	}
	if r := c.UplinkChunkSize; r != nil && r.To > maxUplinkChunkSize {
		return fmt.Errorf("uplinkChunkSize exceeds the safe limit")
	}
	if c.ScMaxBufferedPosts < 0 || c.ScMaxBufferedPosts > maxBufferedPosts {
		return fmt.Errorf("scMaxBufferedPosts is outside the safe limit")
	}
	if err := c.validateSessionIDs(); err != nil {
		return err
	}
	if x := c.Xmux; x != nil {
		if err := validateNonNegativeRange("xmux.maxConnections", x.MaxConnections, maxXMUXConnections); err != nil {
			return err
		}
		if err := validateNonNegativeRange("xmux.maxConcurrency", x.MaxConcurrency, maxXMUXConcurrency); err != nil {
			return err
		}
		if err := validateNonNegativeRange("xmux.cMaxReuseTimes", x.CMaxReuseTimes, maxXMUXRequests); err != nil {
			return err
		}
		if err := validateNonNegativeRange("xmux.hMaxRequestTimes", x.HMaxRequestTimes, maxXMUXRequests); err != nil {
			return err
		}
		if err := validateNonNegativeRange("xmux.hMaxReusableSecs", x.HMaxReusableSecs, maxXMUXReusableSecs); err != nil {
			return err
		}
	}
	if p := c.CustomDownlinkPadding; p != nil {
		if err := p.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) validateSessionIDs() error {
	table := c.SessionIDTable
	if predefined, ok := PredefinedTable[table]; ok {
		table = predefined
	}
	if table == "" {
		table = PredefinedTable["Base62"]
	}
	if len(table) > maxSessionIDTableLength {
		return fmt.Errorf("sessionIDTable exceeds the safe limit")
	}
	var seen [256]bool
	for i := range len(table) {
		b := table[i]
		if seen[b] {
			return fmt.Errorf("sessionIDTable must not contain duplicate characters")
		}
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '_' || b == '.' || b == '~') {
			return fmt.Errorf("sessionIDTable must contain only URI-unreserved ASCII characters")
		}
		seen[b] = true
	}
	length := c.SessionIDLength
	if length == nil || length.From == 0 && length.To == 0 {
		length = &RangeConfig{From: defaultSessionIDLength, To: defaultSessionIDLength}
	}
	if length.From <= 0 || length.To < length.From || length.To > maxSessionIDLength {
		return fmt.Errorf("sessionIDLength is invalid")
	}
	space := new(big.Int).Exp(big.NewInt(int64(len(table))), big.NewInt(int64(length.From)), nil)
	if space.BitLen() <= 128 {
		return fmt.Errorf("session IDs require at least 128 bits of entropy at the shortest configured length")
	}
	return nil
}

func validateNonNegativeRange(name string, value *RangeConfig, maximum int32) error {
	if value == nil || (value.From == 0 && value.To == 0) {
		return nil
	}
	if value.From < 0 || value.To < value.From {
		return fmt.Errorf("%s range is invalid", name)
	}
	if value.To > maximum {
		return fmt.Errorf("%s exceeds the safe limit", name)
	}
	return nil
}

func (c *Config) GetNormalizedPath() string {
	pathAndQuery := strings.SplitN(c.Path, "?", 2)
	path := pathAndQuery[0]

	if path == "" || path[0] != '/' {
		path = "/" + path
	}

	if c.GetNormalizedSessionPlacement() == PlacementPath ||
		c.GetNormalizedSeqPlacement() == PlacementPath {
		if path[len(path)-1] != '/' {
			path = path + "/"
		}
	}

	return path
}

func (c *Config) GetNormalizedQuery() string {
	pathAndQuery := strings.SplitN(c.Path, "?", 2)
	query := ""

	if len(pathAndQuery) > 1 {
		query = pathAndQuery[1]
	}

	return query
}

func (c *Config) GetRequestHeader() http.Header {
	header := http.Header{}
	for k, v := range c.Headers {
		header.Add(k, v)
	}
	utils.TryDefaultHeadersWith(header, "fetch")
	return header
}

func (c *Config) GetRequestHeaderWithPayload(payload []byte) http.Header {
	header := c.GetRequestHeader()

	key := c.UplinkDataKey
	encodedData := base64.RawURLEncoding.EncodeToString(payload)

	for i := 0; len(encodedData) > 0; i++ {
		chunkSize := min(int(c.GetNormalizedUplinkChunkSize().rand()), len(encodedData))
		chunk := encodedData[:chunkSize]
		encodedData = encodedData[chunkSize:]
		headerKey := fmt.Sprintf("%s-%d", key, i)
		header.Set(headerKey, chunk)
	}

	return header
}

func (c *Config) GetRequestCookiesWithPayload(payload []byte) []*http.Cookie {
	cookies := []*http.Cookie{}

	key := c.UplinkDataKey
	encodedData := base64.RawURLEncoding.EncodeToString(payload)

	for i := 0; len(encodedData) > 0; i++ {
		chunkSize := min(int(c.GetNormalizedUplinkChunkSize().rand()), len(encodedData))
		chunk := encodedData[:chunkSize]
		encodedData = encodedData[chunkSize:]
		cookieName := fmt.Sprintf("%s_%d", key, i)
		cookies = append(cookies, &http.Cookie{Name: cookieName, Value: chunk})
	}

	return cookies
}

func (c *Config) WriteResponseHeader(writer http.ResponseWriter, requestMethod string, requestHeader http.Header) {
	if origin := requestHeader.Get("Origin"); origin != "" {
		writer.Header().Set("Access-Control-Allow-Origin", origin)
		if c.GetNormalizedSessionPlacement() == PlacementCookie ||
			c.GetNormalizedSeqPlacement() == PlacementCookie ||
			c.GetNormalizedXPaddingPlacement() == PlacementCookie ||
			c.GetNormalizedUplinkDataPlacement() == PlacementCookie {
			writer.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		if requestMethod == "OPTIONS" {
			requestedMethod := requestHeader.Get("Access-Control-Request-Method")
			if requestedMethod != "" {
				writer.Header().Set("Access-Control-Allow-Methods", requestedMethod)
			} else {
				writer.Header().Set("Access-Control-Allow-Methods", "*")
			}

			requestedHeaders := requestHeader.Get("Access-Control-Request-Headers")
			if requestedHeaders == "" {
				writer.Header().Set("Access-Control-Allow-Headers", "*")
			} else {
				writer.Header().Set("Access-Control-Allow-Headers", requestedHeaders)
			}
		}
	}
}

func (c *Config) GetNormalizedUplinkHTTPMethod() string {
	if c.UplinkHTTPMethod == "" {
		return "POST"
	}

	return c.UplinkHTTPMethod
}

func (c *Config) GetNormalizedScMaxEachPostBytes() *RangeConfig {
	if c.ScMaxEachPostBytes == nil || c.ScMaxEachPostBytes.To == 0 {
		return &RangeConfig{
			From: 1000000,
			To:   1000000,
		}
	}

	return c.ScMaxEachPostBytes
}

func (c *Config) GetNormalizedScMinPostsIntervalMs() *RangeConfig {
	if c.ScMinPostsIntervalMs == nil || c.ScMinPostsIntervalMs.To == 0 {
		return &RangeConfig{
			From: 30,
			To:   30,
		}
	}

	return c.ScMinPostsIntervalMs
}

func (c *Config) GetNormalizedScMaxBufferedPosts() int {
	if c.ScMaxBufferedPosts == 0 {
		return 30
	}

	return int(c.ScMaxBufferedPosts)
}

func (c *Config) GetNormalizedScStreamUpServerSecs() *RangeConfig {
	if c.ScStreamUpServerSecs == nil || c.ScStreamUpServerSecs.To == 0 {
		return &RangeConfig{
			From: 20,
			To:   80,
		}
	}

	return c.ScStreamUpServerSecs
}

func (c *Config) GetNormalizedUplinkChunkSize() *RangeConfig {
	if c.UplinkChunkSize == nil || c.UplinkChunkSize.To == 0 {
		switch c.UplinkDataPlacement {
		case PlacementCookie:
			return &RangeConfig{
				From: 2 * 1024,
				To:   3 * 1024,
			}
		case PlacementHeader:
			return &RangeConfig{
				From: 3 * 1000,
				To:   4 * 1000,
			}
		default:
			return c.GetNormalizedScMaxEachPostBytes()
		}
	} else if c.UplinkChunkSize.From < 64 {
		return &RangeConfig{
			From: 64,
			To:   max(64, c.UplinkChunkSize.To),
		}
	}

	return c.UplinkChunkSize
}

func (c *Config) GetNormalizedServerMaxHeaderBytes() int {
	if c.ServerMaxHeaderBytes <= 0 {
		return 8192
	} else {
		return int(c.ServerMaxHeaderBytes)
	}
}

func (c *Config) GetNormalizedSessionPlacement() string {
	if c.SessionIDPlacement == "" {
		return PlacementPath
	}
	return c.SessionIDPlacement
}

func (c *Config) GetNormalizedSeqPlacement() string {
	if c.SeqPlacement == "" {
		return PlacementPath
	}
	return c.SeqPlacement
}

func (c *Config) GetNormalizedUplinkDataPlacement() string {
	if c.UplinkDataPlacement == "" {
		return PlacementBody
	}
	return c.UplinkDataPlacement
}

func (c *Config) GetNormalizedXPaddingPlacement() string {
	if c.XPaddingPlacement == "" {
		return PlacementQuery
	}
	return c.XPaddingPlacement
}

func (c *Config) GetNormalizedXPaddingKey() string {
	if c.XPaddingKey == "" {
		return "p"
	}
	return c.XPaddingKey
}

func (c *Config) GetNormalizedXPaddingHeader() string {
	if c.XPaddingHeader == "" {
		return "X-Pad"
	}
	return c.XPaddingHeader
}

func (c *Config) GetNormalizedXPaddingMethod() PaddingMethod {
	if c.XPaddingMethod == "" {
		return PaddingMethodTokenish
	}
	return PaddingMethod(c.XPaddingMethod)
}

func (c *Config) GetNormalizedSessionKey() string {
	if c.SessionIDKey != "" {
		return c.SessionIDKey
	}
	switch c.GetNormalizedSessionPlacement() {
	case PlacementHeader:
		return "X-Session"
	case PlacementCookie, PlacementQuery:
		return "x_session"
	default:
		return ""
	}
}

func (c *Config) GetNormalizedSeqKey() string {
	if c.SeqKey != "" {
		return c.SeqKey
	}
	switch c.GetNormalizedSeqPlacement() {
	case PlacementHeader:
		return "X-Seq"
	case PlacementCookie, PlacementQuery:
		return "x_seq"
	default:
		return ""
	}
}

func (c *Config) ApplyMetaToRequest(req *http.Request, sessionId string, seqStr string) {
	sessionPlacement := c.GetNormalizedSessionPlacement()
	seqPlacement := c.GetNormalizedSeqPlacement()
	sessionKey := c.GetNormalizedSessionKey()
	seqKey := c.GetNormalizedSeqKey()

	if sessionId != "" {
		switch sessionPlacement {
		case PlacementPath:
			req.URL.Path = appendToPath(req.URL.Path, sessionId)
		case PlacementQuery:
			q := req.URL.Query()
			q.Set(sessionKey, sessionId)
			req.URL.RawQuery = q.Encode()
		case PlacementHeader:
			req.Header.Set(sessionKey, sessionId)
		case PlacementCookie:
			req.AddCookie(&http.Cookie{Name: sessionKey, Value: sessionId})
		}
	}

	if seqStr != "" {
		switch seqPlacement {
		case PlacementPath:
			req.URL.Path = appendToPath(req.URL.Path, seqStr)
		case PlacementQuery:
			q := req.URL.Query()
			q.Set(seqKey, seqStr)
			req.URL.RawQuery = q.Encode()
		case PlacementHeader:
			req.Header.Set(seqKey, seqStr)
		case PlacementCookie:
			req.AddCookie(&http.Cookie{Name: seqKey, Value: seqStr})
		}
	}
}

func (c *Config) FillStreamRequest(request *http.Request, sessionId string, seqStr string) {
	request.Header = c.GetRequestHeader()
	c.ApplyXPaddingToRequest(request, c.newXPaddingConfig(request.URL.String()))
	c.ApplyMetaToRequest(request, sessionId, "")
}

func (c *Config) FillPacketRequest(request *http.Request, sessionId string, seqStr string, payload buf.MultiBuffer) error {
	dataPlacement := c.GetNormalizedUplinkDataPlacement()

	data := make([]byte, payload.Len())
	payload.Copy(data)
	buf.ReleaseMulti(payload)

	if dataPlacement == PlacementBody || dataPlacement == PlacementAuto {
		request.Header = c.GetRequestHeader()
		request.Body = io.NopCloser(bytes.NewReader(data))
		request.ContentLength = int64(len(data))
		request.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		}
	} else {
		switch dataPlacement {
		case PlacementHeader:
			request.Header = c.GetRequestHeaderWithPayload(data)
		case PlacementCookie:
			request.Header = c.GetRequestHeader()
			for _, cookie := range c.GetRequestCookiesWithPayload(data) {
				request.AddCookie(cookie)
			}
		}
	}

	c.ApplyXPaddingToRequest(request, c.newXPaddingConfig(request.URL.String()))
	c.ApplyMetaToRequest(request, sessionId, seqStr)

	return nil
}

func (c *Config) newXPaddingConfig(rawURL string) XPaddingConfig {
	config := XPaddingConfig{
		Length: int(c.GetNormalizedXPaddingBytes().rand()),
		Placement: XPaddingPlacement{
			Placement: c.GetNormalizedXPaddingPlacement(),
			Key:       c.GetNormalizedXPaddingKey(),
			Header:    c.GetNormalizedXPaddingHeader(),
			RawURL:    rawURL,
		},
	}
	if c.XPaddingObfsMode {
		config.Method = c.GetNormalizedXPaddingMethod()
	}
	return config
}

func (c *Config) ExtractMetaFromRequest(req *http.Request, path string) (sessionId string, seqStr string) {
	sessionPlacement := c.GetNormalizedSessionPlacement()
	seqPlacement := c.GetNormalizedSeqPlacement()
	sessionKey := c.GetNormalizedSessionKey()
	seqKey := c.GetNormalizedSeqKey()

	var subpath []string
	pathPart := 0
	if sessionPlacement == PlacementPath || seqPlacement == PlacementPath {
		subpath = strings.Split(req.URL.Path[len(path):], "/")
	}

	switch sessionPlacement {
	case PlacementPath:
		if len(subpath) > pathPart {
			sessionId = subpath[pathPart]
			pathPart += 1
		}
	case PlacementQuery:
		sessionId = req.URL.Query().Get(sessionKey)
	case PlacementHeader:
		sessionId = req.Header.Get(sessionKey)
	case PlacementCookie:
		if cookie, e := req.Cookie(sessionKey); e == nil {
			sessionId = cookie.Value
		}
	}

	switch seqPlacement {
	case PlacementPath:
		if len(subpath) > pathPart {
			seqStr = subpath[pathPart]
			pathPart += 1
		}
	case PlacementQuery:
		seqStr = req.URL.Query().Get(seqKey)
	case PlacementHeader:
		seqStr = req.Header.Get(seqKey)
	case PlacementCookie:
		if cookie, e := req.Cookie(seqKey); e == nil {
			seqStr = cookie.Value
		}
	}

	return sessionId, seqStr
}

func (m *XmuxConfig) GetNormalizedMaxConcurrency() *RangeConfig {
	if m.MaxConcurrency == nil {
		return &RangeConfig{
			From: 0,
			To:   0,
		}
	}

	return m.MaxConcurrency
}

func (m *XmuxConfig) GetNormalizedMaxConnections() *RangeConfig {
	if m.MaxConnections == nil {
		return &RangeConfig{
			From: 0,
			To:   0,
		}
	}

	return m.MaxConnections
}

func (m *XmuxConfig) GetNormalizedCMaxReuseTimes() *RangeConfig {
	if m.CMaxReuseTimes == nil {
		return &RangeConfig{
			From: 0,
			To:   0,
		}
	}

	return m.CMaxReuseTimes
}

func (m *XmuxConfig) GetNormalizedHMaxRequestTimes() *RangeConfig {
	if m.HMaxRequestTimes == nil {
		return &RangeConfig{
			From: 0,
			To:   0,
		}
	}

	return m.HMaxRequestTimes
}

func (m *XmuxConfig) GetNormalizedHMaxReusableSecs() *RangeConfig {
	if m.HMaxReusableSecs == nil {
		return &RangeConfig{
			From: 0,
			To:   0,
		}
	}

	return m.HMaxReusableSecs
}

func init() {
	common.Must(internet.RegisterProtocolConfigCreator(protocolName, func() interface{} {
		return new(Config)
	}))
}

func (c *RangeConfig) rand() int32 {
	if c == nil {
		return 0
	}
	return int32(crypto.RandBetween(int64(c.From), int64(c.To)))
}

var PredefinedTable = map[string]string{
	"ALPHABET": "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	"Alphabet": "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz",
	"BASE36":   "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	"Base62":   "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz",
	"HEX":      "0123456789ABCDEF",
	"alphabet": "abcdefghijklmnopqrstuvwxyz",
	"base36":   "0123456789abcdefghijklmnopqrstuvwxyz",
	"hex":      "0123456789abcdef",
	"number":   "0123456789",
}

func (c *Config) GenerateSessionID() string {
	length := c.SessionIDLength.rand()
	table := c.SessionIDTable
	if predefined, ok := PredefinedTable[table]; ok {
		table = predefined
	}
	if table == "" {
		table = PredefinedTable["Base62"]
		if length <= 0 {
			length = defaultSessionIDLength
		}
	}
	if length <= 0 {
		length = defaultSessionIDLength
	}
	if length > maxSessionIDLength {
		length = maxSessionIDLength
	}
	return generateRandomSessionID(length, table)
}

func generateRandomSessionID(length int32, table string) string {
	if length <= 0 || table == "" {
		return ""
	}
	id := make([]byte, length)
	limit := 256
	if len(table) < limit {
		limit -= 256 % len(table)
	}
	var randomBytes [256]byte
	for offset := 0; offset < len(id); {
		common.Must2(cryptorand.Read(randomBytes[:]))
		for _, value := range randomBytes {
			if int(value) >= limit {
				continue
			}
			id[offset] = table[int(value)%len(table)]
			offset++
			if offset == len(id) {
				return string(id)
			}
		}
	}
	return string(id)
}

func appendToPath(path, value string) string {
	if strings.HasSuffix(path, "/") {
		return path + value
	}
	return path + "/" + value
}
