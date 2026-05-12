package auth

// Error describes an authentication related failure in a provider agnostic format.
type Error struct {
	// Code is a short machine readable identifier.
	Code string `json:"code,omitempty"`
	// Message is a human readable description of the failure.
	Message string `json:"message"`
	// Retryable indicates whether a retry might fix the issue automatically.
	Retryable bool `json:"retryable"`
	// HTTPStatus optionally records an HTTP-like status code for the error.
	HTTPStatus int `json:"http_status,omitempty"`
	// QuotaWindow identifies the provider quota window that was exhausted.
	QuotaWindow string `json:"quota_window,omitempty"`
	// QuotaWindowMinutes stores the provider quota window duration when known.
	QuotaWindowMinutes int `json:"quota_window_minutes,omitempty"`
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// StatusCode implements optional status accessor for manager decision making.
func (e *Error) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.HTTPStatus
}
