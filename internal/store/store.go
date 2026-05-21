package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"packetlab/internal/models"

	_ "modernc.org/sqlite"
)

// Store 持久化存储
type Store struct {
	db *sql.DB
	mu sync.RWMutex
}

// New 创建存储实例
func New(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// SQLite 性能优化
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	// WAL 模式提升并发性能
	db.Exec("PRAGMA journal_mode=WAL")
	db.Exec("PRAGMA synchronous=NORMAL")
	db.Exec("PRAGMA cache_size=-8000") // 8MB cache
	db.Exec("PRAGMA busy_timeout=5000")

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return s, nil
}

// migrate 建表
func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS requests (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		method      TEXT    NOT NULL,
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
		captured_at TEXT    NOT NULL DEFAULT (datetime('now'))
	);

	CREATE INDEX IF NOT EXISTS idx_requests_host ON requests(host);
	CREATE INDEX IF NOT EXISTS idx_requests_method ON requests(method);
	CREATE INDEX IF NOT EXISTS idx_requests_captured_at ON requests(captured_at);
	CREATE INDEX IF NOT EXISTS idx_requests_host_path ON requests(host, path);

	CREATE TABLE IF NOT EXISTS api_notes (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		host       TEXT    NOT NULL,
		path       TEXT    NOT NULL DEFAULT '/',
		method     TEXT    NOT NULL DEFAULT '',
		note       TEXT    NOT NULL DEFAULT '',
		created_at TEXT    NOT NULL DEFAULT (datetime('now')),
		updated_at TEXT    NOT NULL DEFAULT (datetime('now')),
		UNIQUE(host, path, method)
	);

	CREATE INDEX IF NOT EXISTS idx_api_notes_host ON api_notes(host);
	`
	_, err := s.db.Exec(schema)
	return err
}

// Save 保存一条捕获的请求
func (s *Store) Save(req *models.CapturedRequest) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(req)
}

// saveLocked 内部保存（调用方需持有锁）
func (s *Store) saveLocked(req *models.CapturedRequest) (int64, error) {
	reqHeadersJSON, _ := json.Marshal(req.ReqHeaders)
	resHeadersJSON, _ := json.Marshal(req.ResHeaders)

	if req.CapturedAt.IsZero() {
		req.CapturedAt = time.Now()
	}

	result, err := s.db.Exec(
		`INSERT INTO requests (method, url, host, path, protocol, is_https, req_headers, req_body, status_code, res_headers, res_body, duration_ms, size_bytes, captured_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.Method, req.URL, req.Host, req.Path, req.Protocol, boolToInt(req.IsHTTPS),
		string(reqHeadersJSON), truncateStr(req.ReqBody, 32768),
		req.StatusCode, string(resHeadersJSON),
		truncateStr(req.ResBody, 65536), req.DurationMs, req.SizeBytes,
		req.CapturedAt.Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("insert request: %w", err)
	}

	return result.LastInsertId()
}

// SaveBatch 批量保存（事务内单次提交）
func (s *Store) SaveBatch(reqs []*models.CapturedRequest) ([]int64, error) {
	if len(reqs) == 0 {
		return nil, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO requests (method, url, host, path, protocol, is_https, req_headers, req_body, status_code, res_headers, res_body, duration_ms, size_bytes, captured_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	ids := make([]int64, 0, len(reqs))
	for _, req := range reqs {
		reqHeadersJSON, _ := json.Marshal(req.ReqHeaders)
		resHeadersJSON, _ := json.Marshal(req.ResHeaders)
		if req.CapturedAt.IsZero() {
			req.CapturedAt = time.Now()
		}

		result, err := stmt.Exec(
			req.Method, req.URL, req.Host, req.Path, req.Protocol, boolToInt(req.IsHTTPS),
			string(reqHeadersJSON), truncateStr(req.ReqBody, 32768),
			req.StatusCode, string(resHeadersJSON), truncateStr(req.ResBody, 65536),
			req.DurationMs, req.SizeBytes, req.CapturedAt.Format(time.RFC3339),
		)
		if err != nil {
			return ids, fmt.Errorf("exec batch: %w", err)
		}
		id, _ := result.LastInsertId()
		ids = append(ids, id)
	}

	if err := tx.Commit(); err != nil {
		return ids, fmt.Errorf("commit: %w", err)
	}

	return ids, nil
}

// List 获取请求列表（分页 + 过滤）
func (s *Store) List(method, search, host string, errorOnly bool, limit, offset int) ([]models.RequestListItem, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	where := []string{"1=1"}
	args := []interface{}{}

	if method != "" && method != "all" {
		where = append(where, "method = ?")
		args = append(args, method)
	}
	if host != "" {
		where = append(where, "host = ?")
		args = append(args, host)
	} else if search != "" {
		where = append(where, "(url LIKE ? OR host LIKE ? OR CAST(status_code AS TEXT) LIKE ?)")
		pattern := "%" + search + "%"
		args = append(args, pattern, pattern, pattern)
	}
	if errorOnly {
		where = append(where, "status_code >= 400")
	}

	whereClause := strings.Join(where, " AND ")

	var total int
	countSQL := fmt.Sprintf("SELECT COUNT(*) FROM requests WHERE %s", whereClause)
	if err := s.db.QueryRow(countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count: %w", err)
	}

	querySQL := fmt.Sprintf(
		`SELECT id, method, url, host, status_code, duration_ms, size_bytes, captured_at, is_https
		 FROM requests WHERE %s ORDER BY id DESC LIMIT ? OFFSET ?`, whereClause)
	args = append(args, limit, offset)

	rows, err := s.db.Query(querySQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var items []models.RequestListItem
	for rows.Next() {
		var item models.RequestListItem
		var capturedAt string
		if err := rows.Scan(&item.ID, &item.Method, &item.URL, &item.Host,
			&item.StatusCode, &item.DurationMs, &item.SizeBytes, &capturedAt, &item.IsHTTPS); err != nil {
			return nil, 0, fmt.Errorf("scan: %w", err)
		}
		item.CapturedAt, _ = time.Parse(time.RFC3339, capturedAt)
		items = append(items, item)
	}

	return items, total, nil
}

// Get 获取单条请求完整详情
func (s *Store) Get(id int64) (*models.CapturedRequest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var req models.CapturedRequest
	var reqHeadersJSON, resHeadersJSON, capturedAt string

	err := s.db.QueryRow(
		`SELECT id, method, url, host, path, protocol, is_https, req_headers, req_body, status_code, res_headers, res_body, duration_ms, size_bytes, captured_at
		 FROM requests WHERE id = ?`, id,
	).Scan(&req.ID, &req.Method, &req.URL, &req.Host, &req.Path, &req.Protocol, &req.IsHTTPS,
		&reqHeadersJSON, &req.ReqBody, &req.StatusCode, &resHeadersJSON, &req.ResBody,
		&req.DurationMs, &req.SizeBytes, &capturedAt)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("request not found: %d", id)
	}
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}

	json.Unmarshal([]byte(reqHeadersJSON), &req.ReqHeaders)
	json.Unmarshal([]byte(resHeadersJSON), &req.ResHeaders)
	req.CapturedAt, _ = time.Parse(time.RFC3339, capturedAt)

	if req.ReqHeaders == nil {
		req.ReqHeaders = make(map[string]string)
	}
	if req.ResHeaders == nil {
		req.ResHeaders = make(map[string]string)
	}

	return &req, nil
}

// Delete 删除单条请求
func (s *Store) Delete(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM requests WHERE id = ?", id)
	return err
}

// Clear 清空所有记录
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM requests")
	return err
}

// Stats 统计信息
func (s *Store) Stats() (total int, errors int, totalSize int64, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if err := s.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(size_bytes),0) FROM requests").Scan(&total, &totalSize); err != nil {
		return 0, 0, 0, err
	}
	s.db.QueryRow("SELECT COUNT(*) FROM requests WHERE status_code >= 400").Scan(&errors)
	return
}

// ========================================
// API Map / Notes
// ========================================

// SaveAPINote 保存/更新 API 备注
func (s *Store) SaveAPINote(note *models.APINote) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec(
		`INSERT INTO api_notes (host, path, method, note, updated_at) VALUES (?, ?, ?, ?, datetime('now'))
		 ON CONFLICT(host, path, method) DO UPDATE SET note = excluded.note, updated_at = datetime('now')`,
		note.Host, note.Path, note.Method, note.Note,
	)
	if err != nil {
		return 0, fmt.Errorf("save note: %w", err)
	}
	return result.LastInsertId()
}

// DeleteAPINote 删除 API 备注
func (s *Store) DeleteAPINote(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM api_notes WHERE id = ?", id)
	return err
}

// GetAPINotes 获取所有 API 备注
func (s *Store) GetAPINotes() ([]models.APINote, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT id, host, path, method, note, created_at, updated_at FROM api_notes ORDER BY host, path")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var notes []models.APINote
	for rows.Next() {
		var n models.APINote
		var createdAt, updatedAt string
		if err := rows.Scan(&n.ID, &n.Host, &n.Path, &n.Method, &n.Note, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		n.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		n.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
		notes = append(notes, n)
	}
	return notes, nil
}

// APIMapNode API 地图树节点
type APIMapNode struct {
	Name     string        `json:"name"`
	FullPath string        `json:"full_path"`
	Method   string        `json:"method,omitempty"`
	Count    int           `json:"count"`
	Methods  []string      `json:"methods,omitempty"`
	Statuses map[int]int   `json:"statuses,omitempty"`
	Note     string        `json:"note,omitempty"`
	NoteID   int64         `json:"note_id,omitempty"`
	Children []*APIMapNode `json:"children,omitempty"`
	IsLeaf   bool          `json:"is_leaf"`
}

// methodInfo 方法统计信息
type methodInfo struct {
	count    int
	statuses map[int]int
}

// GetAPIMap 按 host 获取 API 地图树
func (s *Store) GetAPIMap(host string) (*APIMapNode, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 查询该 host 下所有不同路径+方法的组合，及出现次数和状态码分布
	rows, err := s.db.Query(
		`SELECT path, method, COUNT(*) as cnt, status_code
		 FROM requests WHERE host = ? AND path != '' AND method != 'CONNECT'
		 GROUP BY path, method, status_code
		 ORDER BY path, method`, host,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 路径 → {方法 → {count, statuses}}
	pathMethods := make(map[string]map[string]*methodInfo)

	for rows.Next() {
		var path, method string
		var cnt, status int
		if err := rows.Scan(&path, &method, &cnt, &status); err != nil {
			continue
		}
		if pathMethods[path] == nil {
			pathMethods[path] = make(map[string]*methodInfo)
		}
		if pathMethods[path][method] == nil {
			pathMethods[path][method] = &methodInfo{statuses: make(map[int]int)}
		}
		pathMethods[path][method].count += cnt
		pathMethods[path][method].statuses[status] += cnt
	}

	// 加载备注
	notesMap := make(map[string]models.APINote)
	noteRows, err := s.db.Query("SELECT path, method, note, id FROM api_notes WHERE host = ?", host)
	if err == nil {
		defer noteRows.Close()
		for noteRows.Next() {
			var n models.APINote
			noteRows.Scan(&n.Path, &n.Method, &n.Note, &n.ID)
			key := n.Path + "|" + n.Method
			notesMap[key] = n
		}
	}

	// 构建树
	root := &APIMapNode{
		Name:     host,
		FullPath: "/",
		Children: []*APIMapNode{},
	}

	for path, methods := range pathMethods {
		parts := splitPath(path)
		insertPath(root, parts, path, methods, notesMap)
	}

	return root, nil
}

// ListHosts 获取捕获过的 host 列表（支持搜索 + 分页）
func (s *Store) ListHosts(search string, limit, offset int) ([]string, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > 200 {
		limit = 50
	}

	where := "WHERE host != ''"
	args := []interface{}{}
	if search != "" {
		where += " AND host LIKE ?"
		args = append(args, "%"+search+"%")
	}

	// 总数
	var total int
	countSQL := fmt.Sprintf("SELECT COUNT(DISTINCT host) FROM requests %s", where)
	if err := s.db.QueryRow(countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// 分页 — 按请求数降序
	args = append(args, limit, offset)
	rows, err := s.db.Query(
		fmt.Sprintf("SELECT host, COUNT(*) as cnt FROM requests %s GROUP BY host ORDER BY cnt DESC LIMIT ? OFFSET ?", where),
		args...,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var hosts []string
	for rows.Next() {
		var h string
		var cnt int
		if err := rows.Scan(&h, &cnt); err != nil {
			continue
		}
		hosts = append(hosts, fmt.Sprintf("%s (%d)", h, cnt))
	}
	return hosts, total, nil
}

func insertPath(node *APIMapNode, parts []string, fullPath string, methods map[string]*methodInfo, notesMap map[string]models.APINote) {
	if len(parts) == 0 {
		return
	}

	name := parts[0]
	remaining := parts[1:]

	var child *APIMapNode
	for _, c := range node.Children {
		if c.Name == name {
			child = c
			break
		}
	}

	if child == nil {
		child = &APIMapNode{
			Name:     name,
			FullPath: "",
			Children: []*APIMapNode{},
		}
		node.Children = append(node.Children, child)
	}

	// 构建完整路径
	if node.FullPath == "/" {
		child.FullPath = "/" + name
	} else {
		child.FullPath = node.FullPath + "/" + name
	}
	if len(parts) == 1 {
		child.FullPath = fullPath // 使用原始路径
	}

	if len(remaining) == 0 {
		// 叶子节点 — 填充方法信息
		child.IsLeaf = true
		child.Count = 0
		child.Methods = []string{}
		child.Statuses = make(map[int]int)
		for method, info := range methods {
			child.Methods = append(child.Methods, method)
			child.Count += info.count
			for s, c := range info.statuses {
				child.Statuses[s] += c
			}
			key := fullPath + "|" + method
			if note, ok := notesMap[key]; ok {
				child.Note = note.Note
				child.NoteID = note.ID
			}
		}
	} else {
		insertPath(child, remaining, fullPath, methods, notesMap)
	}
}

func splitPath(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return []string{}
	}
	return strings.Split(path, "/")
}

// ========================================
// Utilities
// ========================================

func (s *Store) Close() error {
	return s.db.Close()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func truncateStr(s string, maxLen int) string {
	if len(s) > maxLen {
		return s[:maxLen]
	}
	return s
}
