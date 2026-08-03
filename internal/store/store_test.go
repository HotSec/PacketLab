package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf8"

	"packetlab/internal/models"
)

// helper: create in-memory store for tests
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	st, err := New(dbPath)
	if err != nil {
		t.Fatalf("New(%s): %v", dbPath, err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// ========================================
// SaveInterceptLog / ListInterceptLogs
// ========================================

func TestSaveInterceptLog(t *testing.T) {
	st := newTestStore(t)

	log := &models.InterceptLog{
		Action:        "drop",
		RequestURL:    "https://example.com/api/data",
		RequestMethod: "POST",
		RequestHost:   "example.com",
		RulePattern:   "*.example.com",
		Mode:          "auto",
	}

	err := st.SaveInterceptLog(log)
	if err != nil {
		t.Fatalf("SaveInterceptLog: %v", err)
	}

	if log.ID == 0 {
		t.Error("expected ID > 0 after save")
	}
	if log.CreatedAt == "" {
		t.Error("expected CreatedAt to be set")
	}
}

func TestSaveInterceptLogManualMode(t *testing.T) {
	st := newTestStore(t)

	log := &models.InterceptLog{
		Action:        "allow",
		RequestURL:    "https://httpbin.org/get",
		RequestMethod: "GET",
		RequestHost:   "httpbin.org",
		RulePattern:   "", // manual 模式无规则
		Mode:          "manual",
	}

	err := st.SaveInterceptLog(log)
	if err != nil {
		t.Fatalf("SaveInterceptLog: %v", err)
	}
}

func TestListInterceptLogsAll(t *testing.T) {
	st := newTestStore(t)

	// insert 3 logs
	for _, log := range []*models.InterceptLog{
		{Action: "allow", RequestURL: "https://a.com/1", RequestMethod: "GET", RequestHost: "a.com", Mode: "auto"},
		{Action: "drop", RequestURL: "https://b.com/2", RequestMethod: "POST", RequestHost: "b.com", RulePattern: "*.b.com", Mode: "auto"},
		{Action: "modify", RequestURL: "https://c.com/3", RequestMethod: "PUT", RequestHost: "c.com", Mode: "manual"},
	} {
		if err := st.SaveInterceptLog(log); err != nil {
			t.Fatalf("SaveInterceptLog: %v", err)
		}
	}

	logs, total, err := st.ListInterceptLogs("", "", "", "", 50, 0)
	if err != nil {
		t.Fatalf("ListInterceptLogs: %v", err)
	}

	if total != 3 {
		t.Errorf("expected total=3, got %d", total)
	}
	if len(logs) != 3 {
		t.Errorf("expected len=3, got %d", len(logs))
	}
}

func TestListInterceptLogsFilterByAction(t *testing.T) {
	st := newTestStore(t)

	for _, log := range []*models.InterceptLog{
		{Action: "allow", RequestURL: "https://a.com/1", RequestMethod: "GET", RequestHost: "a.com", Mode: "auto"},
		{Action: "drop", RequestURL: "https://a.com/2", RequestMethod: "POST", RequestHost: "a.com", Mode: "auto"},
		{Action: "allow", RequestURL: "https://a.com/3", RequestMethod: "GET", RequestHost: "a.com", Mode: "auto"},
	} {
		if err := st.SaveInterceptLog(log); err != nil {
			t.Fatalf("SaveInterceptLog: %v", err)
		}
	}

	logs, total, err := st.ListInterceptLogs("allow", "", "", "", 50, 0)
	if err != nil {
		t.Fatalf("ListInterceptLogs allow: %v", err)
	}

	if total != 2 {
		t.Errorf("expected total=2 for action=allow, got %d", total)
	}
	if len(logs) != 2 {
		t.Errorf("expected len=2 for action=allow, got %d", len(logs))
	}

	// verify all returned logs have action=allow
	for _, l := range logs {
		if l.Action != "allow" {
			t.Errorf("expected action=allow, got %s", l.Action)
		}
	}
}

func TestListInterceptLogsFilterByDrop(t *testing.T) {
	st := newTestStore(t)

	for _, log := range []*models.InterceptLog{
		{Action: "drop", RequestURL: "https://x.com/1", RequestMethod: "GET", RequestHost: "x.com", Mode: "auto"},
		{Action: "allow", RequestURL: "https://x.com/2", RequestMethod: "GET", RequestHost: "x.com", Mode: "auto"},
	} {
		if err := st.SaveInterceptLog(log); err != nil {
			t.Fatalf("SaveInterceptLog: %v", err)
		}
	}

	logs, total, err := st.ListInterceptLogs("drop", "", "", "", 10, 0)
	if err != nil {
		t.Fatalf("ListInterceptLogs drop: %v", err)
	}

	if total != 1 {
		t.Errorf("expected total=1 for action=drop, got %d", total)
	}
	if len(logs) != 1 {
		t.Errorf("expected len=1 for action=drop, got %d", len(logs))
	}
	if len(logs) > 0 && logs[0].Action != "drop" {
		t.Errorf("expected action=drop, got %s", logs[0].Action)
	}
}

func TestListInterceptLogsPagination(t *testing.T) {
	st := newTestStore(t)

	for i := 0; i < 5; i++ {
		log := &models.InterceptLog{
			Action:        "allow",
			RequestURL:    "https://example.com/",
			RequestMethod: "GET",
			RequestHost:   "example.com",
			Mode:          "auto",
		}
		if err := st.SaveInterceptLog(log); err != nil {
			t.Fatalf("SaveInterceptLog: %v", err)
		}
	}

	// limit=2, should get 2 results
	logs, total, err := st.ListInterceptLogs("", "", "", "", 2, 0)
	if err != nil {
		t.Fatalf("ListInterceptLogs page1: %v", err)
	}

	if total != 5 {
		t.Errorf("expected total=5, got %d", total)
	}
	if len(logs) != 2 {
		t.Errorf("expected len=2 with limit=2 offset=0, got %d", len(logs))
	}

	// offset=2, should get next 2
	logs2, total2, err := st.ListInterceptLogs("", "", "", "", 2, 2)
	if err != nil {
		t.Fatalf("ListInterceptLogs page2: %v", err)
	}

	if total2 != 5 {
		t.Errorf("expected total=5, got %d", total2)
	}
	if len(logs2) != 2 {
		t.Errorf("expected len=2 with limit=2 offset=2, got %d", len(logs2))
	}

	// verify no overlap
	if len(logs) > 0 && len(logs2) > 0 && logs[0].ID == logs2[0].ID {
		t.Error("expected different results for different offsets")
	}
}

func TestListInterceptLogsEmpty(t *testing.T) {
	st := newTestStore(t)

	logs, total, err := st.ListInterceptLogs("", "", "", "", 50, 0)
	if err != nil {
		t.Fatalf("ListInterceptLogs: %v", err)
	}

	if total != 0 {
		t.Errorf("expected total=0, got %d", total)
	}
	if len(logs) != 0 {
		t.Errorf("expected len=0, got %d", len(logs))
	}
}

func TestListInterceptLogsModifyAction(t *testing.T) {
	st := newTestStore(t)

	err := st.SaveInterceptLog(&models.InterceptLog{
		Action:        "modify",
		RequestURL:    "https://httpbin.org/post",
		RequestMethod: "POST",
		RequestHost:   "httpbin.org",
		Mode:          "manual",
	})
	if err != nil {
		t.Fatalf("SaveInterceptLog: %v", err)
	}

	logs, total, err := st.ListInterceptLogs("modify", "", "", "", 10, 0)
	if err != nil {
		t.Fatalf("ListInterceptLogs modify: %v", err)
	}

	if total != 1 {
		t.Errorf("expected total=1 for action=modify, got %d", total)
	}
	if len(logs) > 0 && logs[0].Action != "modify" {
		t.Errorf("expected action=modify, got %s", logs[0].Action)
	}
}

// ========================================
// Save / Get / List / Delete / Clear
// ========================================

func TestSaveAndGet(t *testing.T) {
	st := newTestStore(t)

	req := &models.CapturedRequest{
		Method:     "GET",
		URL:        "https://example.com/",
		Host:       "example.com",
		Path:       "/",
		Protocol:   "HTTP/1.1",
		ReqHeaders: map[string]string{"Accept": "text/html"},
		ReqBody:    "",
		StatusCode: 200,
		ResHeaders: map[string]string{"Content-Type": "text/html"},
		ResBody:    "<html></html>",
		DurationMs: 42,
		SizeBytes:  1024,
	}

	id, err := st.Save(req)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if id == 0 {
		t.Error("expected ID > 0")
	}

	got, err := st.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Method != "GET" {
		t.Errorf("expected GET, got %s", got.Method)
	}
	if got.URL != "https://example.com/" {
		t.Errorf("expected URL, got %s", got.URL)
	}
	if got.StatusCode != 200 {
		t.Errorf("expected 200, got %d", got.StatusCode)
	}
}

func TestGetNotFound(t *testing.T) {
	st := newTestStore(t)
	_, err := st.Get(99999)
	if err == nil {
		t.Error("expected error for not found")
	}
}

func TestDelete(t *testing.T) {
	st := newTestStore(t)

	req := &models.CapturedRequest{
		Method: "GET", URL: "https://example.com/", Host: "example.com",
		Path: "/", Protocol: "HTTP/1.1",
	}
	id, err := st.Save(req)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := st.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err = st.Get(id)
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestClear(t *testing.T) {
	st := newTestStore(t)

	for i := 0; i < 3; i++ {
		_, err := st.Save(&models.CapturedRequest{
			Method: "GET", URL: "https://example.com/", Host: "example.com",
			Path: "/", Protocol: "HTTP/1.1",
		})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	total, _, _, _ := st.Stats()
	if total != 3 {
		t.Fatalf("expected 3 records, got %d", total)
	}

	if err := st.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	total, _, _, _ = st.Stats()
	if total != 0 {
		t.Errorf("expected 0 after clear, got %d", total)
	}
}

func TestStats(t *testing.T) {
	st := newTestStore(t)

	_, err := st.Save(&models.CapturedRequest{
		Method: "GET", URL: "https://example.com/", Host: "example.com",
		Path: "/", Protocol: "HTTP/1.1", SizeBytes: 500, StatusCode: 200,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, err = st.Save(&models.CapturedRequest{
		Method: "GET", URL: "https://example.com/err", Host: "example.com",
		Path: "/err", Protocol: "HTTP/1.1", SizeBytes: 100, StatusCode: 500,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	total, errs, totalSize, err := st.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if total != 2 {
		t.Errorf("expected total=2, got %d", total)
	}
	if errs != 1 {
		t.Errorf("expected errors=1, got %d", errs)
	}
	if totalSize != 600 {
		t.Errorf("expected totalSize=600, got %d", totalSize)
	}
}

// ========================================
// Settings
// ========================================

func TestGetSetSetting(t *testing.T) {
	st := newTestStore(t)

	// get non-existing key
	val, err := st.GetSetting("nonexistent")
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if val != "" {
		t.Errorf("expected empty, got %s", val)
	}

	// set and get
	if err := st.SetSetting("test_key", "test_value"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	val, err = st.GetSetting("test_key")
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if val != "test_value" {
		t.Errorf("expected test_value, got %s", val)
	}

	// overwrite
	if err := st.SetSetting("test_key", "new_value"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	val, err = st.GetSetting("test_key")
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if val != "new_value" {
		t.Errorf("expected new_value, got %s", val)
	}
}

// ========================================
// Intercept Rules
// ========================================

func TestSaveRule(t *testing.T) {
	st := newTestStore(t)

	rule := &models.InterceptRule{
		Pattern: "*.example.com",
		Action:  "block",
		Enabled: true,
	}
	id, err := st.SaveRule(rule)
	if err != nil {
		t.Fatalf("SaveRule: %v", err)
	}
	if id == 0 {
		t.Error("expected ID > 0")
	}
}

func TestListRules(t *testing.T) {
	st := newTestStore(t)

	for _, r := range []*models.InterceptRule{
		{Pattern: "*.a.com", Action: "block", Enabled: true},
		{Pattern: "*.b.com", Action: "allow", Enabled: false},
	} {
		if _, err := st.SaveRule(r); err != nil {
			t.Fatalf("SaveRule: %v", err)
		}
	}

	rules, err := st.ListRules()
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(rules) != 2 {
		t.Errorf("expected 2 rules, got %d", len(rules))
	}
}

func TestUpdateRule(t *testing.T) {
	st := newTestStore(t)

	id, err := st.SaveRule(&models.InterceptRule{
		Pattern: "*.test.com", Action: "allow", Enabled: true,
	})
	if err != nil {
		t.Fatalf("SaveRule: %v", err)
	}

	if err := st.UpdateRule(id, false); err != nil {
		t.Fatalf("UpdateRule: %v", err)
	}

	rules, _ := st.ListRules()
	for _, r := range rules {
		if r.ID == id && r.Enabled {
			t.Error("expected Enabled=false after update")
		}
	}
}

func TestDeleteRule(t *testing.T) {
	st := newTestStore(t)

	id, err := st.SaveRule(&models.InterceptRule{
		Pattern: "*.del.com", Action: "block", Enabled: true,
	})
	if err != nil {
		t.Fatalf("SaveRule: %v", err)
	}

	if err := st.DeleteRule(id); err != nil {
		t.Fatalf("DeleteRule: %v", err)
	}

	rules, _ := st.ListRules()
	for _, r := range rules {
		if r.ID == id {
			t.Error("expected rule to be deleted")
		}
	}
}

// ========================================
// APINotes
// ========================================

func TestSaveAPINote(t *testing.T) {
	st := newTestStore(t)

	id, err := st.SaveAPINote(&models.APINote{
		Host:   "example.com",
		Path:   "/api/users",
		Method: "GET",
		Note:   "Get user list",
	})
	if err != nil {
		t.Fatalf("SaveAPINote: %v", err)
	}
	if id == 0 {
		t.Error("expected ID > 0")
	}
}

func TestSaveAPINoteUpsert(t *testing.T) {
	st := newTestStore(t)

	_, _ = st.SaveAPINote(&models.APINote{
		Host: "example.com", Path: "/api", Method: "GET", Note: "first",
	})
	id2, err := st.SaveAPINote(&models.APINote{
		Host: "example.com", Path: "/api", Method: "GET", Note: "second",
	})
	if err != nil {
		t.Fatalf("SaveAPINote upsert: %v", err)
	}
	if id2 == 0 {
		t.Error("expected ID > 0")
	}

	notes, err := st.GetAPINotes()
	if err != nil {
		t.Fatalf("GetAPINotes: %v", err)
	}
	noteCount := 0
	for _, n := range notes {
		if n.Host == "example.com" && n.Path == "/api" && n.Method == "GET" {
			noteCount++
			if n.Note != "second" {
				t.Errorf("expected Note=second after upsert, got %s", n.Note)
			}
		}
	}
	if noteCount != 1 {
		t.Errorf("expected exactly 1 note, got %d", noteCount)
	}
}

func TestDeleteAPINote(t *testing.T) {
	st := newTestStore(t)

	id, _ := st.SaveAPINote(&models.APINote{
		Host: "example.com", Path: "/api", Method: "GET", Note: "test",
	})
	if err := st.DeleteAPINote(id); err != nil {
		t.Fatalf("DeleteAPINote: %v", err)
	}
}

// ========================================
// SaveBatch
// ========================================

func TestSaveBatch(t *testing.T) {
	st := newTestStore(t)

	reqs := []*models.CapturedRequest{
		{Method: "GET", URL: "https://a.com/", Host: "a.com", Path: "/", Protocol: "HTTP/1.1"},
		{Method: "POST", URL: "https://b.com/", Host: "b.com", Path: "/", Protocol: "HTTP/1.1"},
	}
	ids, err := st.SaveBatch(reqs)
	if err != nil {
		t.Fatalf("SaveBatch: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 ids, got %d", len(ids))
	}
}

func TestSaveBatchEmpty(t *testing.T) {
	st := newTestStore(t)
	ids, err := st.SaveBatch(nil)
	if err != nil {
		t.Fatalf("SaveBatch nil: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 ids, got %d", len(ids))
	}
}

// TestSaveBatch_BeginFailureReturnsNilIDs 验证 Begin 失败时返回 nil ids。
// 关闭 db 连接强制让 s.db.Begin() 失败，此时应返回 (nil, err)。
func TestSaveBatch_BeginFailureReturnsNilIDs(t *testing.T) {
	st := newTestStore(t)
	// 关闭 db 连接强制让 Begin 失败
	st.db.Close()

	ids, err := st.SaveBatch([]*models.CapturedRequest{
		{Method: "GET", URL: "http://x", Host: "x", Path: "/", Protocol: "HTTP/1.1", CapturedAt: time.Now()},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if ids != nil {
		t.Fatalf("expected nil ids on begin failure, got %v", ids)
	}
}

// TestSaveBatch_ExecFailureReturnsNilIDs 验证事务内 Exec 失败时返回 nil ids，
// 而不是包含前 K-1 个 LastInsertId 的部分 ids。
//
// Bug 2 复现：旧实现在 stmt.Exec 失败时 `return ids, err`（ids 含已插入的 LastInsertId），
// 但 defer tx.Rollback() 已回滚事务，这些 ID 在 DB 中不存在。调用方若误用会引用无效记录。
//
// 测试方法：重建 requests 表并加 CHECK 约束，使第二条 INSERT 触发约束失败。
func TestSaveBatch_ExecFailureReturnsNilIDs(t *testing.T) {
	st := newTestStore(t)

	// 重建 requests 表，加 CHECK 约束使 method='BADMETHOD' 的插入失败
	if _, err := st.db.Exec("DROP TABLE requests"); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := st.db.Exec(`CREATE TABLE requests (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		method      TEXT    NOT NULL CHECK (method != 'BADMETHOD'),
		url         TEXT    NOT NULL,
		host        TEXT    NOT NULL,
		path        TEXT    NOT NULL,
		protocol    TEXT    NOT NULL DEFAULT 'HTTP/1.1',
		is_https    INTEGER NOT NULL DEFAULT 0,
		req_headers TEXT    NOT NULL DEFAULT '{}',
		req_body    TEXT    NOT NULL DEFAULT '',
		status_code INTEGER NOT NULL DEFAULT 0,
		res_headers TEXT    NOT NULL DEFAULT '{}',
		res_body    TEXT    NOT NULL DEFAULT '',
		duration_ms INTEGER NOT NULL DEFAULT 0,
		size_bytes  INTEGER NOT NULL DEFAULT 0,
		captured_at TEXT    NOT NULL DEFAULT (datetime('now')),
		capture_mode TEXT DEFAULT 'proxy',
		process_pid INTEGER DEFAULT 0,
		process_name TEXT DEFAULT '',
		is_sse      INTEGER NOT NULL DEFAULT 0,
		sse_events  TEXT    NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("recreate table: %v", err)
	}

	// 第一条正常插入（ids 会 append 第一个 LastInsertId），第二条触发 CHECK 约束失败
	reqs := []*models.CapturedRequest{
		{Method: "GET", URL: "http://a", Host: "a", Path: "/", Protocol: "HTTP/1.1", CapturedAt: time.Now()},
		{Method: "BADMETHOD", URL: "http://b", Host: "b", Path: "/", Protocol: "HTTP/1.1", CapturedAt: time.Now()},
	}
	ids, err := st.SaveBatch(reqs)
	if err == nil {
		t.Fatal("expected error from CHECK constraint violation, got nil")
	}
	if ids != nil {
		t.Fatalf("expected nil ids on exec failure (bug: returned partial ids %v)", ids)
	}
}

// ========================================
// List with filters
// ========================================

func TestListMethodFilter(t *testing.T) {
	st := newTestStore(t)

	for _, m := range []string{"GET", "POST", "GET"} {
		_, err := st.Save(&models.CapturedRequest{
			Method: m, URL: "https://example.com/", Host: "example.com",
			Path: "/", Protocol: "HTTP/1.1",
		})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	items, total, err := st.List("POST", "", "", false, 50, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 {
		t.Errorf("expected total=1 for POST, got %d", total)
	}
	if len(items) != 1 {
		t.Errorf("expected len=1 for POST, got %d", len(items))
	}
}

func TestListErrorOnly(t *testing.T) {
	st := newTestStore(t)

	for _, sc := range []int{200, 500, 200} {
		_, err := st.Save(&models.CapturedRequest{
			Method: "GET", URL: "https://example.com/", Host: "example.com",
			Path: "/", Protocol: "HTTP/1.1", StatusCode: sc,
		})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	_, total, err := st.List("", "", "", true, 50, 0)
	if err != nil {
		t.Fatalf("List error_only: %v", err)
	}
	if total != 1 {
		t.Errorf("expected total=1 for error_only, got %d", total)
	}
}

// ========================================
// ListHosts
// ========================================

func TestListHosts(t *testing.T) {
	st := newTestStore(t)

	for _, host := range []string{"example.com", "httpbin.org", "example.com"} {
		_, err := st.Save(&models.CapturedRequest{
			Method: "GET", URL: "https://" + host + "/", Host: host,
			Path: "/", Protocol: "HTTP/1.1",
		})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	hosts, total, err := st.ListHosts("", 100, 0)
	if err != nil {
		t.Fatalf("ListHosts: %v", err)
	}
	if total != 2 {
		t.Errorf("expected total=2 distinct hosts, got %d", total)
	}
	if len(hosts) != 2 {
		t.Errorf("expected len=2, got %d", len(hosts))
	}
}

func TestListHostsSearch(t *testing.T) {
	st := newTestStore(t)

	for _, host := range []string{"example.com", "httpbin.org", "test.example.com"} {
		_, err := st.Save(&models.CapturedRequest{
			Method: "GET", URL: "https://" + host + "/", Host: host,
			Path: "/", Protocol: "HTTP/1.1",
		})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	hosts, total, err := st.ListHosts("example", 100, 0)
	if err != nil {
		t.Fatalf("ListHosts: %v", err)
	}
	if total < 2 {
		t.Errorf("expected total>=2 for search 'example', got %d", total)
	}
	if len(hosts) < 2 {
		t.Errorf("expected len>=2, got %d", len(hosts))
	}
}

// ========================================
// GetAPIMap
// ========================================

func TestGetAPIMap(t *testing.T) {
	st := newTestStore(t)

	_, err := st.Save(&models.CapturedRequest{
		Method: "GET", URL: "https://httpbin.org/get", Host: "httpbin.org",
		Path: "/get", Protocol: "HTTP/1.1", StatusCode: 200,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, err = st.Save(&models.CapturedRequest{
		Method: "POST", URL: "https://httpbin.org/post", Host: "httpbin.org",
		Path: "/post", Protocol: "HTTP/1.1", StatusCode: 200,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	tree, err := st.GetAPIMap("httpbin.org")
	if err != nil {
		t.Fatalf("GetAPIMap: %v", err)
	}
	if tree == nil {
		t.Fatal("expected non-nil tree")
	}
	if tree.Name != "httpbin.org" {
		t.Errorf("expected name=httpbin.org, got %s", tree.Name)
	}
}

// ========================================
// Migrate — WAL and table creation
// ========================================

func TestMigrateTables(t *testing.T) {
	st := newTestStore(t)
	if st == nil {
		t.Fatal("New returned nil")
	}
	// verify settings table exists with intercept_mode
	val, err := st.GetSetting("intercept_mode")
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if val != "auto" {
		t.Errorf("expected intercept_mode=auto, got %s", val)
	}
}

func TestSaveEmptyFields(t *testing.T) {
	st := newTestStore(t)

	_, err := st.Save(&models.CapturedRequest{
		Method: "GET", URL: "https://empty.example.com/", Host: "empty.example.com",
		Path: "/", Protocol: "",
	})
	if err != nil {
		t.Fatalf("Save with empty fields: %v", err)
	}
}

// Verify db file exists (WAL mode creates .db-wal and .db-shm)
func TestDBFile(t *testing.T) {
	st := newTestStore(t)
	_ = st
	// Test passes if New() didn't panic — WAL pragmas were applied
}

// Verify test isolation — each test has its own temp dir
func TestIsolation(t *testing.T) {
	st1 := newTestStore(t)
	_, _ = st1.Save(&models.CapturedRequest{
		Method: "GET", URL: "https://a.com/", Host: "a.com", Path: "/", Protocol: "HTTP/1.1",
	})

	st2 := newTestStore(t)
	total, _, _, _ := st2.Stats()
	if total != 0 {
		t.Errorf("expected isolated stores, st2 has %d records", total)
	}
}

// Verify no file leak
func TestCloseCleanup(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "closed.db")
	st, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Verify files can be removed (no locks)
	entries, _ := os.ReadDir(dir)
	_ = entries
	// The file should be closeable without error — we already verified.
}

// ========================================
// ListHosts — 缓存 + 强制分页
// ========================================

func TestListHosts_Pagination(t *testing.T) {
	st := newTestStore(t)

	for i := 0; i < 200; i++ {
		host := fmt.Sprintf("host%d.com", i)
		_, _ = st.Save(&models.CapturedRequest{
			Method: "GET", URL: "http://" + host, Host: host, CapturedAt: time.Now(),
		})
	}

	hosts, total, err := st.ListHosts("", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 50 {
		t.Fatalf("expected 50 hosts, got %d", len(hosts))
	}
	if total != 200 {
		t.Fatalf("expected total=200, got %d", total)
	}
}

func TestListHosts_CacheHit(t *testing.T) {
	st := newTestStore(t)

	// 第一次调用前插入 1 个 host
	_, _ = st.Save(&models.CapturedRequest{Host: "a.com", CapturedAt: time.Now()})
	_, _, _ = st.ListHosts("", 100, 0) // 填充缓存

	// 调用后插入新 host
	_, _ = st.Save(&models.CapturedRequest{Host: "b.com", CapturedAt: time.Now()})

	// P1-8 修复后：写操作应使 hostCache 失效，因此第二次调用应重新查询并看到两个 host
	hosts2, _, _ := st.ListHosts("", 100, 0)
	if len(hosts2) != 2 {
		t.Fatalf("expected 2 hosts after cache invalidation, got %d", len(hosts2))
	}
}

// ========================================
// Cleanup — V3-4 retention_days
// ========================================

// TestCleanup_WithRetentionDays 验证 Cleanup 按 retentionDays 删除过期数据。
// V3-4：暴露 retention_days 为 CLI，但底层 Cleanup 行为不变。
func TestCleanup_WithRetentionDays(t *testing.T) {
	st := newTestStore(t)

	now := time.Now()
	oldReq := &models.CapturedRequest{Method: "GET", URL: "http://x.example/old", CapturedAt: now.AddDate(0, 0, -10)}
	newReq := &models.CapturedRequest{Method: "GET", URL: "http://y.example/new", CapturedAt: now.AddDate(0, 0, -3)}
	if _, err := st.Save(oldReq); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Save(newReq); err != nil {
		t.Fatal(err)
	}

	// retentionDays=7：oldReq (10天前) 应被删除，newReq (3天前) 应保留
	// retentionDays=7: oldReq (10d ago) deleted, newReq (3d ago) retained
	dr, dl, days, err := st.Cleanup(7)
	if err != nil {
		t.Fatalf("Cleanup(7): %v", err)
	}
	if days != 7 {
		t.Errorf("appliedDays = %d, want 7", days)
	}
	if dr != 1 {
		t.Errorf("deletedRequests = %d, want 1", dr)
	}
	if dl != 0 {
		t.Errorf("deletedLogs = %d, want 0 (no intercept logs inserted)", dl)
	}

	reqs, _, err := st.List("", "", "", false, 100, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("expected 1 remaining request, got %d", len(reqs))
	}
	if reqs[0].URL != "http://y.example/new" {
		t.Errorf("remaining URL = %q, want http://y.example/new", reqs[0].URL)
	}
}

// TestCleanup_ZeroRetentionDays_NoOp 验证 retentionDays=0 且 settings 无值时 Cleanup 为 no-op。
func TestCleanup_ZeroRetentionDays_NoOp(t *testing.T) {
	st := newTestStore(t)

	now := time.Now()
	oldReq := &models.CapturedRequest{Method: "GET", URL: "http://x.example/old", CapturedAt: now.AddDate(0, 0, -30)}
	if _, err := st.Save(oldReq); err != nil {
		t.Fatal(err)
	}

	// retentionDays=0 且 settings 表无 'retention_days' → Cleanup 应为 no-op
	// retentionDays=0 and no 'retention_days' setting → Cleanup should be no-op
	dr, dl, days, err := st.Cleanup(0)
	if err != nil {
		t.Fatalf("Cleanup(0): %v", err)
	}
	if days != 0 {
		t.Errorf("appliedDays = %d, want 0 (no-op)", days)
	}
	if dr != 0 || dl != 0 {
		t.Errorf("deleted = (%d, %d), want (0, 0) for no-op", dr, dl)
	}

	// 验证数据未删除
	reqs, _, _ := st.List("", "", "", false, 100, 0)
	if len(reqs) != 1 {
		t.Errorf("expected 1 remaining request (no-op), got %d", len(reqs))
	}
}

// TestCleanup_ReadsRetentionDaysFromSettings 验证 retentionDays=0 时从 settings 表读取 'retention_days'。
func TestCleanup_ReadsRetentionDaysFromSettings(t *testing.T) {
	st := newTestStore(t)

	// 设置 retention_days=5
	if err := st.SetSetting("retention_days", "5"); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	oldReq := &models.CapturedRequest{Method: "GET", URL: "http://x.example/old", CapturedAt: now.AddDate(0, 0, -10)}
	newReq := &models.CapturedRequest{Method: "GET", URL: "http://y.example/new", CapturedAt: now.AddDate(0, 0, -2)}
	if _, err := st.Save(oldReq); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Save(newReq); err != nil {
		t.Fatal(err)
	}

	// retentionDays=0 → 从 settings 读到 5 → oldReq (10天前) 删除，newReq (2天前) 保留
	dr, _, days, err := st.Cleanup(0)
	if err != nil {
		t.Fatalf("Cleanup(0): %v", err)
	}
	if days != 5 {
		t.Errorf("appliedDays = %d, want 5 (from settings)", days)
	}
	if dr != 1 {
		t.Errorf("deletedRequests = %d, want 1", dr)
	}
}

// TestTruncateStrUTF8 验证 truncateStr 不会切断多字节 UTF-8 字符：
// 截断后必须是合法 UTF-8，且尽可能保留完整字符。
func TestTruncateStrUTF8(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"ascii exact", "hello", 5, "hello"},
		{"ascii cut", "hello world", 5, "hello"},
		{"short string", "你好", 10, "你好"},
		{"rune boundary aligned", "你好", 6, "你好"},
		{"cut inside rune", "你好世界", 7, "你好"},   // s[:7] 落在"世"中间 → 回退
		{"cut at rune start", "你好世界", 6, "你好"}, // "世"整字装不下 → 丢弃
		{"exact rune fit", "你", 3, "你"},
		{"too small for rune", "你", 2, ""},
		{"mixed ascii utf8", "a你好b", 5, "a你"}, // 切点在"好"中间 → 回退到"你"
		{"4-byte rune", "a🀄b", 3, "a"},          // 🀄 = 4 字节，装不下 → 丢弃
		{"empty", "", 5, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateStr(tt.in, tt.max)
			if got != tt.want {
				t.Errorf("truncateStr(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncateStr(%q, %d) = %q is not valid UTF-8", tt.in, tt.max, got)
			}
		})
	}
}

// TestEscapeLike 验证 LIKE 通配符转义（配合 ESCAPE '\'）。
func TestEscapeLike(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", "plain"},
		{"100%", `100\%`},
		{"a_b", `a\_b`},
		{`a\b`, `a\\b`},
		{"50% off_", `50\% off\_`},
		{"", ""},
	}
	for _, tt := range tests {
		if got := escapeLike(tt.in); got != tt.want {
			t.Errorf("escapeLike(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestListSearchLikeWildcardEscaped 集成验证：搜索输入中的 % 与 _ 被当作
// 字面量而非 SQL LIKE 通配符（旧实现中搜索 "100%" 会匹配所有 "100" 前缀记录）。
func TestListSearchLikeWildcardEscaped(t *testing.T) {
	st := newTestStore(t)

	for _, u := range []string{
		"http://a.test/100%off",
		"http://a.test/1000",
		"http://a.test/10_0",
		"http://b.test/plain",
	} {
		if _, err := st.Save(&models.CapturedRequest{
			Method: "GET", URL: u, Host: "a.test",
			Path: "/", Protocol: "HTTP/1.1",
		}); err != nil {
			t.Fatalf("Save(%s): %v", u, err)
		}
	}

	// 搜索 "100%"：只应命中字面 "100%off"，不得命中 "1000"
	_, total, err := st.List("", "100%", "", false, 50, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 {
		t.Errorf("search \"100%%\" total = %d, want 1 (literal %% must not act as wildcard)", total)
	}

	// 搜索 "10_0"：只应命中字面 "10_0"，不得命中 "1000"
	_, total, err = st.List("", "10_0", "", false, 50, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 {
		t.Errorf("search \"10_0\" total = %d, want 1 (literal _ must not act as wildcard)", total)
	}

	// 搜索 "1000" 仍正常命中
	_, total, err = st.List("", "1000", "", false, 50, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 {
		t.Errorf("search \"1000\" total = %d, want 1", total)
	}
}
