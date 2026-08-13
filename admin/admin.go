// Package admin implements the POST /admin/shutdown endpoint used by the
// "blind-llm-eyes stop" subcommand to trigger a graceful server shutdown.
package admin

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
)

// ShutdownHandler validates the X-Admin-Token header and, on match, signals
// graceful shutdown exactly once. It is safe for concurrent use.
type ShutdownHandler struct {
	token   string
	trigger chan struct{}
	once    sync.Once
}

// NewShutdownHandler creates a handler bound to the given token. If token is
// empty, a random one is generated (convenient for tests that don't care).
func NewShutdownHandler(token string) *ShutdownHandler {
	if token == "" {
		token = MustGenerateToken(32)
	}
	return &ShutdownHandler{
		token:   token,
		trigger: make(chan struct{}),
	}
}

// Done returns a channel that is closed when a valid shutdown request arrives.
func (h *ShutdownHandler) Done() <-chan struct{} { return h.trigger }

// Token returns the admin token (used when writing the pidfile).
func (h *ShutdownHandler) Token() string { return h.token }

// ServeHTTP implements http.Handler. POST with a matching X-Admin-Token
// triggers shutdown (202); missing/wrong token yields 403; other methods 405.
func (h *ShutdownHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("X-Admin-Token") != h.token {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	h.once.Do(func() { close(h.trigger) })
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintln(w, "shutting down")
}

// MustGenerateToken returns n random bytes as a hex string. It panics if the
// system CSPRNG fails (treated as an unrecoverable startup error).
func MustGenerateToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("admin: generate token: %v", err))
	}
	return hex.EncodeToString(b)
}
