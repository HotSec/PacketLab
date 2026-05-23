package store

import (
	"path/filepath"
	"testing"

	"packetlab/internal/models"
)

func newTestStoreForListFull(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	st, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestListFullEmpty(t *testing.T) {
	st := newTestStoreForListFull(t)
	items, err := st.ListFull("", "", "", false, 50, 0)
	if err != nil {
		t.Fatalf("ListFull: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 items, got %d", len(items))
	}
}

func TestListFullWithData(t *testing.T) {
	st := newTestStoreForListFull(t)

	st.Save(&models.CapturedRequest{
		Method:     "GET",
		URL:        "https://api.example.com/users",
		Host:       "api.example.com",
		Path:       "/users",
		Protocol:   "HTTP/1.1",
		ReqHeaders: map[string]string{"Accept": "application/json"},
		ReqBody:    "",
		StatusCode: 200,
		ResHeaders: map[string]string{"Content-Type": "application/json"},
		ResBody:    `[{"id":1}]`,
		DurationMs: 100,
		SizeBytes:  256,
	})

	items, err := st.ListFull("", "", "", false, 50, 0)
	if err != nil {
		t.Fatalf("ListFull: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}

	req := items[0]
	if req.Method != "GET" {
		t.Errorf("expected GET, got %s", req.Method)
	}
	if req.URL != "https://api.example.com/users" {
		t.Errorf("unexpected URL: %s", req.URL)
	}
	if req.StatusCode != 200 {
		t.Errorf("expected 200, got %d", req.StatusCode)
	}
	if req.ReqHeaders["Accept"] != "application/json" {
		t.Errorf("expected Accept header, got %v", req.ReqHeaders)
	}
	if req.ResHeaders["Content-Type"] != "application/json" {
		t.Errorf("expected Content-Type header, got %v", req.ResHeaders)
	}
	if req.ResBody != `[{"id":1}]` {
		t.Errorf("unexpected ResBody: %s", req.ResBody)
	}
	if req.DurationMs != 100 {
		t.Errorf("expected DurationMs=100, got %d", req.DurationMs)
	}
}

func TestListFullMethodFilter(t *testing.T) {
	st := newTestStoreForListFull(t)

	st.Save(&models.CapturedRequest{Method: "GET", URL: "https://a.com/", Host: "a.com", Path: "/", Protocol: "HTTP/1.1"})
	st.Save(&models.CapturedRequest{Method: "POST", URL: "https://b.com/", Host: "b.com", Path: "/", Protocol: "HTTP/1.1"})
	st.Save(&models.CapturedRequest{Method: "GET", URL: "https://c.com/", Host: "c.com", Path: "/", Protocol: "HTTP/1.1"})

	items, err := st.ListFull("POST", "", "", false, 50, 0)
	if err != nil {
		t.Fatalf("ListFull: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 POST, got %d", len(items))
	}
	if len(items) > 0 && items[0].Method != "POST" {
		t.Errorf("expected POST, got %s", items[0].Method)
	}
}

func TestListFullErrorOnly(t *testing.T) {
	st := newTestStoreForListFull(t)

	st.Save(&models.CapturedRequest{Method: "GET", URL: "https://a.com/", Host: "a.com", Path: "/", Protocol: "HTTP/1.1", StatusCode: 200})
	st.Save(&models.CapturedRequest{Method: "GET", URL: "https://b.com/", Host: "b.com", Path: "/", Protocol: "HTTP/1.1", StatusCode: 500})

	items, err := st.ListFull("", "", "", true, 50, 0)
	if err != nil {
		t.Fatalf("ListFull: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 error, got %d", len(items))
	}
	if len(items) > 0 && items[0].StatusCode != 500 {
		t.Errorf("expected 500, got %d", items[0].StatusCode)
	}
}

func TestListFullHostFilter(t *testing.T) {
	st := newTestStoreForListFull(t)

	st.Save(&models.CapturedRequest{Method: "GET", URL: "https://api.example.com/", Host: "api.example.com", Path: "/", Protocol: "HTTP/1.1"})
	st.Save(&models.CapturedRequest{Method: "GET", URL: "https://other.com/", Host: "other.com", Path: "/", Protocol: "HTTP/1.1"})

	items, err := st.ListFull("", "", "api.example.com", false, 50, 0)
	if err != nil {
		t.Fatalf("ListFull: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 item for host filter, got %d", len(items))
	}
}

func TestListFullPagination(t *testing.T) {
	st := newTestStoreForListFull(t)

	for i := 0; i < 5; i++ {
		st.Save(&models.CapturedRequest{
			Method: "GET", URL: "https://example.com/", Host: "example.com",
			Path: "/", Protocol: "HTTP/1.1",
		})
	}

	page1, err := st.ListFull("", "", "", false, 2, 0)
	if err != nil {
		t.Fatalf("ListFull page1: %v", err)
	}
	if len(page1) != 2 {
		t.Errorf("expected 2 items on page 1, got %d", len(page1))
	}

	page2, err := st.ListFull("", "", "", false, 2, 2)
	if err != nil {
		t.Fatalf("ListFull page2: %v", err)
	}
	if len(page2) != 2 {
		t.Errorf("expected 2 items on page 2, got %d", len(page2))
	}

	if page1[0].ID == page2[0].ID {
		t.Error("pages should have different items")
	}
}

func TestListFullNilHeaders(t *testing.T) {
	st := newTestStoreForListFull(t)

	st.Save(&models.CapturedRequest{
		Method:   "GET",
		URL:      "https://minimal.com/",
		Host:     "minimal.com",
		Path:     "/",
		Protocol: "HTTP/1.1",
	})

	items, err := st.ListFull("", "", "", false, 50, 0)
	if err != nil {
		t.Fatalf("ListFull: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ReqHeaders == nil {
		t.Error("ReqHeaders should not be nil")
	}
	if items[0].ResHeaders == nil {
		t.Error("ResHeaders should not be nil")
	}
}

func TestVersionedMigration(t *testing.T) {
	st := newTestStoreForListFull(t)

	var version int
	err := st.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if version == 0 {
		t.Error("expected at least one migration to be applied")
	}
}

func TestMigrationIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "idempotent.db")

	st1, err := New(dbPath)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	st1.Close()

	st2, err := New(dbPath)
	if err != nil {
		t.Fatalf("second New (idempotent): %v", err)
	}
	st2.Close()
}

func TestListSearchFilter(t *testing.T) {
	st := newTestStoreForListFull(t)

	st.Save(&models.CapturedRequest{Method: "GET", URL: "https://api.example.com/users", Host: "api.example.com", Path: "/users", Protocol: "HTTP/1.1"})
	st.Save(&models.CapturedRequest{Method: "GET", URL: "https://other.com/products", Host: "other.com", Path: "/products", Protocol: "HTTP/1.1"})

	items, total, err := st.List("", "example", "", false, 50, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 {
		t.Errorf("expected total=1 for search 'example', got %d", total)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 item, got %d", len(items))
	}
}

func TestListHostFilterWithPort(t *testing.T) {
	st := newTestStoreForListFull(t)

	st.Save(&models.CapturedRequest{Method: "GET", URL: "http://localhost:8080/api", Host: "localhost:8080", Path: "/api", Protocol: "HTTP/1.1"})
	st.Save(&models.CapturedRequest{Method: "GET", URL: "https://other.com/", Host: "other.com", Path: "/", Protocol: "HTTP/1.1"})

	items, total, err := st.List("", "", "localhost", false, 50, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 {
		t.Errorf("expected total=1 for host filter 'localhost', got %d", total)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 item, got %d", len(items))
	}
}

func TestGetAPINotes(t *testing.T) {
	st := newTestStoreForListFull(t)

	st.SaveAPINote(&models.APINote{Host: "a.com", Path: "/api", Method: "GET", Note: "note a"})
	st.SaveAPINote(&models.APINote{Host: "b.com", Path: "/api", Method: "POST", Note: "note b"})

	notes, err := st.GetAPINotes()
	if err != nil {
		t.Fatalf("GetAPINotes: %v", err)
	}
	if len(notes) < 2 {
		t.Errorf("expected at least 2 notes, got %d", len(notes))
	}
}

func TestListHostsPagination(t *testing.T) {
	st := newTestStoreForListFull(t)

	for _, host := range []string{"a.com", "b.com", "c.com", "d.com"} {
		st.Save(&models.CapturedRequest{
			Method: "GET", URL: "https://" + host + "/", Host: host,
			Path: "/", Protocol: "HTTP/1.1",
		})
	}

	hosts, total, err := st.ListHosts("", 2, 0)
	if err != nil {
		t.Fatalf("ListHosts: %v", err)
	}
	if total != 4 {
		t.Errorf("expected total=4, got %d", total)
	}
	if len(hosts) != 2 {
		t.Errorf("expected 2 hosts on first page, got %d", len(hosts))
	}
}
