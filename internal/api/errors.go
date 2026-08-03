package api

import "fmt"

type AppError struct {
	Message   string `json:"message"`
	Code      string `json:"code"`
	StatusCode int   `json:"-"`
	RequestID string `json:"request_id,omitempty"`
}

func (e *AppError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("[%s] %s: %s", e.RequestID, e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func newAppError(code, message string, status int) *AppError {
	return &AppError{Code: code, Message: message, StatusCode: status}
}

func ErrNotFound(resource, id string) *AppError {
	return newAppError("NOT_FOUND", fmt.Sprintf("%s not found: %s", resource, id), 404)
}

func ErrValidation(message string) *AppError {
	return newAppError("VALIDATION_ERROR", message, 400)
}

func ErrMethodNotAllowed() *AppError {
	return newAppError("METHOD_NOT_ALLOWED", "Method not allowed", 405)
}

func ErrInternal(message string) *AppError {
	return newAppError("INTERNAL_ERROR", message, 500)
}

func ErrUnavailable(message string) *AppError {
	return newAppError("SERVICE_UNAVAILABLE", message, 503)
}

func ErrBadGateway(message string) *AppError {
	return newAppError("BAD_GATEWAY", message, 502)
}

func ErrRateLimited() *AppError {
	return newAppError("RATE_LIMITED", "Too many requests, please try again later", 429)
}

func ErrUnauthorized() *AppError {
	return newAppError("UNAUTHORIZED", "Missing or invalid API token", 401)
}

func ErrConflict(message string) *AppError {
	return newAppError("CONFLICT", message, 409)
}

func WithRequestID(err error, requestID string) error {
	if appErr, ok := err.(*AppError); ok {
		cp := *appErr
		cp.RequestID = requestID
		return &cp
	}
	return err
}
