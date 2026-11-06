package xerahttp

import "net/http"

func (h *requestHandler) acceptsMethod(method string) bool {
	return method == http.MethodGet || method == h.config.GetNormalizedUplinkHTTPMethod()
}

func (h *requestHandler) rejectRequest(writer http.ResponseWriter, request *http.Request) {
	for _, name := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials", "Access-Control-Allow-Methods", "Access-Control-Allow-Headers", "Cache-Control", "Vary", h.config.GetNormalizedXPaddingHeader()} {
		writer.Header().Del(name)
	}
	if h.config.CustomDownlinkPadding != nil {
		writer.Header().Del(h.config.CustomDownlinkPadding.Header)
	}
	writer.Header().Del("Set-Cookie")
	http.NotFound(writer, request)
}
