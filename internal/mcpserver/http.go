package mcpserver

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Default limits for hardened HTTP transport.
const (
	DefaultMaxConcurrentRequests = 32
	DefaultSessionTimeout        = 15 * time.Minute
	DefaultToolTimeout           = 30 * time.Second
)

// NewToken makes a bearer token for the HTTP transport.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "wamcp_" + hex.EncodeToString(b), nil
}

// isLoopbackAddr verifies if an address binds to loopback interface.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

type requestLimiter struct {
	sem chan struct{}
}

func newRequestLimiter(max int) *requestLimiter {
	if max <= 0 {
		max = DefaultMaxConcurrentRequests
	}
	return &requestLimiter{sem: make(chan struct{}, max)}
}

func (l *requestLimiter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case l.sem <- struct{}{}:
			defer func() { <-l.sem }()
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Server overloaded: too many active requests"}}`)
		}
	})
}

// HTTPServer serves MCP over streamable HTTP, strictly guarded for local or authenticated proxy use.
func HTTPServer(srv *mcp.Server, addr, token, publicHost string) (*http.Server, error) {
	return HTTPServerWithConcurrency(srv, addr, token, publicHost, DefaultMaxConcurrentRequests)
}

// HTTPServerWithConcurrency builds an HTTPServer with custom concurrency limit.
func HTTPServerWithConcurrency(srv *mcp.Server, addr, token, publicHost string, maxConcurrent int) (*http.Server, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("refusing to serve HTTP without a token")
	}

	if !isLoopbackAddr(addr) {
		return nil, fmt.Errorf("refusing to bind HTTP server to non-loopback address %q in hardened private fork", addr)
	}

	opts := &mcp.StreamableHTTPOptions{
		DisableLocalhostProtection:   publicHost != "",
		MaxRequestBodyBytes:          4 << 20, // 4MB maximum
		SessionTimeout:               DefaultSessionTimeout,
		PropagateRequestCancellation: true,
	}

	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv }, opts,
	)

	mux := http.NewServeMux()

	// A health check that needs no token, so a tunnel or proxy can be tested
	// without handing the token to anything. It reveals nothing.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})

	limiter := newRequestLimiter(maxConcurrent)

	// Wrap handler with concurrency limit, Host, Origin, and Token protections
	guarded := limiter.wrap(requireHost(publicHost, requireOrigin(addr, publicHost, requireToken(token, handler))))
	mux.Handle("/", guarded)

	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}, nil
}

// requireToken checks the bearer token in constant time.
func requireToken(token string, next http.Handler) http.Handler {
	want := []byte(token)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		got := strings.TrimSpace(raw)

		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="whatsapp-mcp"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"jsonrpc":"2.0","error":{"code":-32001,"message":"Missing or invalid bearer token"}}`)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireHost enforces exact Host header matching against publicHost or loopback.
func requireHost(publicHost string, next http.Handler) http.Handler {
	publicHost = strings.ToLower(strings.TrimSpace(publicHost))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")

		allowed := false
		if publicHost != "" {
			allowed = (host == publicHost)
		} else {
			allowed = (host == "localhost" || host == "127.0.0.1" || host == "::1")
		}

		if !allowed {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireOrigin enforces strict canonical same-origin and trusted public HTTPS origin checks.
func requireOrigin(bindAddr, publicHost string, next http.Handler) http.Handler {
	publicHost = strings.ToLower(strings.TrimSpace(publicHost))
	_, bindPort, _ := net.SplitHostPort(bindAddr)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			// Machine client without Origin header (curl, cli, stdio proxy) is permitted
			next.ServeHTTP(w, r)
			return
		}

		u, err := url.Parse(origin)
		if err != nil || u.Hostname() == "" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		// Reject malformed origins with userinfo, paths, queries, or fragments
		if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		originHost := strings.ToLower(u.Hostname())
		originHost = strings.Trim(originHost, "[]")
		originPort := u.Port()
		originScheme := strings.ToLower(u.Scheme)

		if publicHost != "" {
			// Strict trusted expected public HTTPS origin
			if originScheme != "https" {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			expHost, expPort, err := net.SplitHostPort(publicHost)
			if err != nil {
				expHost = publicHost
				expPort = ""
			}
			expHost = strings.ToLower(strings.Trim(expHost, "[]"))

			if originHost != expHost {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			if expPort != "" && originPort != expPort {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			if expPort == "" && originPort != "" && originPort != "443" {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
		} else {
			// Pure loopback origin check: only allow localhost / 127.0.0.1 / ::1
			if originScheme != "http" && originScheme != "https" {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			if originHost != "localhost" && originHost != "127.0.0.1" && originHost != "::1" {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			// Enforce matching port if bind port is specified
			if bindPort != "" && originPort != "" && originPort != bindPort {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}
