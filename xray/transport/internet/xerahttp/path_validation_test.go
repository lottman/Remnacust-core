package xerahttp

import "testing"

func TestValidRequestPath(t *testing.T) {
	tests := []struct {
		name       string
		request    string
		configured string
		valid      bool
	}{
		{name: "root", request: "/anything", configured: "/", valid: true},
		{name: "exact file path", request: "/api/socket.js", configured: "/api/socket.js", valid: true},
		{name: "file path suffix is rejected", request: "/api/socket.js.evil", configured: "/api/socket.js", valid: false},
		{name: "path segment is accepted", request: "/api/socket.js/session", configured: "/api/socket.js/", valid: true},
		{name: "similar segment is rejected", request: "/api/socket.js.evil/session", configured: "/api/socket.js/", valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isValidRequestPath(test.request, test.configured); got != test.valid {
				t.Fatalf("isValidRequestPath(%q, %q) = %v, want %v", test.request, test.configured, got, test.valid)
			}
		})
	}
}
