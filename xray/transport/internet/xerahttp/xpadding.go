package xerahttp

import (
	"crypto/rand"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/http2/hpack"
)

type PaddingMethod string

const PaddingMethodTokenish PaddingMethod = "tokenish"

const charsetBase62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var base62HuffmanBits = func() [len(charsetBase62)]int {
	var bits [len(charsetBase62)]int
	for i, value := range []byte(charsetBase62) {
		bits[i] = int(hpack.HuffmanEncodeLength(strings.Repeat(string(value), 8)))
	}
	return bits
}()

const validationTolerance = 2

type XPaddingPlacement struct {
	Placement string
	Key       string
	Header    string
	RawURL    string
}

type XPaddingConfig struct {
	Length    int
	Placement XPaddingPlacement
	Method    PaddingMethod
}

func GenerateTokenishPaddingBase62(targetHuffmanBytes int) string {
	if targetHuffmanBytes <= 0 || targetHuffmanBytes > maxXPaddingBytes {
		return ""
	}
	targetBits := targetHuffmanBytes * 8
	limit := byte(256 - (256 % len(charsetBase62)))
	result := make([]byte, 0, (targetBits+4)/5)
	encodedBits := 0
	var randomBytes [256]byte
	for encodedBits < targetBits-7 {
		if _, err := rand.Read(randomBytes[:]); err != nil {
			return ""
		}
		for _, value := range randomBytes {
			if value >= limit {
				continue
			}
			index := int(value) % len(charsetBase62)
			result = append(result, charsetBase62[index])
			encodedBits += base62HuffmanBits[index]
			if encodedBits >= targetBits-7 {
				break
			}
		}
	}
	return string(result)
}

func GeneratePadding(method PaddingMethod, length int) string {
	if length <= 0 {
		return ""
	}

	switch method {
	case PaddingMethodTokenish:
		return GenerateTokenishPaddingBase62(length)
	default:
		return ""
	}
}

func ApplyPaddingToCookie(req *http.Request, name, value string) {
	if req == nil || name == "" || value == "" {
		return
	}
	req.AddCookie(&http.Cookie{
		Name:  name,
		Value: value,
		Path:  "/",
	})
}

func ApplyPaddingToResponseCookie(writer http.ResponseWriter, name, value string) {
	if name == "" || value == "" {
		return
	}
	http.SetCookie(writer, &http.Cookie{
		Name:  name,
		Value: value,
		Path:  "/",
	})
}

func ApplyPaddingToQuery(u *url.URL, key, value string) {
	if u == nil || key == "" || value == "" {
		return
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
}

func (c *Config) GetNormalizedXPaddingBytes() *RangeConfig {
	if c.XPaddingBytes == nil || c.XPaddingBytes.To == 0 {
		return &RangeConfig{
			From: 100,
			To:   1000,
		}
	}

	return c.XPaddingBytes
}

func (c *Config) ApplyXPaddingToHeader(h http.Header, config XPaddingConfig) {
	if h == nil {
		return
	}

	paddingValue := GeneratePadding(config.Method, config.Length)

	switch p := config.Placement; p.Placement {
	case PlacementHeader:
		h.Set(p.Header, paddingValue)
	case PlacementQueryInHeader:
		u, err := url.Parse(p.RawURL)
		if err != nil || u == nil {
			return
		}
		u.RawQuery = p.Key + "=" + paddingValue
		h.Set(p.Header, u.String())
	}
}

func (c *Config) ApplyXPaddingToRequest(req *http.Request, config XPaddingConfig) {
	if req == nil {
		return
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}

	placement := config.Placement.Placement

	if placement == PlacementHeader || placement == PlacementQueryInHeader {
		c.ApplyXPaddingToHeader(req.Header, config)
		return
	}

	paddingValue := GeneratePadding(config.Method, config.Length)

	switch placement {
	case PlacementCookie:
		ApplyPaddingToCookie(req, config.Placement.Key, paddingValue)
	case PlacementQuery:
		ApplyPaddingToQuery(req.URL, config.Placement.Key, paddingValue)
	}
}

func (c *Config) ApplyXPaddingToResponse(writer http.ResponseWriter, config XPaddingConfig) {
	placement := config.Placement.Placement
	if placement == PlacementQuery {
		placement = PlacementHeader
		config.Placement.Placement = placement
		if config.Placement.Header == "" {
			config.Placement.Header = c.GetNormalizedXPaddingHeader()
		}
	}

	if placement == PlacementHeader || placement == PlacementQueryInHeader {
		c.ApplyXPaddingToHeader(writer.Header(), config)
		return
	}

	paddingValue := GeneratePadding(config.Method, config.Length)

	switch placement {
	case PlacementCookie:
		ApplyPaddingToResponseCookie(writer, config.Placement.Key, paddingValue)
	}
}

func (c *Config) ExtractXPaddingFromRequest(req *http.Request) (string, string) {
	if req == nil {
		return "", ""
	}

	key := c.GetNormalizedXPaddingKey()
	header := c.GetNormalizedXPaddingHeader()
	switch c.GetNormalizedXPaddingPlacement() {
	case PlacementQueryInHeader:
		headerValue := req.Header.Get(header)
		if headerValue != "" {
			if referrerURL, err := url.Parse(headerValue); err == nil {
				return referrerURL.Query().Get(key), PlacementQueryInHeader + "=" + header + ", key=" + key
			}
		}
	case PlacementHeader:
		return req.Header.Get(header), PlacementHeader + "=" + header
	case PlacementCookie:
		if cookie, err := req.Cookie(key); err == nil && cookie != nil {
			return cookie.Value, PlacementCookie + ", key=" + key
		}
	default:
		return req.URL.Query().Get(key), PlacementQuery + ", key=" + key
	}
	return "", ""
}

func (c *Config) IsPaddingValid(paddingValue string, from, to int32, method PaddingMethod) bool {
	if paddingValue == "" {
		return false
	}
	if to <= 0 {
		r := c.GetNormalizedXPaddingBytes()
		from, to = r.From, r.To
	}

	switch method {
	case PaddingMethodTokenish:
		if strings.Trim(paddingValue, charsetBase62) != "" {
			return false
		}
		const tolerance = int32(validationTolerance)

		n := int32(hpack.HuffmanEncodeLength(paddingValue))
		f := from - tolerance
		t := to + tolerance
		if f < 0 {
			f = 0
		}
		return n >= f && n <= t
	default:
		return false
	}
}
