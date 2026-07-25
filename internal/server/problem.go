// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v5"
)

// Problem is an RFC 9457 Problem Details document.
//
// One representation is used for every failure so the Ember client has exactly
// one error shape to parse. Field-level validation failures ride along in the
// Errors extension member, which RFC 9457 explicitly permits.
type Problem struct {
	// Type is a URI reference identifying the problem kind. "about:blank" means
	// the HTTP status is the whole story.
	Type string `json:"type"`
	// Title is a short, stable, human-readable summary. It does not change from
	// occurrence to occurrence.
	Title string `json:"title"`
	// Status repeats the HTTP status code so a stored document stays meaningful.
	Status int `json:"status"`
	// Detail explains this specific occurrence. It never contains a secret, a
	// SQL statement, or an internal path.
	Detail string `json:"detail,omitempty"`
	// Instance is the request path that produced the problem.
	Instance string `json:"instance,omitempty"`
	// Errors is the extension member carrying per-field validation failures.
	Errors []FieldError `json:"errors,omitempty"`
}

// FieldError names one invalid input field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Error implements error so a handler can simply return a Problem.
func (p *Problem) Error() string {
	if p.Detail != "" {
		return fmt.Sprintf("%d %s: %s", p.Status, p.Title, p.Detail)
	}
	return fmt.Sprintf("%d %s", p.Status, p.Title)
}

// StatusCode lets Echo v5 resolve the response status from the error itself.
func (p *Problem) StatusCode() int { return p.Status }

// newProblem builds a Problem whose title is the canonical status text.
func newProblem(status int, detail string) *Problem {
	return &Problem{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
	}
}

// The helpers below cover every failure the API reports. Detail strings are
// written for an operator or an end user, never for a debugger.

func badRequest(detail string) *Problem   { return newProblem(http.StatusBadRequest, detail) }
func unauthorized(detail string) *Problem { return newProblem(http.StatusUnauthorized, detail) }
func forbidden(detail string) *Problem    { return newProblem(http.StatusForbidden, detail) }
func notFound(detail string) *Problem     { return newProblem(http.StatusNotFound, detail) }
func conflict(detail string) *Problem     { return newProblem(http.StatusConflict, detail) }

// unprocessable reports input that parsed but failed validation.
func unprocessable(detail string, fields ...FieldError) *Problem {
	p := newProblem(http.StatusUnprocessableEntity, detail)
	p.Errors = fields
	return p
}

// internalError reports a server fault. The underlying error is logged by the
// error handler and deliberately kept out of the response.
func internalError() *Problem {
	return newProblem(http.StatusInternalServerError, "The server could not complete the request.")
}

// errorHandler renders every error as application/problem+json.
//
// It is the single place where an internal error becomes a client response, so
// it is also the single place that decides what a client is allowed to learn.
func (s *Server) errorHandler(c *echo.Context, err error) {
	if err == nil {
		return
	}
	// This handler runs twice for a single failure: the request logger calls it
	// so it can log the resolved status, and Echo calls it again with the error
	// the logger still returns. Without this guard the body is written twice and
	// Content-Length is doubled, which no client can parse.
	if response, unwrapErr := echo.UnwrapResponse(c.Response()); unwrapErr == nil && response.Committed {
		return
	}

	problem := toProblem(err)
	problem.Instance = c.Request().URL.Path

	log := s.log
	if requestID := c.Response().Header().Get(echo.HeaderXRequestID); requestID != "" {
		log = log.With("request_id", requestID)
	}
	if problem.Status >= http.StatusInternalServerError {
		// Full detail goes to the log, never to the client.
		log.Error("request failed", "status", problem.Status, "path", problem.Instance, "error", err)
	} else {
		log.Debug("request rejected", "status", problem.Status, "path", problem.Instance, "detail", problem.Detail)
	}

	if c.Request().Method == http.MethodHead {
		if writeErr := c.NoContent(problem.Status); writeErr != nil {
			log.Error("write error response", "error", writeErr)
		}
		return
	}

	c.Response().Header().Set(echo.HeaderContentType, "application/problem+json")
	if writeErr := c.JSON(problem.Status, problem); writeErr != nil {
		log.Error("write error response", "error", writeErr)
	}
}

// toProblem maps any error onto a Problem, defaulting to a detail-free 500 so
// an unanticipated failure cannot leak internals.
func toProblem(err error) *Problem {
	var problem *Problem
	if errors.As(err, &problem) {
		// Copy so a shared sentinel is never mutated with a per-request path.
		clone := *problem
		if clone.Type == "" {
			clone.Type = "about:blank"
		}
		if clone.Title == "" {
			clone.Title = http.StatusText(clone.Status)
		}
		return &clone
	}

	var httpErr *echo.HTTPError
	if errors.As(err, &httpErr) {
		status := httpErr.StatusCode()
		detail := ""
		// Echo's own messages are safe status-level text; anything at 500 or
		// above is suppressed anyway.
		if status < http.StatusInternalServerError {
			detail = httpErr.Message
		}
		return newProblem(status, detail)
	}

	if status := echo.StatusCode(err); status != 0 && status != http.StatusInternalServerError {
		return newProblem(status, "")
	}
	return internalError()
}
