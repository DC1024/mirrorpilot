package web

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRecoveryAnswersWith500 pins the half of withRecovery that a user can see.
//
// Without it net/http closes the connection after a panic with no response at
// all, which reads as "the panel hung" rather than "the panel broke".
//
// Every request passes through this middleware, so the branch that does nothing
// is covered by the whole suite; the branch that catches was covered by nothing.
// That is the usual shape of a recovery handler — it only ever runs on the day
// it matters, which is the worst possible day to find out it is wrong.
func TestRecoveryAnswersWith500(t *testing.T) {
	var logs bytes.Buffer

	h := newHarness(t, func(o *Options) {
		// Replaces the harness logger, which forwards to t.Log and so cannot be
		// inspected. Requests are served synchronously here, so the buffer is
		// not shared with another goroutine.
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/boom", nil)

	h.server.withRecovery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	// The body deliberately says nothing about the cause; the log is the only
	// record, which is why it is asserted on below.
	if body := rec.Body.String(); !strings.Contains(body, "internal server error") {
		t.Errorf("body = %q, want it to say internal server error", body)
	}

	logged := logs.String()
	if !strings.Contains(logged, "web: panic serving request") {
		t.Errorf("log = %q, want the panic to be reported", logged)
	}
	if !strings.Contains(logged, "boom") {
		t.Errorf("log = %q, want the recovered value", logged)
	}
	// The stack is the part that makes a 500 diagnosable a day later.
	if !strings.Contains(logged, "goroutine") {
		t.Errorf("log = %q, want a stack trace", logged)
	}
}

// TestRecoveryRethrowsErrAbortHandler pins the half a user must not see.
//
// A handler panics with http.ErrAbortHandler specifically to have the connection
// dropped without a response. Answering 500 instead would turn every deliberate
// abort into a logged server error, and the only symptom would be noise nobody
// could explain.
func TestRecoveryRethrowsErrAbortHandler(t *testing.T) {
	h := newHarness(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/abort", nil)

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		h.server.withRecovery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler)
		})).ServeHTTP(rec, req)
	}()

	if recovered != http.ErrAbortHandler {
		t.Fatalf("recovered %v, want the original http.ErrAbortHandler so the server closes the connection", recovered)
	}
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("wrote %d bytes with status %d, want no response at all", rec.Body.Len(), rec.Code)
	}
}
