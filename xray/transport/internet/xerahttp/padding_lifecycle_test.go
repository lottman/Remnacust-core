package xerahttp

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/http2/hpack"
)

func TestTokenishPaddingEncodedLength(t *testing.T) {
	for _, size := range []int{1, 2, 3, 16, 100, 1000, 8192, 65536} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			for range 8 {
				value := GenerateTokenishPaddingBase62(size)
				got := int(hpack.HuffmanEncodeLength(value))
				if got < size-2 || got > size+2 {
					t.Fatalf("encoded length = %d, want %d within tolerance", got, size)
				}
				if strings.Trim(value, charsetBase62) != "" {
					t.Fatal("padding contains non-base62 characters")
				}
			}
		})
	}
}

func TestQueryPaddingResponseUsesConfiguredHeader(t *testing.T) {
	c := &Config{}
	config := XPaddingConfig{
		Length:    128,
		Method:    PaddingMethodTokenish,
		Placement: XPaddingPlacement{Placement: PlacementQuery, Key: "token", Header: "X-Response-Token"},
	}
	request := httptest.NewRequest("GET", "https://example.com/?keep=yes", nil)
	c.ApplyXPaddingToRequest(request, config)
	if request.URL.Query().Get("keep") != "yes" || request.URL.Query().Get("token") == "" {
		t.Fatal("request query padding missing or original query lost")
	}
	response := httptest.NewRecorder()
	c.ApplyXPaddingToResponse(response, config)
	value := response.Header().Get("X-Response-Token")
	if !c.IsPaddingValid(value, 128, 128, PaddingMethodTokenish) {
		t.Fatal("response padding missing or invalid")
	}
	if response.Body.Len() != 0 {
		t.Fatal("response padding corrupted stream body")
	}
}

func BenchmarkTokenishPadding(b *testing.B) {
	for _, size := range []int{100, 1000, 8192} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				GenerateTokenishPaddingBase62(size)
			}
		})
	}
}
