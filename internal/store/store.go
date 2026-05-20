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
	db.SetMaxOpenConns(1) // SQLite 单写
	db.SetConnMaxLifetime(0)

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
	`
	_, err := s.db.Exec(schema)
	return err
}

// Save 保存一条捕获的请求
func (s *Store) Save(req *models.CapturedRequest) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	reqHeadersJSON, _ := json.Marshal(req.ReqHeaders)
	resHeadersJSON, _ := json.Marshal(req.ResHeaders)

	if req.CapturedAt.IsZero() {
		req.CapturedAt = time.Now()
	}

	result, err := s.db.Exec(
		`INSERT INTO requests (method, url, host, path, protocol, is_https, req_headers, req_body, status_code, res_headers, res_body, duration_ms, size_bytes, captured_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.Method, req.URL, req.Host, req.Path, req.Protocol, boolToInt(req.IsHTTPS),
		string(reqHeadersJSON), req.ReqBody, req.StatusCode, string(resHeadersJSON),
		req.ResBody, req.DurationMs, req.SizeBytes, req.CapturedAt.Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("insert request: %w", err)
	}

	return result.LastInsertId()
}

// List 获取请求列表（分页 + 过滤）
func (s *Store) List(method, search string, errorOnly bool, limit, offset int) ([]models.RequestListItem, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	where := []string{"1=1"}
	args := []interface{}{}

	if method != "" && method != "all" {
		where = append(where, "method = ?")
		args = append(args, method)
	}
	if search != "" {
		where = append(where, "(url LIKE ? OR host LIKE ? OR CAST(status_code AS TEXT) LIKE ?)")
		pattern := "%" + search + "%"
		args = append(args, pattern, pattern, pattern)
	}
	if errorOnly {
		where = append(where, "status_code >= 400")
	}

	whereClause := strings.Join(where, " AND ")

	// 总数
	var total int
	countSQL := fmt.Sprintf("SELECT COUNT(*) FROM requests WHERE %s", whereClause)
	if err := s.db.QueryRow(countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count: %w", err)
	}

	// 列表
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

	// 确保 maps 不为 nil
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

// Close 关闭数据库
func (s *Store) Close() error {
	return s.db.Close()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
