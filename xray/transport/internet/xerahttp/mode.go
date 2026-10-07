package xerahttp

const (
	ModeAuto       = "auto"
	ModeStreamAuto = "stream-auto"
	ModeStreamOne  = "stream-one"
	ModeStreamUp   = "stream-up"
	ModePacketUp   = "packet-up"
)

func resolveMode(configuredMode, httpVersion string, isReality, hasDownloadSettings bool) string {
	switch configuredMode {
	case "", ModeAuto:
		return resolveLegacyMode(isReality, hasDownloadSettings)
	case ModeStreamAuto:
		return resolveStreamAutoMode(httpVersion, isReality, hasDownloadSettings)
	default:
		return configuredMode
	}
}

func resolveLegacyMode(isReality, hasDownloadSettings bool) string {
	if !isReality {
		return ModePacketUp
	}
	if hasDownloadSettings {
		return ModeStreamUp
	}
	return ModeStreamOne
}

func resolveStreamAutoMode(httpVersion string, isReality, hasDownloadSettings bool) string {
	// ALPN describes the edge connection, not how a CDN forwards request bodies.
	// Finite uploads work with buffering intermediaries. Direct deployments can
	// explicitly opt into stream-up; REALITY cannot pass through a TLS CDN.
	if httpVersion == "2" && isReality {
		if !hasDownloadSettings {
			return ModeStreamOne
		}
		return ModeStreamUp
	}
	return ModePacketUp
}

func isModeAllowed(configuredMode, requestMode string) bool {
	return configuredMode == "" || configuredMode == ModeAuto || configuredMode == ModeStreamAuto || configuredMode == requestMode
}
