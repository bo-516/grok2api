// Package grok runs one restricted grok CLI process and turns its stdout into a completion.
package grok

import (
	"errors"
	"fmt"
	"strings"
)

// Failure codes are the OpenAI error.code values the HTTP layer copies through.
// They are stable: callers and tests branch on the code, not on the message text.
const (
	// CodeNotLoggedIn means grok has no SuperGrok / X Premium+ session.
	CodeNotLoggedIn = "grok_not_logged_in"
	// CodeRateLimited means grok asked the caller to slow down.
	CodeRateLimited = "grok_rate_limited"
	// CodeUsageLimit means the subscription quota is exhausted.
	CodeUsageLimit = "grok_usage_limit"
	// CodeNotFound means the grok executable could not be started.
	CodeNotFound = "grok_not_found"
	// CodeModelNotFound means grok rejected the model id we passed with -m.
	CodeModelNotFound = "model_not_found"
	// CodeFailed means grok exited without a more specific class.
	CodeFailed = "grok_failed"
	// CodeBadOutput means stdout was not a usable completion or envelope.
	CodeBadOutput = "grok_bad_output"
	// CodeStructured means a tool envelope or JSON schema result was still invalid.
	CodeStructured = "grok_structured_output_failed"
	// CodeTimeout means the request deadline killed the process group.
	CodeTimeout = "grok_timeout"
	// CodeUnsafe means the init record did not advertise an empty tool list.
	CodeUnsafe = "unsafe_grok_toolset"
)

// Error is a classified grok failure. Code selects the HTTP status.
// Message is safe to show to the caller and says what to do next.
// SessionID is set when grok already opened a session, so the caller can delete it.
type Error struct {
	// Code is one of the Code* constants.
	Code string
	// Message is the OpenAI error.message text.
	Message string
	// SessionID is the grok session to delete, or empty when none was opened.
	SessionID string
	// Err is the underlying exec or decode error, if any. It is not shown to clients.
	Err error
}

// Error returns the client message so logs and errors.As see the same text.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// Unwrap exposes Err for errors.Is on context failures wrapped inside a grok error.
func (e *Error) Unwrap() error { return e.Err }

// AsError reports whether err is a *Error and returns it.
func AsError(err error) (*Error, bool) {
	var ge *Error
	if errors.As(err, &ge) {
		return ge, true
	}
	return nil, false
}

// Classify maps grok's message and stderr onto a *Error.
// Matching is case-insensitive. Model failures are tested before login because
// the model error text tells the user to run `grok models` and must not become a 401.
// detail is included so the caller sees grok's own sentence. stderrTail is the
// last 4 KiB of stderr. An empty detail still returns a specific code when stderr matches.
func Classify(detail, stderrTail string) *Error {
	blob := strings.ToLower(detail + "\n" + stderrTail)
	msg := strings.TrimSpace(detail)
	if msg == "" {
		msg = strings.TrimSpace(stderrTail)
	}
	switch {
	case containsAny(blob, "unknown model", "couldn't set model", "could not set model", "isn't available", "is not available"):
		return &Error{Code: CodeModelNotFound, Message: "grok rejected the model. Run `grok models` and retry. " + msg}
	case containsAny(blob, "rate limit", "rate_limited", "rate limited", "too many requests", "concurrency"):
		return &Error{Code: CodeRateLimited, Message: "grok rate limited this request. Wait and retry. " + msg}
	case containsAny(blob, "usage limit", "usage_pool", "weekly limit", "free usage"):
		return &Error{Code: CodeUsageLimit, Message: "grok subscription usage is exhausted. " + msg}
	case containsAny(blob, "disk quota"):
		return &Error{Code: CodeFailed, Message: "grok failed. " + msg}
	case strings.Contains(blob, "quota"):
		return &Error{Code: CodeUsageLimit, Message: "grok subscription usage is exhausted. " + msg}
	case containsAny(blob, "not signed in", "not authenticated", "not logged in", "unauthorized", "authentication failed", "authentication required", "login", "401"):
		return &Error{Code: CodeNotLoggedIn, Message: "grok CLI is not logged in. Run `grok login` with your own SuperGrok / X Premium+ account, then retry."}
	default:
		if msg == "" {
			msg = "grok exited without a result"
		}
		return &Error{Code: CodeFailed, Message: "grok failed. " + msg}
	}
}

// containsAny reports whether s contains any needle. Used by Classify only.
func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// Failed builds a CodeFailed error that includes the stderr tail.
// Callers use it when no JSON record arrived. The tail can be empty.
func Failed(stderrTail string, err error) *Error {
	msg := "grok failed before producing a result."
	if t := strings.TrimSpace(stderrTail); t != "" {
		msg = fmt.Sprintf("grok failed before producing a result. stderr: %s", t)
	}
	return &Error{Code: CodeFailed, Message: msg, Err: err}
}
