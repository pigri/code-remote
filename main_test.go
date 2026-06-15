package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const testToken = "secret-test-token"

func testHandler() http.Handler {
	// ScreenBin/ClaudeHome empty: List shells out to a missing binary, the
	// error is ignored, and it returns an empty list — enough to exercise
	// routing/auth/validation without needing screen installed.
	return newHandler(testToken, &Manager{Prefix: "test-rc"})
}

func do(t *testing.T, h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestHealthzNoAuth(t *testing.T) {
	if rr := do(t, testHandler(), http.MethodGet, "/healthz", ""); rr.Code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", rr.Code)
	}
}

func TestAuth(t *testing.T) {
	h := testHandler()
	cases := []struct {
		name, token string
		want        int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"wrong token", "nope", http.StatusUnauthorized},
		{"correct token", testToken, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rr := do(t, h, http.MethodGet, "/sessions", c.token); rr.Code != c.want {
				t.Errorf("GET /sessions [%s] = %d, want %d (body: %s)", c.name, rr.Code, c.want, rr.Body)
			}
		})
	}
}

func TestValidation(t *testing.T) {
	h := testHandler()
	cases := []struct {
		name, method, path string
		want               int
	}{
		{"get bad id", http.MethodGet, "/sessions/not-a-uuid", http.StatusBadRequest},
		{"delete bad id", http.MethodDelete, "/sessions/not-a-uuid", http.StatusBadRequest},
		{"get unknown id", http.MethodGet, "/sessions/6fd0b321-a454-4b40-9aed-131afe120d36", http.StatusNotFound},
		{"delete unknown id", http.MethodDelete, "/sessions/6fd0b321-a454-4b40-9aed-131afe120d36", http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rr := do(t, h, c.method, c.path, testToken); rr.Code != c.want {
				t.Errorf("%s %s = %d, want %d", c.method, c.path, rr.Code, c.want)
			}
		})
	}
}

func TestListEmptyOK(t *testing.T) {
	rr := do(t, testHandler(), http.MethodGet, "/sessions", testToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /sessions = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
}

// TestAuthCannotBeBypassed asserts that every protected route requires the
// exact bearer token regardless of method, auth scheme, or path tricks, and
// that the only unauthenticated surface is the non-sensitive /healthz handler.
func TestAuthCannotBeBypassed(t *testing.T) {
	h := testHandler()
	const validID = "6fd0b321-a454-4b40-9aed-131afe120d36"
	cases := []struct {
		name, method, path, auth string
		want                     int
	}{
		// Mutating routes must never be reachable without the token.
		{"create no token", http.MethodPost, "/sessions", "", http.StatusUnauthorized},
		{"delete no token", http.MethodDelete, "/sessions/" + validID, "", http.StatusUnauthorized},
		{"list no token", http.MethodGet, "/sessions", "", http.StatusUnauthorized},

		// Wrong credentials / schemes.
		{"empty bearer", http.MethodGet, "/sessions", "Bearer ", http.StatusUnauthorized},
		{"wrong token", http.MethodGet, "/sessions", "Bearer wrong", http.StatusUnauthorized},
		{"lowercase scheme", http.MethodGet, "/sessions", "bearer " + testToken, http.StatusUnauthorized},
		{"basic scheme", http.MethodGet, "/sessions", "Basic " + testToken, http.StatusUnauthorized},
		{"token without scheme", http.MethodGet, "/sessions", testToken, http.StatusUnauthorized},
		{"token with junk prefix", http.MethodGet, "/sessions", "X Bearer " + testToken, http.StatusUnauthorized},

		// /healthz exemption must not leak to protected paths via path tricks.
		{"healthz traversal", http.MethodGet, "/healthz/../sessions", "", http.StatusUnauthorized},
		{"healthz prefix", http.MethodGet, "/healthzz", "", http.StatusUnauthorized},
		{"healthz subpath", http.MethodGet, "/healthz/sessions", "", http.StatusUnauthorized},
		{"double slash healthz", http.MethodGet, "//healthz", "", http.StatusUnauthorized},
		{"uppercase healthz", http.MethodGet, "/Healthz", "", http.StatusUnauthorized},

		// Unknown routes are still gated (no existence oracle without auth).
		{"unknown route", http.MethodGet, "/admin", "", http.StatusUnauthorized},

		// Positive controls.
		{"correct token", http.MethodGet, "/sessions", "Bearer " + testToken, http.StatusOK},
		{"healthz is exempt", http.MethodGet, "/healthz", "", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, nil)
			if c.auth != "" {
				req.Header.Set("Authorization", c.auth)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != c.want {
				t.Errorf("%s %s [auth=%q] = %d, want %d", c.method, c.path, c.auth, rr.Code, c.want)
			}
		})
	}
}
