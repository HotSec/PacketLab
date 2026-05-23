package api

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestAppErrorConstruction(t *testing.T) {
	err := newAppError("TEST_CODE", "test message", 418)
	if err.Code != "TEST_CODE" {
		t.Errorf("expected Code=TEST_CODE, got %s", err.Code)
	}
	if err.Message != "test message" {
		t.Errorf("expected Message='test message', got %s", err.Message)
	}
	if err.StatusCode != 418 {
		t.Errorf("expected StatusCode=418, got %d", err.StatusCode)
	}
	if err.RequestID != "" {
		t.Errorf("expected empty RequestID, got %s", err.RequestID)
	}
}

func TestAppErrorImplementsError(t *testing.T) {
	err := newAppError("CODE", "msg", 500)
	if err.Error() != "CODE: msg" {
		t.Errorf("expected 'CODE: msg', got %s", err.Error())
	}
}

func TestAppErrorWithRequestID(t *testing.T) {
	err := newAppError("CODE", "msg", 500)
	err.RequestID = "abc123"
	if err.Error() != "[abc123] CODE: msg" {
		t.Errorf("expected '[abc123] CODE: msg', got %s", err.Error())
	}
}

func TestErrNotFound(t *testing.T) {
	err := ErrNotFound("Request", "42")
	if err.Code != "NOT_FOUND" {
		t.Errorf("expected NOT_FOUND, got %s", err.Code)
	}
	if err.StatusCode != 404 {
		t.Errorf("expected 404, got %d", err.StatusCode)
	}
	if err.Message != "Request not found: 42" {
		t.Errorf("unexpected message: %s", err.Message)
	}
}

func TestErrValidation(t *testing.T) {
	err := ErrValidation("bad input")
	if err.Code != "VALIDATION_ERROR" {
		t.Errorf("expected VALIDATION_ERROR, got %s", err.Code)
	}
	if err.StatusCode != 400 {
		t.Errorf("expected 400, got %d", err.StatusCode)
	}
}

func TestErrMethodNotAllowed(t *testing.T) {
	err := ErrMethodNotAllowed()
	if err.Code != "METHOD_NOT_ALLOWED" {
		t.Errorf("expected METHOD_NOT_ALLOWED, got %s", err.Code)
	}
	if err.StatusCode != 405 {
		t.Errorf("expected 405, got %d", err.StatusCode)
	}
}

func TestErrInternal(t *testing.T) {
	err := ErrInternal("something broke")
	if err.Code != "INTERNAL_ERROR" {
		t.Errorf("expected INTERNAL_ERROR, got %s", err.Code)
	}
	if err.StatusCode != 500 {
		t.Errorf("expected 500, got %d", err.StatusCode)
	}
}

func TestErrUnavailable(t *testing.T) {
	err := ErrUnavailable("service down")
	if err.Code != "SERVICE_UNAVAILABLE" {
		t.Errorf("expected SERVICE_UNAVAILABLE, got %s", err.Code)
	}
	if err.StatusCode != 503 {
		t.Errorf("expected 503, got %d", err.StatusCode)
	}
}

func TestErrBadGateway(t *testing.T) {
	err := ErrBadGateway("upstream failed")
	if err.Code != "BAD_GATEWAY" {
		t.Errorf("expected BAD_GATEWAY, got %s", err.Code)
	}
	if err.StatusCode != 502 {
		t.Errorf("expected 502, got %d", err.StatusCode)
	}
}

func TestErrRateLimited(t *testing.T) {
	err := ErrRateLimited()
	if err.Code != "RATE_LIMITED" {
		t.Errorf("expected RATE_LIMITED, got %s", err.Code)
	}
	if err.StatusCode != 429 {
		t.Errorf("expected 429, got %d", err.StatusCode)
	}
}

func TestErrConflict(t *testing.T) {
	err := ErrConflict("duplicate")
	if err.Code != "CONFLICT" {
		t.Errorf("expected CONFLICT, got %s", err.Code)
	}
	if err.StatusCode != 409 {
		t.Errorf("expected 409, got %d", err.StatusCode)
	}
}

func TestWithRequestIDAppError(t *testing.T) {
	original := ErrNotFound("User", "1")
	wrapped := WithRequestID(original, "req_abc")
	appErr, ok := wrapped.(*AppError)
	if !ok {
		t.Fatal("expected *AppError")
	}
	if appErr.RequestID != "req_abc" {
		t.Errorf("expected RequestID=req_abc, got %s", appErr.RequestID)
	}
	if appErr.Code != "NOT_FOUND" {
		t.Errorf("expected Code=NOT_FOUND, got %s", appErr.Code)
	}
}

func TestWithRequestIDNonAppError(t *testing.T) {
	original := errors.New("plain error")
	wrapped := WithRequestID(original, "req_abc")
	if wrapped.Error() != "plain error" {
		t.Errorf("expected unchanged error, got %s", wrapped.Error())
	}
}

func TestWithRequestIDDoesNotMutateOriginal(t *testing.T) {
	original := ErrNotFound("Item", "5")
	_ = WithRequestID(original, "req_xyz")
	if original.RequestID != "" {
		t.Error("WithRequestID should not mutate the original error")
	}
}

func TestAppErrorJSONSerialization(t *testing.T) {
	err := ErrValidation("bad input")
	err.RequestID = "req_123"
	data, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatalf("marshal: %v", marshalErr)
	}
	var parsed map[string]interface{}
	if unmarshalErr := json.Unmarshal(data, &parsed); unmarshalErr != nil {
		t.Fatalf("unmarshal: %v", unmarshalErr)
	}
	if parsed["code"] != "VALIDATION_ERROR" {
		t.Errorf("expected code=VALIDATION_ERROR, got %v", parsed["code"])
	}
	if parsed["message"] != "bad input" {
		t.Errorf("expected message='bad input', got %v", parsed["message"])
	}
	if parsed["request_id"] != "req_123" {
		t.Errorf("expected request_id=req_123, got %v", parsed["request_id"])
	}
}

func TestAppErrorJSONOmitsEmptyRequestID(t *testing.T) {
	err := ErrInternal("fail")
	data, _ := json.Marshal(err)
	var parsed map[string]interface{}
	json.Unmarshal(data, &parsed)
	if _, exists := parsed["request_id"]; exists {
		t.Error("request_id should be omitted when empty")
	}
}
