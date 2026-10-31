package mcpserver

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNonLoopbackBindRejected(t *testing.T) {
	srv := New(Deps{})
	token := "wamcp_validtokentest12345678901234567890"

	badAddrs := []string{
		"0.0.0.0:8765",
		"192.168.1.100:8765",
		"10.0.0.1:8765",
		"::8765",
		"external.example.com:8765",
	}

	for _, addr := range badAddrs {
		_, err := HTTPServer(srv, addr, token, "")
		if err == nil {
			t.Errorf("expected non-loopback bind address %q to be rejected, got nil", addr)
		} else if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("expected error mentioning loopback for %q, got: %v", addr, err)
		}
	}
}

func TestHostHeaderProtection(t *testing.T) {
	srv := New(Deps{})
	token := "wamcp_validtokentest12345678901234567890"

	httpSrv, err := HTTPServer(srv, "127.0.0.1:8765", token, "")
	if err != nil {
		t.Fatalf("HTTPServer: %v", err)
	}

	// 1. Valid loopback Host
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Host = "127.0.0.1:8765"
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Errorf("valid loopback host was forbidden: %d", rec.Code)
	}

	// 2. Malicious Host
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Host = "evil.attacker.com"
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("malicious Host evil.attacker.com should be 403 Forbidden, got: %d", rec.Code)
	}
}

func TestOriginHeaderProtection(t *testing.T) {
	srv := New(Deps{})
	token := "wamcp_validtokentest12345678901234567890"

	httpSrv, err := HTTPServer(srv, "127.0.0.1:8765", token, "")
	if err != nil {
		t.Fatalf("HTTPServer: %v", err)
	}

	// 1. Machine client with no Origin header must be allowed
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Host = "127.0.0.1:8765"
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Errorf("machine client without Origin was forbidden: %d", rec.Code)
	}

	// 2. Malicious cross-origin Origin header must be rejected
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Host = "127.0.0.1:8765"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", "https://malicious-website.com")
	rec = httptest.NewRecorder()
	httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("untrusted Origin https://malicious-website.com should be 403 Forbidden, got: %d", rec.Code)
	}
}

func TestRequestBodyBudgetEnforced(t *testing.T) {
	srv := New(Deps{})
	token := "wamcp_validtokentest12345678901234567890"

	httpSrv, err := HTTPServer(srv, "127.0.0.1:8765", token, "")
	if err != nil {
		t.Fatalf("HTTPServer: %v", err)
	}

	// Send 5MB payload (exceeding 4MB limit)
	hugePayload := bytes.Repeat([]byte("A"), 5<<20)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(hugePayload))
	req.Host = "127.0.0.1:8765"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	httpSrv.Handler.ServeHTTP(rec, req)

	// Should either return 413 Payload Too Large or fail with error
	if rec.Code == http.StatusOK {
		t.Errorf("5MB payload should not succeed with 200 OK")
	}
}

func TestOriginStrictPublicHostValidation(t *testing.T) {
	srv := New(Deps{})
	token := "wamcp_validtokentest12345678901234567890"

	httpSrv, err := HTTPServer(srv, "127.0.0.1:8765", token, "mcp.example.com")
	if err != nil {
		t.Fatalf("HTTPServer: %v", err)
	}

	tests := []struct {
		name       string
		origin     string
		wantStatus int
	}{
		{"valid expected public https origin", "https://mcp.example.com", http.StatusOK},
		{"insecure http scheme rejected", "http://mcp.example.com", http.StatusForbidden},
		{"foreign port rejected", "https://mcp.example.com:8443", http.StatusForbidden},
		{"localhost origin rejected when publicHost configured", "http://localhost:8765", http.StatusForbidden},
		{"userinfo in origin rejected", "https://user:pass@mcp.example.com", http.StatusForbidden},
		{"path in origin rejected", "https://mcp.example.com/path", http.StatusForbidden},
		{"untrusted domain rejected", "https://attacker.com", http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
			req.Host = "mcp.example.com"
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Origin", tt.origin)
			rec := httptest.NewRecorder()
			httpSrv.Handler.ServeHTTP(rec, req)

			if tt.wantStatus == http.StatusOK && rec.Code == http.StatusForbidden {
				t.Errorf("origin %q should be permitted, got 403", tt.origin)
			}
			if tt.wantStatus == http.StatusForbidden && rec.Code != http.StatusForbidden {
				t.Errorf("origin %q should be 403 Forbidden, got %d", tt.origin, rec.Code)
			}
		})
	}
}

func TestOriginLoopbackPortProtection(t *testing.T) {
	srv := New(Deps{})
	token := "wamcp_validtokentest12345678901234567890"

	httpSrv, err := HTTPServer(srv, "127.0.0.1:8765", token, "")
	if err != nil {
		t.Fatalf("HTTPServer: %v", err)
	}

	// 1. Matching loopback port allowed
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Host = "127.0.0.1:8765"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", "http://127.0.0.1:8765")
	rec := httptest.NewRecorder()
	httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Errorf("matching loopback origin was forbidden: %d", rec.Code)
	}

	// 2. Cross-port loopback origin rejected
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Host = "127.0.0.1:8765"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", "http://127.0.0.1:9999")
	rec = httptest.NewRecorder()
	httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-port loopback origin http://127.0.0.1:9999 should be 403 Forbidden, got: %d", rec.Code)
	}
}

func TestRequestConcurrencyLimitAndPromptOverloadRejection(t *testing.T) {
	srv := New(Deps{})
	token := "wamcp_validtokentest12345678901234567890"

	// Create server with concurrency capacity of 1
	httpSrv, err := HTTPServerWithConcurrency(srv, "127.0.0.1:8765", token, "", 1)
	if err != nil {
		t.Fatalf("HTTPServerWithConcurrency: %v", err)
	}

	// Fill the single slot in concurrency limiter
	limiter := newRequestLimiter(1)
	limiter.sem <- struct{}{} // occupied!

	wrapped := limiter.wrap(httpSrv.Handler)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Host = "127.0.0.1:8765"
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests on overload, got: %d", rec.Code)
	}
}
