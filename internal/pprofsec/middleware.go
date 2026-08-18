// Package pprofsec provides a 3-layer security middleware for Go's
// net/http/pprof endpoints. It is intentionally tiny and importable by
// tests without pulling the entire application bootstrap.
//
// Security layers (outermost-first):
//  1. Config.Enabled flag. When false, handler returns 404 (not present in
//     mux tree, effectively hidden).
//  2. If Config.AuthToken is set: token must match via query ?token=xxx or
//     X-Metrics-Token header. Comparison is constant-time.
//     PASS → serve. FAIL → HTTP 401 "unauthorized".
//  3. If Config.AuthToken is empty (no shared secret configured): only
//     loopback/unspecified remote addresses are allowed.
//     PASS → serve. FAIL → HTTP 403 "forbidden".
package pprofsec

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
)

// Config controls pprofsec middleware behavior. Config is a value object
// (no pointers) so it is copy-safe and does not depend on any project-wide
// Config type — avoiding circular imports with the top-level package.
type Config struct {
	Enabled   bool   // false = endpoint hidden (404)
	AuthToken string // "" = loopback-only mode
}

// Wrap returns a secured http.Handler that wraps `next` with the 3-layer
// pprof security policy. The returned handler is safe for concurrent use.
func Wrap(next http.Handler, cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Layer 1: enable gate — if disabled the handler simply doesn't
		// exist in the mux tree from the caller's perspective.
		if !cfg.Enabled {
			http.NotFound(w, r)
			return
		}

		// Layer 2: token mode (reuses same token contract as /metrics)
		if cfg.AuthToken != "" {
			provided := r.URL.Query().Get("token")
			if provided == "" {
				provided = r.Header.Get("X-Metrics-Token")
			}
			if subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.AuthToken)) != 1 {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte("unauthorized"))
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// Layer 3: no-token mode → allow only loopback or unspecified
		// (bind-any) remote addresses. Any other remote IP → 403.
		if !IsLoopbackRemoteAddr(r.RemoteAddr) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("forbidden: loopback or valid token required"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// IsLoopbackRemoteAddr reports whether a Go HTTP RemoteAddr ("host:port" or
// "[host]:port") represents a loopback or unspecified bind address. Handles
// IPv4, IPv6 (::1, [::1]), unspecified (0.0.0.0, ::), and DNS FQDN forms
// with trailing dot (e.g. "127.0.0.1.").
//
// This helper is exported for tests and for reuse by the metrics middleware
// (in main.go) when pprof and metrics security logic need to agree about
// what counts as a "local" caller.
func IsLoopbackRemoteAddr(remoteAddr string) bool {
	host := hostOnly(remoteAddr)
	// Strip trailing dot (DNS FQDN form like "127.0.0.1.")
	host = strings.TrimRight(host, ".")
	// Strip IPv6 brackets (hostOnly may leave them if RemoteAddr was
	// malformed; defensive).
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")

	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	// Hostname aliases for localhost (defensive belt-and-suspenders; most
	// deployments see pure IP RemoteAddr but some proxies set hostnames).
	switch strings.ToLower(host) {
	case "localhost", "localhost.localdomain":
		return true
	}
	return false
}

// hostOnly extracts the host part of an HTTP RemoteAddr ("host:port" form).
// Falls back to returning the input as-is if the form is unparseable, so
// downstream IP parsing still has a chance.
func hostOnly(remoteAddr string) string {
	// [::1]:54321 case
	if strings.HasPrefix(remoteAddr, "[") {
		if end := strings.Index(remoteAddr, "]:"); end != -1 {
			return remoteAddr[1:end]
		}
		// Unusual: [::1] without port, return inside
		if end := strings.LastIndex(remoteAddr, "]"); end > 0 {
			return remoteAddr[1:end]
		}
	}
	if idx := strings.LastIndex(remoteAddr, ":"); idx != -1 {
		// remoteAddr like 127.0.0.1:1234 — split on last colon
		return remoteAddr[:idx]
	}
	// No port at all, treat whole string as host
	return remoteAddr
}
