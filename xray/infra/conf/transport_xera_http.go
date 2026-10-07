package conf

import (
	"encoding/json"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/transport/internet/xerahttp"
	"google.golang.org/protobuf/proto"
	"strings"
)

type XeraHTTPConfig struct {
	ServerMaxBufferedBytes int64                      `json:"serverMaxBufferedBytes"`
	ServerMaxSessions      uint32                     `json:"serverMaxSessions"`
	CustomDownlinkPadding  *XeraDownlinkPaddingConfig `json:"customDownlinkPadding"`
	Host                   string                     `json:"host"`
	Path                   string                     `json:"path"`
	Mode                   string                     `json:"mode"`
	Headers                map[string]string          `json:"headers"`
	XPaddingBytes          Int32Range                 `json:"xPaddingBytes"`
	XPaddingObfsMode       bool                       `json:"xPaddingObfsMode"`
	XPaddingKey            string                     `json:"xPaddingKey"`
	XPaddingHeader         string                     `json:"xPaddingHeader"`
	XPaddingPlacement      string                     `json:"xPaddingPlacement"`
	XPaddingMethod         string                     `json:"xPaddingMethod"`
	UplinkHTTPMethod       string                     `json:"uplinkHTTPMethod"`
	SessionIDPlacement     string                     `json:"sessionIDPlacement"`
	SessionIDKey           string                     `json:"sessionIDKey"`
	SessionIDTable         string                     `json:"sessionIDTable"`
	SessionIDLength        Int32Range                 `json:"sessionIDLength"`
	SeqPlacement           string                     `json:"seqPlacement"`
	SeqKey                 string                     `json:"seqKey"`
	UplinkDataPlacement    string                     `json:"uplinkDataPlacement"`
	UplinkDataKey          string                     `json:"uplinkDataKey"`
	UplinkChunkSize        Int32Range                 `json:"uplinkChunkSize"`
	NoGRPCHeader           *bool                      `json:"noGRPCHeader"`
	NoSSEHeader            *bool                      `json:"noSSEHeader"`
	ScMaxEachPostBytes     Int32Range                 `json:"scMaxEachPostBytes"`
	ScMinPostsIntervalMs   Int32Range                 `json:"scMinPostsIntervalMs"`
	ScMaxBufferedPosts     int64                      `json:"scMaxBufferedPosts"`
	ScStreamUpServerSecs   Int32Range                 `json:"scStreamUpServerSecs"`
	ServerMaxHeaderBytes   int32                      `json:"serverMaxHeaderBytes"`
	Xmux                   XeraMuxConfig              `json:"xmux"`
	DownloadSettings       *StreamConfig              `json:"downloadSettings"`
	Extra                  json.RawMessage            `json:"extra"`
}

type XeraDownlinkPaddingConfig struct {
	BudgetPercent uint32      `json:"budgetPercent"`
	BurstBytes    uint32      `json:"burstBytes"`
	Uplink        bool        `json:"uplink"`
	Header        string      `json:"header"`
	Token         string      `json:"token"`
	Bytes         *Int32Range `json:"bytes"`
	BlockBytes    *Int32Range `json:"blockBytes"`
}

type XeraMuxConfig struct {
	MaxConcurrency   Int32Range `json:"maxConcurrency"`
	MaxConnections   Int32Range `json:"maxConnections"`
	CMaxReuseTimes   Int32Range `json:"cMaxReuseTimes"`
	HMaxRequestTimes Int32Range `json:"hMaxRequestTimes"`
	HMaxReusableSecs Int32Range `json:"hMaxReusableSecs"`
	HKeepAlivePeriod int64      `json:"hKeepAlivePeriod"`
}

const (
	maxXeraSessionIDLength      = 256
	maxXeraSessionIDTableLength = 256
	maxXeraServerSessions       = 65536
)

func newXeraRangeConfig(input Int32Range) *xerahttp.RangeConfig {
	return &xerahttp.RangeConfig{
		From: input.From,
		To:   input.To,
	}
}

func validateXeraRange(name string, value Int32Range, allowNegative bool) error {
	if value == (Int32Range{}) {
		return nil
	}
	if value.From > value.To {
		return errors.New(name + " range must have from <= to")
	}
	if !allowNegative && value.From < 0 {
		return errors.New(name + " range cannot be negative")
	}
	return nil
}

func (c *XeraHTTPConfig) Build() (proto.Message, error) {
	if c.Extra != nil {
		var extra XeraHTTPConfig
		if err := json.Unmarshal(c.Extra, &extra); err != nil {
			return nil, errors.New(`Failed to unmarshal "extra".`).Base(err)
		}
		extra.Host = c.Host
		extra.Path = c.Path
		extra.Mode = c.Mode
		c = &extra
	}
	var downlinkPadding *xerahttp.DownlinkPaddingConfig
	if c.ServerMaxBufferedBytes < 0 {
		return nil, errors.New("serverMaxBufferedBytes must not be negative")
	}
	if c.ServerMaxSessions > maxXeraServerSessions {
		return nil, errors.New("serverMaxSessions exceeds the safe limit")
	}
	for _, item := range []struct {
		name          string
		value         Int32Range
		allowNegative bool
	}{
		{"xPaddingBytes", c.XPaddingBytes, false},
		{"sessionIDLength", c.SessionIDLength, false},
		{"uplinkChunkSize", c.UplinkChunkSize, false},
		{"scMaxEachPostBytes", c.ScMaxEachPostBytes, false},
		{"scMinPostsIntervalMs", c.ScMinPostsIntervalMs, true},
		{"scStreamUpServerSecs", c.ScStreamUpServerSecs, false},
		{"xmux.maxConcurrency", c.Xmux.MaxConcurrency, false},
		{"xmux.maxConnections", c.Xmux.MaxConnections, false},
		{"xmux.cMaxReuseTimes", c.Xmux.CMaxReuseTimes, false},
		{"xmux.hMaxRequestTimes", c.Xmux.HMaxRequestTimes, false},
		{"xmux.hMaxReusableSecs", c.Xmux.HMaxReusableSecs, false},
	} {
		if err := validateXeraRange(item.name, item.value, item.allowNegative); err != nil {
			return nil, err
		}
	}
	if p := c.CustomDownlinkPadding; p != nil {
		downlinkPadding = &xerahttp.DownlinkPaddingConfig{Header: p.Header, Token: p.Token, BudgetPercent: p.BudgetPercent, BurstBytes: p.BurstBytes, Uplink: p.Uplink}
		if p.Bytes != nil {
			downlinkPadding.Bytes = newXeraRangeConfig(*p.Bytes)
		}
		if p.BlockBytes != nil {
			downlinkPadding.BlockBytes = newXeraRangeConfig(*p.BlockBytes)
		}
		if err := downlinkPadding.Validate(); err != nil {
			return nil, err
		}
	}

	switch c.Mode {
	case "":
		c.Mode = "auto"
	case xerahttp.ModeAuto, xerahttp.ModeStreamAuto, xerahttp.ModePacketUp, xerahttp.ModeStreamUp, xerahttp.ModeStreamOne:
	default:
		return nil, errors.New("unsupported mode: " + c.Mode)
	}
	for k := range c.Headers {
		if strings.ToLower(k) == "host" {
			return nil, errors.New(`"headers" can't contain "host"`)
		}
	}

	if c.XPaddingBytes != (Int32Range{}) && (c.XPaddingBytes.From <= 0 || c.XPaddingBytes.To <= 0) {
		return nil, errors.New("xPaddingBytes cannot be disabled")
	}

	if c.XPaddingKey == "" {
		c.XPaddingKey = "p"
	}

	if c.XPaddingHeader == "" {
		c.XPaddingHeader = "X-Pad"
	}

	switch c.XPaddingPlacement {
	case "":
		c.XPaddingPlacement = "query"
	case "cookie", "header", "query", "queryInHeader":
	default:
		return nil, errors.New("unsupported padding placement: " + c.XPaddingPlacement)
	}

	switch c.XPaddingMethod {
	case "":
		c.XPaddingMethod = "tokenish"
	case "repeat-x", "tokenish":
	default:
		return nil, errors.New("unsupported padding method: " + c.XPaddingMethod)
	}

	switch c.UplinkDataPlacement {
	case "":
		c.UplinkDataPlacement = xerahttp.PlacementAuto
		if downlinkPadding != nil && downlinkPadding.Uplink {
			c.UplinkDataPlacement = xerahttp.PlacementBody
		}
	case xerahttp.PlacementAuto, xerahttp.PlacementBody:
	case xerahttp.PlacementCookie, xerahttp.PlacementHeader:
		if c.Mode != "packet-up" {
			return nil, errors.New("UplinkDataPlacement can be " + c.UplinkDataPlacement + " only in packet-up mode")
		}
	default:
		return nil, errors.New("unsupported uplink data placement: " + c.UplinkDataPlacement)
	}

	if c.UplinkHTTPMethod == "" {
		c.UplinkHTTPMethod = "POST"
	}
	c.UplinkHTTPMethod = strings.ToUpper(c.UplinkHTTPMethod)

	if c.UplinkHTTPMethod == "GET" && c.Mode != "packet-up" {
		return nil, errors.New("uplinkHTTPMethod can be GET only in packet-up mode")
	}

	switch c.SessionIDPlacement {
	case "":
		c.SessionIDPlacement = "path"
	case "path", "cookie", "header", "query":
	default:
		return nil, errors.New("unsupported session placement: " + c.SessionIDPlacement)
	}

	switch c.SeqPlacement {
	case "":
		c.SeqPlacement = "path"
	case "path", "cookie", "header", "query":
	default:
		return nil, errors.New("unsupported seq placement: " + c.SeqPlacement)
	}

	if c.SessionIDPlacement != "path" && c.SessionIDKey == "" {
		switch c.SessionIDPlacement {
		case "cookie", "query":
			c.SessionIDKey = "x_session"
		case "header":
			c.SessionIDKey = "X-Session"
		}
	}

	if c.SessionIDTable != "" {
		if len(c.SessionIDTable) > maxXeraSessionIDTableLength {
			return nil, errors.New("sessionIDTable exceeds the safe limit")
		}
		if c.SessionIDLength.To > maxXeraSessionIDLength {
			return nil, errors.New("sessionIDLength exceeds the safe limit")
		}
		if predefined, ok := xerahttp.PredefinedTable[c.SessionIDTable]; ok {
			c.SessionIDTable = predefined
		}
		if c.SessionIDLength.From <= 0 {
			return nil, errors.New("sessionIDLength.from must be greater than 0")
		}
		for i := 0; i < len(c.SessionIDTable); i++ {
			if c.SessionIDTable[i] >= 0x80 {
				return nil, errors.New("sessionIDTable must contain only ASCII characters")
			}
		}
	}

	if c.SeqPlacement != "path" && c.SeqKey == "" {
		switch c.SeqPlacement {
		case "cookie", "query":
			c.SeqKey = "x_seq"
		case "header":
			c.SeqKey = "X-Seq"
		}
	}

	if c.UplinkDataPlacement != xerahttp.PlacementBody && c.UplinkDataKey == "" {
		switch c.UplinkDataPlacement {
		case xerahttp.PlacementCookie:
			c.UplinkDataKey = "x_data"
		case xerahttp.PlacementAuto, xerahttp.PlacementHeader:
			c.UplinkDataKey = "X-Data"
		}
	}

	if c.ServerMaxHeaderBytes < 0 {
		return nil, errors.New("invalid negative value of maxHeaderBytes")
	}

	if c.Xmux.MaxConnections.To > 0 && c.Xmux.MaxConcurrency.To > 0 {
		return nil, errors.New("maxConnections cannot be specified together with maxConcurrency")
	}
	if c.Xmux == (XeraMuxConfig{}) {
		c.Xmux.MaxConnections.From = 3
		c.Xmux.MaxConnections.To = 3
		c.Xmux.HMaxRequestTimes.From = 600
		c.Xmux.HMaxRequestTimes.To = 900
		c.Xmux.HMaxReusableSecs.From = 1800
		c.Xmux.HMaxReusableSecs.To = 3000
	}

	if downlinkPadding != nil {
		if downlinkPadding.Uplink && c.UplinkDataPlacement != "" && c.UplinkDataPlacement != "body" {
			return nil, errors.New("custom uplink padding requires body placement")
		}
		for _, name := range []string{c.XPaddingHeader, "X-Padding", "X-Accel-Buffering", c.SessionIDKey, c.SeqKey, c.UplinkDataKey} {
			if strings.EqualFold(name, downlinkPadding.Header) {
				return nil, errors.New("customDownlinkPadding header conflicts with XERA-HTTP metadata")
			}
		}
	}
	noGRPCHeader := true
	if c.NoGRPCHeader != nil {
		noGRPCHeader = *c.NoGRPCHeader
	}
	noSSEHeader := true
	if c.NoSSEHeader != nil {
		noSSEHeader = *c.NoSSEHeader
	}
	config := &xerahttp.Config{
		ServerMaxBufferedBytes: c.ServerMaxBufferedBytes,
		ServerMaxSessions:      c.ServerMaxSessions,
		Host:                   c.Host,
		Path:                   c.Path,
		Mode:                   c.Mode,
		Headers:                c.Headers,
		XPaddingBytes:          newXeraRangeConfig(c.XPaddingBytes),
		XPaddingObfsMode:       c.XPaddingObfsMode,
		XPaddingKey:            c.XPaddingKey,
		XPaddingHeader:         c.XPaddingHeader,
		XPaddingPlacement:      c.XPaddingPlacement,
		CustomDownlinkPadding:  downlinkPadding,
		XPaddingMethod:         c.XPaddingMethod,
		UplinkHTTPMethod:       c.UplinkHTTPMethod,
		SessionIDPlacement:     c.SessionIDPlacement,
		SeqPlacement:           c.SeqPlacement,
		SessionIDKey:           c.SessionIDKey,
		SeqKey:                 c.SeqKey,
		UplinkDataPlacement:    c.UplinkDataPlacement,
		UplinkDataKey:          c.UplinkDataKey,
		UplinkChunkSize:        newXeraRangeConfig(c.UplinkChunkSize),
		NoGRPCHeader:           noGRPCHeader,
		NoSSEHeader:            noSSEHeader,
		ScMaxEachPostBytes:     newXeraRangeConfig(c.ScMaxEachPostBytes),
		ScMinPostsIntervalMs:   newXeraRangeConfig(c.ScMinPostsIntervalMs),
		ScMaxBufferedPosts:     c.ScMaxBufferedPosts,
		ScStreamUpServerSecs:   newXeraRangeConfig(c.ScStreamUpServerSecs),
		ServerMaxHeaderBytes:   c.ServerMaxHeaderBytes,
		SessionIDTable:         c.SessionIDTable,
		SessionIDLength:        newXeraRangeConfig(c.SessionIDLength),
		Xmux: &xerahttp.XmuxConfig{
			MaxConcurrency:   newXeraRangeConfig(c.Xmux.MaxConcurrency),
			MaxConnections:   newXeraRangeConfig(c.Xmux.MaxConnections),
			CMaxReuseTimes:   newXeraRangeConfig(c.Xmux.CMaxReuseTimes),
			HMaxRequestTimes: newXeraRangeConfig(c.Xmux.HMaxRequestTimes),
			HMaxReusableSecs: newXeraRangeConfig(c.Xmux.HMaxReusableSecs),
			HKeepAlivePeriod: c.Xmux.HKeepAlivePeriod,
		},
	}

	if c.DownloadSettings != nil {
		if c.Mode == "stream-one" {
			return nil, errors.New(`Can not use "downloadSettings" in "stream-one" mode.`)
		}
		var err error
		if config.DownloadSettings, err = c.DownloadSettings.Build(); err != nil {
			return nil, errors.New(`Failed to build "downloadSettings".`).Base(err)
		}
		if config.DownloadSettings.ProtocolName != "xera-http" {
			return nil, errors.New(`XERA-HTTP downloadSettings must use network "xera-http"`)
		}
		if config.DownloadSettings.Address == nil || config.DownloadSettings.Port == 0 {
			return nil, errors.New(`XERA-HTTP downloadSettings require an address and a nonzero port`)
		}
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}

	return config, nil
}
