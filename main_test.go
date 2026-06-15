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
