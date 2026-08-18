// Package admin implements the POST /admin/shutdown endpoint used by the
// "blind-llm-eyes stop" subcommand to trigger a graceful server shutdown,
// and POST /admin/reload used by "blind-llm-eyes reload" and SIGHUP to
// hot-reload the configuration without restarting the process.
package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"

	"github.com/ROM4n2/blind-llm-eyes/config"
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
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin-Token")), []byte(h.token)) != 1 {
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

// ReloadHandler validates the X-Admin-Token header and, on match, calls
// ReloadableConfig.Reload() to atomically swap to a new config snapshot.
// It is safe for concurrent use (ReloadableConfig has its own internal mutex).
// Unlike ShutdownHandler, reload can be triggered multiple times — each call
// re-reads the yaml file and attempts a swap.
type ReloadHandler struct {
	token string
	cfg   *config.ReloadableConfig
}

// NewReloadHandler creates a handler bound to the given token and config.
// token is the SAME admin token used by ShutdownHandler (shared via pidfile).
// cfg must be non-nil and should have been initialized with the yaml path
// so Reload() can re-read the file.
func NewReloadHandler(token string, cfg *config.ReloadableConfig) *ReloadHandler {
	return &ReloadHandler{token: token, cfg: cfg}
}

// Token returns the admin token (for symmetry with ShutdownHandler; not
// strictly needed since the token is shared, but useful for tests).
func (h *ReloadHandler) Token() string { return h.token }

// ServeHTTP implements http.Handler.
//   - POST with matching X-Admin-Token → 200 + JSON body with old/new fingerprint
//   - POST with wrong/missing token → 403
//   - POST when Reload() fails (yaml error, non-reloadable field change) → 500
//   - Other methods → 405
func (h *ReloadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin-Token")), []byte(h.token)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	prev, next, err := h.cfg.Reload()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "reload failed: %v\n", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	prevFp := "unknown"
	nextFp := "unknown"
	if prev != nil {
		prevFp = prev.VersionFingerprint()
	}
	if next != nil {
		nextFp = next.VersionFingerprint()
	}
	fmt.Fprintf(w, `{"status":"ok","prev_fingerprint":%q,"next_fingerprint":%q}`+"\n", prevFp, nextFp)
}
