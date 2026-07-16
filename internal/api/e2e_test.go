package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"packetlab/internal/models"
	"packetlab/internal/store"
)

type e2eClient struct {
	handler http.Handler
	t       *testing.T
	baseURL string
}

func newE2EClient(t *testing.T, handler http.Handler, baseURL string) *e2eClient {
	return &e2eClient{handler: handler, t: t, baseURL: baseURL}
}

func (c *e2eClient) do(method, path, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Origin", "http://localhost:8080")
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, req)
	return w
}

func (c *e2eClient) get(path string) *httptest.ResponseRecorder {
	return c.do("GET", path, "")
}

func (c *e2eClient) post(path, body string) *httptest.ResponseRecorder {
	return c.do("POST", path, body)
}

func (c *e2eClient) delete(path string) *httptest.ResponseRecorder {
	return c.do("DELETE", path, "")
}

func (c *e2eClient) put(path, body string) *httptest.ResponseRecorder {
	return c.do("PUT", path, body)
}

func decodeMap(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var v map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&v); err != nil {
		t.Fatalf("decode JSON: %v, body: %s", err, w.Body.String())
	}
	return v
}

func TestE2E_FullLifecycle(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir + "/e2e.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()

	srv := New(st, nil, false, nil)
	handler := srv.Handler()
	client := newE2EClient(t, handler, "http://localhost:9090")

	t.Run("1_health_check", func(t *testing.T) {
		w := client.get("/health")
		if w.Code != 200 {
			t.Fatalf("health: %d", w.Code)
		}
		resp := decodeMap(t, w)
		if resp["status"] != "ok" {
			t.Errorf("expected ok, got %v", resp["status"])
		}
	})

	t.Run("2_ready_check", func(t *testing.T) {
		w := client.get("/ready")
		if w.Code != 200 {
			t.Fatalf("ready: %d", w.Code)
		}
	})

	t.Run("3_empty_list", func(t *testing.T) {
		w := client.get("/api/requests")
		if w.Code != 200 {
			t.Fatalf("list: %d", w.Code)
		}
		resp := decodeMap(t, w)
		if total := int(resp["total"].(float64)); total != 0 {
			t.Errorf("expected 0, got %d", total)
		}
	})

	t.Run("4_save_via_store", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			_, err := st.Save(&models.CapturedRequest{
				Method:     []string{"GET", "POST", "PUT"}[i],
				URL:        fmt.Sprintf("https://api.example.com/resource/%d", i),
				Host:       "api.example.com",
				Path:       fmt.Sprintf("/resource/%d", i),
				Protocol:   "HTTP/1.1",
				IsHTTPS:    true,
				StatusCode: []int{200, 201, 404}[i],
				ReqHeaders: map[string]string{"Accept": "application/json"},
				ResHeaders: map[string]string{"Content-Type": "application/json"},
				ResBody:    fmt.Sprintf(`{"id":%d}`, i),
				DurationMs: int64((i + 1) * 50),
				SizeBytes:  int64((i + 1) * 100),
			})
			if err != nil {
				t.Fatalf("save %d: %v", i, err)
			}
		}
	})

	t.Run("5_list_with_total", func(t *testing.T) {
		w := client.get("/api/requests")
		if w.Code != 200 {
			t.Fatalf("list: %d", w.Code)
		}
		resp := decodeMap(t, w)
		if total := int(resp["total"].(float64)); total != 3 {
			t.Errorf("expected 3, got %d", total)
		}
		data := resp["data"].([]interface{})
		if len(data) != 3 {
			t.Errorf("expected 3 items, got %d", len(data))
		}
	})

	t.Run("6_list_with_method_filter", func(t *testing.T) {
		w := client.get("/api/requests?method=POST")
		resp := decodeMap(t, w)
		if total := int(resp["total"].(float64)); total != 1 {
			t.Errorf("expected 1 POST, got %d", total)
		}
	})

	t.Run("7_list_with_error_filter", func(t *testing.T) {
		w := client.get("/api/requests?error_only=true")
		resp := decodeMap(t, w)
		if total := int(resp["total"].(float64)); total != 1 {
			t.Errorf("expected 1 error, got %d", total)
		}
	})

	t.Run("8_get_by_id", func(t *testing.T) {
		w := client.get("/api/requests/1")
		if w.Code != 200 {
			t.Fatalf("get: %d", w.Code)
		}
		var req models.CapturedRequest
		json.NewDecoder(w.Body).Decode(&req)
		if req.Method != "GET" {
			t.Errorf("expected GET, got %s", req.Method)
		}
		if req.IsHTTPS != true {
			t.Error("expected IsHTTPS=true")
		}
	})

	t.Run("9_get_nonexistent_returns_404", func(t *testing.T) {
		w := client.get("/api/requests/99999")
		assertAPIError(t, w, 404, "NOT_FOUND")
	})

	t.Run("10_stats", func(t *testing.T) {
		w := client.get("/api/stats")
		resp := decodeMap(t, w)
		if total := int(resp["total"].(float64)); total != 3 {
			t.Errorf("expected total=3, got %d", total)
		}
		if errs := int(resp["errors"].(float64)); errs != 1 {
			t.Errorf("expected errors=1, got %d", errs)
		}
	})

	t.Run("11_api_map", func(t *testing.T) {
		w := client.get("/api/apimap?host=api.example.com")
		if w.Code != 200 {
			t.Fatalf("apimap: %d", w.Code)
		}
	})

	t.Run("12_api_hosts", func(t *testing.T) {
		w := client.get("/api/apimap/hosts")
		resp := decodeMap(t, w)
		data := resp["data"].([]interface{})
		if len(data) < 1 {
			t.Errorf("expected at least 1 host, got %d", len(data))
		}
	})

	t.Run("13_add_api_note", func(t *testing.T) {
		w := client.post("/api/apimap/notes", `{"host":"api.example.com","path":"/resource/0","method":"GET","note":"test endpoint"}`)
		if w.Code != 200 {
			t.Fatalf("add note: %d, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("14_intercept_rules", func(t *testing.T) {
		w := client.post("/api/intercept/rules", `{"pattern":"*.block.com","action":"block"}`)
		if w.Code != 201 {
			t.Fatalf("create rule: %d", w.Code)
		}
		var rule models.InterceptRule
		json.NewDecoder(w.Body).Decode(&rule)

		w2 := client.get("/api/intercept/rules")
		if w2.Code != 200 {
			t.Fatalf("list rules: %d", w2.Code)
		}

		w3 := client.put(fmt.Sprintf("/api/intercept/rules/%d", rule.ID), `{"enabled":false}`)
		if w3.Code != 200 {
			t.Errorf("update rule: %d", w3.Code)
		}

		w4 := client.delete(fmt.Sprintf("/api/intercept/rules/%d", rule.ID))
		if w4.Code != 200 {
			t.Errorf("delete rule: %d", w4.Code)
		}
	})

	t.Run("15_export_har", func(t *testing.T) {
		w := client.get("/api/export/har?limit=100")
		if w.Code != 200 {
			t.Fatalf("export HAR: %d", w.Code)
		}
		var har map[string]interface{}
		json.NewDecoder(w.Body).Decode(&har)
		logMap := har["log"].(map[string]interface{})
		entries := logMap["entries"].([]interface{})
		if len(entries) != 3 {
			t.Errorf("expected 3 HAR entries, got %d", len(entries))
		}
		creator := logMap["creator"].(map[string]interface{})
		if creator["name"] != "PacketLab" {
			t.Errorf("expected PacketLab, got %v", creator["name"])
		}
	})

	t.Run("16_metrics", func(t *testing.T) {
		w := client.get("/api/metrics")
		resp := decodeMap(t, w)
		if _, ok := resp["goroutines"]; !ok {
			t.Error("expected goroutines")
		}
	})

	t.Run("17_resend_via_mock", func(t *testing.T) {
		mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"resent":true}`))
		}))
		defer mock.Close()

		w := client.post("/api/resend", fmt.Sprintf(
			`{"method":"GET","url":"%s/test","headers":{}}`, mock.URL,
		))
		if w.Code != 200 {
			t.Fatalf("resend: %d, body: %s", w.Code, w.Body.String())
		}
		var result ResendResult
		json.NewDecoder(w.Body).Decode(&result)
		if result.StatusCode != 200 {
			t.Errorf("expected 200, got %d", result.StatusCode)
		}
	})

	t.Run("18_delete_single", func(t *testing.T) {
		w := client.delete("/api/requests/1")
		if w.Code != 200 {
			t.Fatalf("delete: %d", w.Code)
		}
		w2 := client.get("/api/requests/1")
		assertAPIError(t, w2, 404, "NOT_FOUND")
	})

	t.Run("19_clear_all", func(t *testing.T) {
		w := client.post("/api/clear", "")
		if w.Code != 200 {
			t.Fatalf("clear: %d", w.Code)
		}
		w2 := client.get("/api/stats")
		resp := decodeMap(t, w2)
		if total := int(resp["total"].(float64)); total != 0 {
			t.Errorf("expected 0 after clear, got %d", total)
		}
	})

	t.Run("20_cors_preflight", func(t *testing.T) {
		w := client.do("OPTIONS", "/api/requests", "")
		if w.Code != 204 {
			t.Errorf("expected 204, got %d", w.Code)
		}
		if origin := w.Header().Get("Access-Control-Allow-Origin"); origin != "http://localhost:8080" {
			t.Errorf("expected localhost origin, got %q", origin)
		}
	})

	t.Run("21_error_has_request_id", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/requests/99999", nil)
		req.Header.Set("X-Request-ID", "e2e-trace-001")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		var resp map[string]interface{}
		json.NewDecoder(w.Body).Decode(&resp)
		if resp["request_id"] != "e2e-trace-001" {
			t.Errorf("expected request_id=e2e-trace-001, got %v", resp["request_id"])
		}
	})
}

func TestE2E_MultipleHostsFiltering(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.New(dir + "/multi.db")
	defer st.Close()

	srv := New(st, nil, false, nil)
	client := newE2EClient(t, srv.Handler(), "")

	hosts := []string{"api.a.com", "api.b.com", "api.c.com"}
	for _, h := range hosts {
		st.Save(&models.CapturedRequest{
			Method: "GET", URL: "https://" + h + "/", Host: h,
			Path: "/", Protocol: "HTTP/1.1", StatusCode: 200,
		})
	}

	w := client.get("/api/requests?host=api.b.com")
	resp := decodeMap(t, w)
	if total := int(resp["total"].(float64)); total != 1 {
		t.Errorf("expected 1 for api.b.com, got %d", total)
	}

	w2 := client.get("/api/apimap/hosts")
	resp2 := decodeMap(t, w2)
	if total := int(resp2["total"].(float64)); total != 3 {
		t.Errorf("expected 3 hosts, got %d", total)
	}
}

func TestE2E_InterceptLogsLifecycle(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.New(dir + "/intercept.db")
	defer st.Close()

	srv := New(st, nil, false, nil)
	client := newE2EClient(t, srv.Handler(), "")

	st.SaveInterceptLog(&models.InterceptLog{
		Action: "allow", RequestURL: "https://ok.com/", RequestMethod: "GET",
		RequestHost: "ok.com", Mode: "auto",
	})
	st.SaveInterceptLog(&models.InterceptLog{
		Action: "drop", RequestURL: "https://bad.com/", RequestMethod: "POST",
		RequestHost: "bad.com", RulePattern: "*.bad.com", Mode: "manual",
	})

	w := client.get("/api/intercept/logs")
	resp := decodeMap(t, w)
	if total := int(resp["total"].(float64)); total != 2 {
		t.Errorf("expected 2 logs, got %d", total)
	}

	w2 := client.get("/api/intercept/logs?action=drop")
	resp2 := decodeMap(t, w2)
	if total := int(resp2["total"].(float64)); total != 1 {
		t.Errorf("expected 1 drop log, got %d", total)
	}
}
