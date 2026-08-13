package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestShutdownHandler_NoToken_Forbidden(t *testing.T) {
	h := NewShutdownHandler("secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/shutdown", nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
	select {
	case <-h.Done():
		t.Fatal("shutdown should not trigger without token")
	default:
	}
}

func TestShutdownHandler_WrongToken_Forbidden(t *testing.T) {
	h := NewShutdownHandler("secret")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/shutdown", nil)
	req.Header.Set("X-Admin-Token", "wrong")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
	select {
	case <-h.Done():
		t.Fatal("shutdown should not trigger with wrong token")
	default:
	}
}

func TestShutdownHandler_CorrectToken_TriggersShutdown(t *testing.T) {
	h := NewShutdownHandler("secret")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/shutdown", nil)
	req.Header.Set("X-Admin-Token", "secret")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rr.Code)
	}
	select {
	case <-h.Done():
		// good
	case <-time.After(time.Second):
		t.Fatal("Done() channel should be closed after valid request")
	}
}

func TestShutdownHandler_Idempotent_NoPanicOnRepeat(t *testing.T) {
	h := NewShutdownHandler("secret")
	fire := func() {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/shutdown", nil)
		req.Header.Set("X-Admin-Token", "secret")
		h.ServeHTTP(rr, req)
	}
	fire()
	fire() // must not panic (close on closed channel)
	fire()
	select {
	case <-h.Done():
	default:
		t.Fatal("Done() should remain closed")
	}
}

func TestShutdownHandler_GetMethod_NotAllowed(t *testing.T) {
	h := NewShutdownHandler("secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/shutdown", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

func TestShutdownHandler_TokenExposed(t *testing.T) {
	h := NewShutdownHandler("abc123")
	if h.Token() != "abc123" {
		t.Fatalf("expected token abc123, got %q", h.Token())
	}
}

func TestMustGenerateToken_NonEmptyAndVaries(t *testing.T) {
	a := MustGenerateToken(32)
	b := MustGenerateToken(32)
	if a == "" || len(a) < 32 {
		t.Fatalf("token too short: %q", a)
	}
	if a == b {
		t.Fatal("two tokens should differ")
	}
	if strings.ContainsAny(a, " \t\n") {
		t.Fatalf("token should be hex, got %q", a)
	}
}
