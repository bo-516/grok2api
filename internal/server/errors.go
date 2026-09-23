// Package server is the OpenAI-compatible HTTP front of agent-mock.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/shaoboli/agent-mock/internal/grok"
	"github.com/shaoboli/agent-mock/internal/openai"
	"github.com/shaoboli/agent-mock/internal/prompt"
)

// apiError is the OpenAI error object. A nil Param encodes as null.
type apiError struct {
	// Message tells the caller what to do next.
	Message string `json:"message"`
	// Type is error.type.
	Type string `json:"type"`
	// Param is the offending field, or null.
	Param *string `json:"param"`
	// Code is error.code.
	Code string `json:"code"`
}

// errorBody wraps apiError the way the OpenAI API does.
type errorBody struct {
	Error apiError `json:"error"`
}

// writeJSON writes a JSON body and sets Content-Type. status is the HTTP status.
// A marshal failure still sends status with a plain fallback so the client is not left hanging.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return
	}
}

// writeAPI writes an OpenAI error body. param is null.
func writeAPI(w http.ResponseWriter, status int, typ, code, msg string) {
	writeJSON(w, status, errorBody{Error: apiError{Message: msg, Type: typ, Code: code}})
}

// writeRequestError writes a Parse/Render *openai.RequestError.
func writeRequestError(w http.ResponseWriter, err *openai.RequestError) {
	writeAPI(w, err.Status, err.Type, err.Code, err.Message)
}

// writeRunError maps a grok or envelope failure onto the design-doc status table.
// A cancelled client context writes nothing: the socket is already gone.
// It returns the status it wrote, or 0 when it wrote nothing.
func writeRunError(w http.ResponseWriter, err error) int {
	if err == nil {
		return 0
	}
	var env *prompt.EnvelopeError
	if errors.As(err, &env) {
		writeAPI(w, http.StatusBadGateway, "api_error", env.APICode, env.Msg)
		return http.StatusBadGateway
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		writeRequestError(w, reqErr)
		return reqErr.Status
	}
	if errors.Is(err, context.Canceled) {
		return 0
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeAPI(w, http.StatusGatewayTimeout, "api_error", grok.CodeTimeout, "grok run exceeded the request timeout. Retry or raise -request-timeout.")
		return http.StatusGatewayTimeout
	}
	ge, ok := grok.AsError(err)
	if !ok {
		writeAPI(w, http.StatusBadGateway, "api_error", grok.CodeFailed, "grok failed. "+err.Error())
		return http.StatusBadGateway
	}
	status, typ := grokStatus(ge.Code)
	writeAPI(w, status, typ, ge.Code, ge.Message)
	return status
}

// grokStatus maps a grok error code to an HTTP status and OpenAI error.type.
func grokStatus(code string) (int, string) {
	switch code {
	case grok.CodeNotLoggedIn:
		return http.StatusUnauthorized, "authentication_error"
	case grok.CodeRateLimited, grok.CodeUsageLimit:
		return http.StatusTooManyRequests, "rate_limit_error"
	case grok.CodeNotFound, grok.CodeUnsafe:
		return http.StatusInternalServerError, "server_error"
	case grok.CodeModelNotFound:
		return http.StatusBadRequest, "invalid_request_error"
	case grok.CodeTimeout:
		return http.StatusGatewayTimeout, "api_error"
	default:
		return http.StatusBadGateway, "api_error"
	}
}
