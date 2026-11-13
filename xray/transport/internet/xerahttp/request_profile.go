package xerahttp

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/xtls/xray-core/common/utils"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/reality"
	"github.com/xtls/xray-core/transport/internet/tls"
	"google.golang.org/protobuf/proto"
)

var browserMajorVersion = regexp.MustCompile(`(Chrome|Firefox|Edg)/[0-9]+`)
var firefoxRevisionVersion = regexp.MustCompile(`\brv:[0-9]+`)

func configForHTTPClient(settings *internet.MemoryStreamConfig) *Config {
	profile := ""
	fingerprintName := ""
	version := ""
	if config := tls.ConfigFromStreamSettings(settings); config != nil {
		fingerprintName = config.Fingerprint
	}
	if config := reality.ConfigFromStreamSettings(settings); config != nil {
		fingerprintName = config.Fingerprint
	}
	if fingerprint := tls.GetFingerprint(fingerprintName); fingerprint != nil {
		switch strings.ToLower(fingerprint.Client) {
		case "chrome", "firefox", "safari", "edge":
			profile = strings.ToLower(fingerprint.Client)
			if _, err := strconv.ParseUint(fingerprint.Version, 10, 16); err == nil {
				version = fingerprint.Version
			}
		case "ios":
			profile = "safari"
		}
	}
	config := proto.Clone(settings.ProtocolSettings.(*Config)).(*Config)
	headers := config.requestHeaders(profile)
	if version != "" && profile != "safari" {
		alignHTTPVersion(headers, config.Headers, profile, version)
	}
	config.Headers = make(map[string]string, len(headers))
	for name := range headers {
		config.Headers[name] = headers.Get(name)
	}
	for name, value := range settings.ProtocolSettings.(*Config).Headers {
		if strings.EqualFold(name, "User-Agent") && value == "golang" {
			config.Headers["User-Agent"] = "golang"
		}
	}
	return config
}

func alignHTTPVersion(headers http.Header, explicit map[string]string, profile, version string) {
	for name, value := range explicit {
		if strings.EqualFold(name, "User-Agent") && value != profile {
			return
		}
	}
	headers.Set("User-Agent", browserMajorVersion.ReplaceAllStringFunc(headers.Get("User-Agent"), func(value string) string {
		return value[:strings.IndexByte(value, '/')+1] + version
	}))
	if profile == "firefox" {
		headers.Set("User-Agent", firefoxRevisionVersion.ReplaceAllString(headers.Get("User-Agent"), "rv:"+version))
	}
	for name := range explicit {
		if strings.EqualFold(name, "Sec-CH-UA") {
			return
		}
	}
	if hints := headers.Get("Sec-CH-UA"); hints != "" && (profile == "chrome" || profile == "edge") {
		if major, err := strconv.ParseUint(version, 10, 16); err == nil && major > 0 {
			headers.Set("Sec-CH-UA", utils.BrowserClientHints(int(major), profile))
		}
	}
}
