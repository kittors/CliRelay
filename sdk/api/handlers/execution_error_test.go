package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

type statusHeaderError struct {
	status  int
	headers http.Header
}

func (e *statusHeaderError) Error() string        { return "upstream said no" }
func (e *statusHeaderError) StatusCode() int      { return e.status }
func (e *statusHeaderError) Headers() http.Header { return e.headers }

func TestExecutionError(t *testing.T) {
	headers := http.Header{"Retry-After": {"7"}}
	for name, tc := range map[string]struct {
		err    error
		status int
		addon  http.Header
	}{
		"plain error":      {errors.New("boom"), http.StatusInternalServerError, nil},
		"canceled":         {context.Canceled, 499, nil},
		"deadline":         {fmt.Errorf("wait: %w", context.DeadlineExceeded), 499, nil},
		"status carried":   {&statusHeaderError{status: http.StatusTooManyRequests, headers: headers}, http.StatusTooManyRequests, headers},
		"zero status":      {&statusHeaderError{}, http.StatusInternalServerError, nil},
		"canceled wins":    {fmt.Errorf("%w: %w", context.Canceled, &statusHeaderError{status: http.StatusBadGateway}), 499, nil},
		"headers only":     {&statusHeaderError{status: http.StatusBadGateway, headers: headers}, http.StatusBadGateway, headers},
		"nil headers kept": {&statusHeaderError{status: http.StatusBadGateway}, http.StatusBadGateway, nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := executionError(tc.err)
			if got.StatusCode != tc.status || got.Error != tc.err {
				t.Fatalf("got status %d error %v, want %d %v", got.StatusCode, got.Error, tc.status, tc.err)
			}
			if tc.addon == nil {
				if got.Addon != nil {
					t.Fatalf("addon = %v, want none", got.Addon)
				}
				return
			}
			if got.Addon.Get("Retry-After") != "7" {
				t.Fatalf("addon = %v", got.Addon)
			}
			// a copy: the error's own headers are not shared with the response
			got.Addon.Set("Retry-After", "9")
			if headers.Get("Retry-After") != "7" {
				t.Fatalf("the error's headers were modified through the message")
			}
		})
	}
}
