package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/watchdog"
)

// fakeActions is a ManualActions double that records what it was asked to force
// and returns canned outcomes, so the HTTP layer can be exercised in isolation.
type fakeActions struct {
	lastHash   string
	lastAction config.Action
	lastReason string
	decision   string
	err        error
}

func (f *fakeActions) Force(hash string, action config.Action, reason string) (string, error) {
	f.lastHash, f.lastAction, f.lastReason = hash, action, reason
	return f.decision, f.err
}

// actionsHandler builds a /api/v1/actions handler over a fixed snapshot and a
// fake ManualActions, returning the fake so callers can assert what Force saw.
func actionsHandler(t *testing.T, actions ManualActions) (*http.Handler, *watchdog.Snapshot) {
	t.Helper()
	c, s, m := fixture(t)
	s.Torrents = []watchdog.Row{{ShortHash: "A1B2C3D4E5F6", Hash: "a1b2c3d4e5f67890123456789012345678901234", Name: "target"}}
	h := DynamicHandlerWithActions(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, ConfigSaverFunc{
		ReadFunc: func() ([]byte, string, error) { return nil, "", nil },
		SaveFunc: func([]byte, string) (config.SaveResult, error) { return config.SaveResult{}, nil },
	}, actions)
	return &h, s
}

// postActions builds a mutation request. sameOrigin controls whether the
// browser headers a real same-origin HTMX request would carry are set; there is
// no built-in authentication to provide.
func postActions(t *testing.T, h *http.Handler, body string, sameOrigin bool) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/actions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sameOrigin {
		req.Header.Set("HX-Request", "true")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	(*h).ServeHTTP(rr, req)
	return rr
}

func TestActionsEndpointNotFoundWithoutActions(t *testing.T) {
	c, s, m := fixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, ConfigSaverFunc{
		ReadFunc: func() ([]byte, string, error) { return nil, "", nil },
		SaveFunc: func([]byte, string) (config.SaveResult, error) { return config.SaveResult{}, nil },
	})
	rr := postActions(t, &h, `{"short_hash":"a1b2c3d4e5f6","action":"delete","reason":"explicit"}`, true)
	if rr.Code != 404 {
		t.Fatalf("expected 404 when actions nil, got %d", rr.Code)
	}
}

func TestActionsEndpointSameOriginSucceedsWithoutBuiltInAuth(t *testing.T) {
	fake := &fakeActions{decision: "eligible"}
	h, _ := actionsHandler(t, fake)
	rr := postActions(t, h, `{"short_hash":"a1b2c3d4e5f6","action":"delete","reason":"explicit"}`, true)
	if rr.Code != 202 {
		t.Fatalf("expected 202 for same-origin action without built-in auth, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestActionsEndpointRejectsCrossOrigin(t *testing.T) {
	h, _ := actionsHandler(t, &fakeActions{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/actions", strings.NewReader(`{"short_hash":"a1b2c3d4e5f6","action":"delete","reason":"explicit"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://evil.example")
	(*h).ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("expected 403 for cross-origin action, got %d", rr.Code)
	}
}

func TestActionsEndpointRefusesWithoutCSRF(t *testing.T) {
	h, _ := actionsHandler(t, &fakeActions{})
	rr := postActions(t, h, `{"short_hash":"a1b2c3d4e5f6","action":"delete","reason":"explicit"}`, false)
	if rr.Code != 403 {
		t.Fatalf("expected 403 without CSRF, got %d", rr.Code)
	}
}

func TestActionsEndpointUnknownShortHash(t *testing.T) {
	h, _ := actionsHandler(t, &fakeActions{})
	rr := postActions(t, h, `{"short_hash":"zzzzzzzzzzzz","action":"delete","reason":"explicit"}`, true)
	if rr.Code != 404 {
		t.Fatalf("expected 404 for unknown short hash, got %d", rr.Code)
	}
}

func TestActionsEndpointBadAction(t *testing.T) {
	fake := &fakeActions{err: errInvalidForTest}
	h, _ := actionsHandler(t, fake)
	// An invalid action reaches Force, which rejects it; assert the 422 envelope.
	fake.err = errInvalidForTest
	rr := postActions(t, h, `{"short_hash":"A1B2C3D4E5F6","action":"bogus","reason":"explicit"}`, true)
	if rr.Code != 422 {
		t.Fatalf("expected 422 for rejected action, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"status":"rejected"`) {
		t.Fatal("expected rejected envelope")
	}
}

func TestActionsEndpointSuccessCaseInsensitive(t *testing.T) {
	fake := &fakeActions{decision: "eligible"}
	h, _ := actionsHandler(t, fake)
	// Upper-case short hash must resolve case-insensitively to the full hash.
	rr := postActions(t, h, `{"short_hash":"a1b2c3d4e5f6","action":"delete_file","reason":"explicit"}`, true)
	if rr.Code != 202 {
		t.Fatalf("expected 202, got %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["accepted"] != true {
		t.Fatal("expected accepted:true")
	}
	// The full hash (not the short hash) must flow to Force.
	if fake.lastHash != "a1b2c3d4e5f67890123456789012345678901234" {
		t.Fatalf("Force received %q, want full hash", fake.lastHash)
	}
	if fake.lastAction != config.DeleteFile || fake.lastReason != "explicit" {
		t.Fatalf("Force received action=%q reason=%q", fake.lastAction, fake.lastReason)
	}
}

var errInvalidForTest = &testError{"invalid action"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
