package arr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
)

// Outcome classifies one call to a media manager. The values are stable
// strings because the caller persists them as durable stage markers: after a
// crash the marker is the only evidence of what was already done, so it must
// keep meaning the same thing across upgrades.
//
// The five outcomes answer exactly one question — did the far side apply the
// request? — and nothing else:
//
//	Accepted    it did, and said so
//	NotFound    there was nothing to apply it to; already gone
//	Rejected    it refused, and repeating the call cannot change that
//	Unreachable a read failed; no mutation was requested
//	Ambiguous   it may or may not have applied the request
//
// Ambiguous exists so that a blind retry is never the default. A DELETE that
// times out may already have blocklisted a release, and re-issuing it could
// blocklist the replacement instead.
type Outcome string

const (
	Accepted    Outcome = "accepted"
	NotFound    Outcome = "not_found"
	Rejected    Outcome = "rejected"
	Unreachable Outcome = "transport_failure"
	Ambiguous   Outcome = "ambiguous_timeout"
)

// Settled reports whether the far side reached a definite conclusion. An
// unsettled outcome is not a licence to retry — that policy belongs to the
// caller — but a settled one is a firm instruction not to.
func (o Outcome) Settled() bool { return o == Accepted || o == NotFound || o == Rejected }

// Error is the only error type this package returns. It carries the outcome,
// the operation and the HTTP status, and nothing else: the reason text is a
// fixed phrase chosen from this file, never a server body, never a URL and
// never anything derived from the API key.
type Error struct {
	Service string
	Op      string
	Outcome Outcome
	// Status is the HTTP status the server answered with, or 0 when no
	// answer arrived.
	Status int
	reason string
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s %s: %s (HTTP %d)", e.Service, e.Op, e.reason, e.Status)
	}
	return fmt.Sprintf("%s %s: %s", e.Service, e.Op, e.reason)
}

// OutcomeOf is the caller's entry point for turning any error into a stage
// marker. A nil error is Accepted; an error from elsewhere is Ambiguous,
// because an unrecognised failure cannot be proven not to have applied.
func OutcomeOf(err error) Outcome {
	if err == nil {
		return Accepted
	}
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Outcome
	}
	return Ambiguous
}

// classifyTransport turns a failure that produced no HTTP status into an
// outcome. A timeout or a cancellation is ambiguous for a mutation, because
// the request may already have been applied by the time the answer was given
// up on; for a read there is nothing to be ambiguous about.
func (c *call) classifyTransport(service string, err error) *Error {
	failure := &Error{Service: service, Op: c.op, Outcome: Unreachable, reason: "request failed (network or TLS)"}
	// EOF, connection reset and TLS/network errors do not prove that a
	// mutation was never sent. Conservatively classify all of them alike.
	if c.mutating {
		failure.Outcome = Ambiguous
	}
	var timeout net.Error
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) && !(errors.As(err, &timeout) && timeout.Timeout()) {
		return failure
	}
	failure.reason = "request timed out"
	return failure
}

// classifyStatus turns a non-2xx status into an outcome.
//
// A refused redirect and any other 4xx are decisions the server made and will
// make again, so they are Rejected. A 404 is called out separately because a
// queue item that has already vanished is a normal race with the media
// manager's own import loop, not a failure. Overload and server-side errors
// are unsettled: a 502 or 504 from a reverse proxy frequently means the
// request did reach the application, so for a mutation it is ambiguous.
func (c *call) classifyStatus(service string, status int) *Error {
	failure := &Error{Service: service, Op: c.op, Status: status, Outcome: Rejected}
	switch {
	case status == http.StatusNotFound:
		failure.Outcome, failure.reason = NotFound, "the resource no longer exists"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		failure.reason = "API key rejected; check the key and its instance"
	case status >= 300 && status < 400:
		failure.reason = "redirect refused; point the URL at the instance itself"
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500:
		failure.Outcome, failure.reason = Unreachable, "instance unavailable"
		if c.mutating {
			failure.Outcome = Ambiguous
		}
	default:
		failure.reason = "request refused"
	}
	return failure
}

// reject reports a fault this package can see without asking the server, so
// nothing was applied and repeating the call cannot help.
func (c *call) reject(service, reason string) *Error {
	return &Error{Service: service, Op: c.op, Outcome: Rejected, reason: reason}
}

// ambiguous reports a mutation that the server accepted but whose answer could
// not be understood: the work is done, its identity is not known.
func (c *call) ambiguous(service, reason string) *Error {
	return &Error{Service: service, Op: c.op, Outcome: Ambiguous, reason: reason}
}
