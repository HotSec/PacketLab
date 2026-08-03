package models

import (
	"encoding/json"
	"testing"
)

func TestInterceptLogJSON(t *testing.T) {
	log := InterceptLog{
		ID:            1,
		Action:        "drop",
		RequestURL:    "https://example.com/api/data",
		RequestMethod: "POST",
		RequestHost:   "example.com",
		RulePattern:   "*.example.com",
		Mode:          "auto",
		CreatedAt:     "2026-05-23T10:00:00Z",
	}

	data, err := json.Marshal(log)
	if err != nil {
		t.Fatalf("marshal InterceptLog: %v", err)
	}

	var result InterceptLog
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal InterceptLog: %v", err)
	}

	if result.ID != log.ID {
		t.Errorf("expected ID %d, got %d", log.ID, result.ID)
	}
	if result.Action != log.Action {
		t.Errorf("expected Action %s, got %s", log.Action, result.Action)
	}
	if result.RequestURL != log.RequestURL {
		t.Errorf("expected RequestURL %s, got %s", log.RequestURL, result.RequestURL)
	}
	if result.RequestMethod != log.RequestMethod {
		t.Errorf("expected RequestMethod %s, got %s", log.RequestMethod, result.RequestMethod)
	}
	if result.RequestHost != log.RequestHost {
		t.Errorf("expected RequestHost %s, got %s", log.RequestHost, result.RequestHost)
	}
	if result.RulePattern != log.RulePattern {
		t.Errorf("expected RulePattern %s, got %s", log.RulePattern, result.RulePattern)
	}
	if result.Mode != log.Mode {
		t.Errorf("expected Mode %s, got %s", log.Mode, result.Mode)
	}
	if result.CreatedAt != log.CreatedAt {
		t.Errorf("expected CreatedAt %s, got %s", log.CreatedAt, result.CreatedAt)
	}
}

func TestInterceptLogJSONEmptyRulePattern(t *testing.T) {
	// manual 模式下 rule_pattern 应为空
	log := InterceptLog{
		ID:            2,
		Action:        "allow",
		RequestURL:    "https://example.com/",
		RequestMethod: "GET",
		RequestHost:   "example.com",
		RulePattern:   "",
		Mode:          "manual",
		CreatedAt:     "2026-05-23T11:00:00Z",
	}

	data, err := json.Marshal(log)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var result InterceptLog
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if result.RulePattern != "" {
		t.Errorf("expected empty RulePattern for manual mode, got %s", result.RulePattern)
	}
	if result.Mode != "manual" {
		t.Errorf("expected mode=manual, got %s", result.Mode)
	}
}

func TestCleanupRequestJSON(t *testing.T) {
	req := CleanupRequest{RetentionDays: 7}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var result CleanupRequest
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if result.RetentionDays != 7 {
		t.Errorf("expected RetentionDays=7, got %d", result.RetentionDays)
	}
}

func TestCleanupRequestJSONDefault(t *testing.T) {
	// retention_days=0 表示使用默认值
	req := CleanupRequest{}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var result CleanupRequest
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if result.RetentionDays != 0 {
		t.Errorf("expected RetentionDays=0, got %d", result.RetentionDays)
	}
}

func TestCleanupResponseJSON(t *testing.T) {
	resp := CleanupResponse{
		DeletedRequests: 100,
		DeletedLogs:     50,
		RetentionDays:   30,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var result CleanupResponse
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if result.DeletedRequests != 100 {
		t.Errorf("expected DeletedRequests=100, got %d", result.DeletedRequests)
	}
	if result.DeletedLogs != 50 {
		t.Errorf("expected DeletedLogs=50, got %d", result.DeletedLogs)
	}
	if result.RetentionDays != 30 {
		t.Errorf("expected RetentionDays=30, got %d", result.RetentionDays)
	}
}

func TestInterceptRuleJSON(t *testing.T) {
	rule := InterceptRule{
		ID:      1,
		Pattern: "*.example.com",
		Action:  "block",
		Enabled: true,
	}

	data, err := json.Marshal(rule)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var result InterceptRule
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if result.ID != 1 || result.Pattern != "*.example.com" || result.Action != "block" || !result.Enabled {
		t.Errorf("InterceptRule roundtrip failed")
	}
}

func TestInterceptResultJSON(t *testing.T) {
	result := InterceptResult{
		RequestID:  "req_123",
		Action:     "modify",
		Method:     "POST",
		URL:        "https://example.com/api",
		NewHeaders: map[string]string{"X-Custom": "value"},
		NewBody:    `{"key":"value"}`,
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed InterceptResult
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if parsed.RequestID != "req_123" || parsed.Action != "modify" {
		t.Errorf("InterceptResult roundtrip failed")
	}
}

func TestPendingRequestJSON(t *testing.T) {
	pr := PendingRequest{
		ID:      "req_456",
		Method:  "GET",
		URL:     "https://example.com/",
		Host:    "example.com",
		Path:    "/",
		Headers: map[string]string{"Accept": "application/json"},
		Body:    "",
	}

	data, err := json.Marshal(pr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed PendingRequest
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if parsed.ID != "req_456" || parsed.Method != "GET" {
		t.Errorf("PendingRequest roundtrip failed")
	}
}
