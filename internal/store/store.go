package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"packetlab/internal/models"

	_ "modernc.org/sqlite"
)

// Store 持久化存储
type Store struct {
	db    *sql.DB
	dbRO  *sql.DB // 只读连接，支持 WAL 并发读
	mu    sync.RWMutex
}

// New 创建存储实例
func New(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// 主连接用于写操作
	db.SetMaxOpenConns(5)
	db.SetConnMaxLifetime(0)
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=OFF",
		"PRAGMA cache_size=-32000",
		"PRAGMA busy_timeout=5000",
		"PRAGMA wal_autocheckpoint=5000",
		"PRAGMA mmap_size=268435456",
	} {
		if _, err := db.Exec(p); err != nil {
			slog.Warn("PRAGMA failed", "pragma", p, "error", err)
		}
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	// 初始化只读连接（利用 WAL 并发读）
	s.initReadConn(dbPath)

	return s, nil
}

func (s *Store) initReadConn(dbPath string) {
	dbRO, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		slog.Warn("failed to open read-only DB, reads will use write conn", "error", err)
		return
	}
	dbRO.SetMaxOpenConns(2)
	dbRO.SetConnMaxLifetime(0)
	s.dbRO = dbRO
}

// migrate 建表（版本化迁移）
func (s *Store) migrate() error {
	s.db.Exec("CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)")

	var currentVersion int
	s.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&currentVersion)

	type migration struct {
		version int
		sql     string
	}

	migrations := []migration{
		{1, `CREATE TABLE IF NOT EXISTS requests (
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
		)`},
		{2, `CREATE INDEX IF NOT EXISTS idx_requests_host ON requests(host)`},
		{3, `CREATE INDEX IF NOT EXISTS idx_requests_method ON requests(method)`},
		{4, `CREATE INDEX IF NOT EXISTS idx_requests_captured_at ON requests(captured_at)`},
		{5, `CREATE INDEX IF NOT EXISTS idx_requests_host_path ON requests(host, path)`},
		{6, `CREATE TABLE IF NOT EXISTS api_notes (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			host       TEXT NOT NULL,
			path       TEXT NOT NULL DEFAULT '/',
			method     TEXT NOT NULL DEFAULT '',
			note       TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			UNIQUE(host, path, method)
		)`},
		{7, `CREATE INDEX IF NOT EXISTS idx_api_notes_host ON api_notes(host)`},
		{8, `CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`},
		{9, `CREATE TABLE IF NOT EXISTS intercept_rules (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			pattern    TEXT NOT NULL,
			action     TEXT NOT NULL DEFAULT 'allow',
			enabled    INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`},
		{10, `CREATE TABLE IF NOT EXISTS intercept_logs (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			action         TEXT NOT NULL,
			request_url    TEXT NOT NULL,
			request_method TEXT NOT NULL DEFAULT 'GET',
			request_host   TEXT NOT NULL DEFAULT '',
			rule_pattern   TEXT NOT NULL DEFAULT '',
			mode           TEXT NOT NULL DEFAULT 'manual',
			created_at     TEXT NOT NULL DEFAULT (datetime('now'))
		)`},
		{11, `CREATE INDEX IF NOT EXISTS idx_intercept_logs_action ON intercept_logs(action)`},
		{12, `CREATE INDEX IF NOT EXISTS idx_intercept_logs_created_at ON intercept_logs(created_at)`},
		{13, `INSERT OR IGNORE INTO settings (key, value) VALUES ('intercept_mode', 'auto')`},
		{14, `ALTER TABLE requests ADD COLUMN capture_mode TEXT DEFAULT 'proxy'`},
		{15, `ALTER TABLE requests ADD COLUMN process_pid INTEGER DEFAULT 0`},
		{16, `ALTER TABLE requests ADD COLUMN process_name TEXT DEFAULT ''`},
	}

	for _, m := range migrations {
		if m.version <= currentVersion {
			continue
		}
		if _, err := s.db.Exec(m.sql); err != nil {
			if isDuplicateColumnError(err) {
				slog.Warn("migrate alter (column exists, skipped)", "version", m.version, "error", err)
			} else {
				return fmt.Errorf("migrate v%d: %w", m.version, err)
			}
		}
		if _, err := s.db.Exec("INSERT INTO schema_migrations (version) VALUES (?)", m.version); err != nil {
			return fmt.Errorf("record migration v%d: %w", m.version, err)
		}
		slog.Info("migration applied", "version", m.version)
	}

	return nil
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
		`INSERT INTO requests (method, url, host, path, protocol, is_https, req_headers, req_body, status_code, res_headers, res_body, duration_ms, size_bytes, captured_at, capture_mode, process_pid, process_name)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.Method, req.URL, req.Host, req.Path, req.Protocol, boolToInt(req.IsHTTPS),
		string(reqHeadersJSON), truncateStr(req.ReqBody, 32768),
		req.StatusCode, string(resHeadersJSON),
		truncateStr(req.ResBody, 65536), req.DurationMs, req.SizeBytes,
		req.CapturedAt.Format(time.RFC3339),
		req.CaptureMode, req.ProcessPID, req.ProcessName,
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
		`INSERT INTO requests (method, url, host, path, protocol, is_https, req_headers, req_body, status_code, res_headers, res_body, duration_ms, size_bytes, captured_at, capture_mode, process_pid, process_name)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
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
			req.CaptureMode, req.ProcessPID, req.ProcessName,
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
		where = append(where, "(host = ? OR host LIKE ? || ':%')")
		hostNoPort := stripPort(host)
		args = append(args, host, hostNoPort)
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
		`SELECT id, method, url, host, status_code, duration_ms, size_bytes, captured_at, is_https, capture_mode, process_pid, process_name
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
			&item.StatusCode, &item.DurationMs, &item.SizeBytes, &capturedAt, &item.IsHTTPS,
			&item.CaptureMode, &item.ProcessPID, &item.ProcessName); err != nil {
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
		`SELECT id, method, url, host, path, protocol, is_https, req_headers, req_body, status_code, res_headers, res_body, duration_ms, size_bytes, captured_at, capture_mode, process_pid, process_name
		 FROM requests WHERE id = ?`, id,
	).Scan(&req.ID, &req.Method, &req.URL, &req.Host, &req.Path, &req.Protocol, &req.IsHTTPS,
		&reqHeadersJSON, &req.ReqBody, &req.StatusCode, &resHeadersJSON, &req.ResBody,
		&req.DurationMs, &req.SizeBytes, &capturedAt,
		&req.CaptureMode, &req.ProcessPID, &req.ProcessName)

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

// ListFull 获取请求完整详情列表（分页 + 过滤，消除 N+1 查询）
func (s *Store) ListFull(method, search, host string, errorOnly bool, limit, offset int) ([]models.CapturedRequest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	where := []string{"1=1"}
	args := []interface{}{}

	if method != "" && method != "all" {
		where = append(where, "method = ?")
		args = append(args, method)
	}
	if host != "" {
		where = append(where, "(host = ? OR host LIKE ? || ':%')")
		hostNoPort := stripPort(host)
		args = append(args, host, hostNoPort)
	} else if search != "" {
		where = append(where, "(url LIKE ? OR host LIKE ? OR CAST(status_code AS TEXT) LIKE ?)")
		pattern := "%" + search + "%"
		args = append(args, pattern, pattern, pattern)
	}
	if errorOnly {
		where = append(where, "status_code >= 400")
	}

	whereClause := strings.Join(where, " AND ")

	querySQL := fmt.Sprintf(
		`SELECT id, method, url, host, path, protocol, is_https, req_headers, req_body, status_code, res_headers, res_body, duration_ms, size_bytes, captured_at
		 FROM requests WHERE %s ORDER BY id DESC LIMIT ? OFFSET ?`, whereClause)
	args = append(args, limit, offset)

	rows, err := s.db.Query(querySQL, args...)
	if err != nil {
		return nil, fmt.Errorf("query full: %w", err)
	}
	defer rows.Close()

	var items []models.CapturedRequest
	for rows.Next() {
		var req models.CapturedRequest
		var reqHeadersJSON, resHeadersJSON, capturedAt string
		if err := rows.Scan(&req.ID, &req.Method, &req.URL, &req.Host, &req.Path, &req.Protocol, &req.IsHTTPS,
			&reqHeadersJSON, &req.ReqBody, &req.StatusCode, &resHeadersJSON, &req.ResBody,
			&req.DurationMs, &req.SizeBytes, &capturedAt); err != nil {
			continue
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
		items = append(items, req)
	}

	return items, nil
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
		return 0, 0, 0, fmt.Errorf("stats count: %w", err)
	}
	// 错误计数失败不影响主统计返回
	if scanErr := s.db.QueryRow("SELECT COUNT(*) FROM requests WHERE status_code >= 400").Scan(&errors); scanErr != nil {
		errors = 0
	}
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
		return nil, fmt.Errorf("query notes: %w", err)
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
	Children []*APIMapNode `json:"children"` // 必须始终输出，空时输出 [] 而非省略
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

	// 去端口：httpbin.org:443 → httpbin.org，解决 host 精确匹配不到非 CONNECT 请求的问题
	hostNoPort := stripPort(host)

	// 查询该 host 下所有不同路径+方法的组合，及出现次数和状态码分布
	// 匹配: 精确host / 去端口host / 去端口host:任意端口（如 httpbin.org → 匹配 httpbin.org:443 等）
	rows, err := s.db.Query(
		`SELECT path, method, COUNT(*) as cnt, status_code
		 FROM requests WHERE (host = ? OR host = ? OR host LIKE ? || ':%') AND path != ''
		 GROUP BY path, method, status_code
		 ORDER BY path, method`, host, hostNoPort, hostNoPort,
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

	// 加载备注 — 同样匹配 host / hostNoPort / hostNoPort:*
	notesMap := make(map[string]models.APINote)
	noteRows, err := s.db.Query("SELECT path, method, note, id FROM api_notes WHERE host = ? OR host = ? OR host LIKE ? || ':%'", host, hostNoPort, hostNoPort)
	if err == nil {
		defer noteRows.Close()
		for noteRows.Next() {
			var n models.APINote
			if err := noteRows.Scan(&n.Path, &n.Method, &n.Note, &n.ID); err != nil {
				slog.Warn("scan note row failed", "error", err)
				continue
			}
			key := n.Path + "|" + n.Method
			notesMap[key] = n
		}
		if err := noteRows.Err(); err != nil {
			slog.Warn("note rows iteration error", "error", err)
		}
	}

	// 构建树
	root := &APIMapNode{
		Name:     host,
		FullPath: "/",
		Children: []*APIMapNode{},
	}

	for path, methods := range pathMethods {
		// 根路径 / 直接附加到 root 节点
		if path == "/" {
			root.IsLeaf = true
			root.Count = 0
			root.Methods = []string{}
			root.Statuses = make(map[int]int)
			for method, info := range methods {
				root.Methods = append(root.Methods, method)
				root.Count += info.count
				for s, c := range info.statuses {
					root.Statuses[s] += c
				}
				key := path + "|" + method
				if note, ok := notesMap[key]; ok {
					root.Note = note.Note
					root.NoteID = note.ID
				}
			}
			continue
		}
		parts := splitPath(path)
		insertPath(root, parts, path, methods, notesMap)
	}

	// 后序遍历 — 中间节点聚合子节点的 method/count/status 统计
	aggregateNodeStats(root)

	return root, nil
}

// aggregateNodeStats 后序遍历聚合统计信息到中间节点
func aggregateNodeStats(node *APIMapNode) {
	if node == nil {
		return
	}
	for _, child := range node.Children {
		aggregateNodeStats(child)
	}

	// 如果节点有子节点，则不再是叶子节点
	if len(node.Children) > 0 {
		node.IsLeaf = false
	}

	// 聚合子节点数据到当前节点
	methodSet := make(map[string]bool)
	for _, m := range node.Methods {
		methodSet[m] = true
	}
	for _, child := range node.Children {
		for _, m := range child.Methods {
			if !methodSet[m] {
				methodSet[m] = true
				node.Methods = append(node.Methods, m)
			}
		}
		node.Count += child.Count
		if len(child.Statuses) > 0 {
			if node.Statuses == nil {
				node.Statuses = make(map[int]int)
			}
			for s, c := range child.Statuses {
				node.Statuses[s] += c
			}
		}
	}
}

// ListHosts 获取捕获过的 host 列表（支持搜索 + 分页）
func (s *Store) ListHosts(search string, limit, offset int) ([]string, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > 500 {
		limit = 100
	}

	where := "WHERE host != ''"
	args := []interface{}{}
	if search != "" {
		where += " AND host LIKE ?"
		args = append(args, "%"+search+"%")
	}

	// 总数 — 先查所有 host 再在内存中去端口去重计数
	var total int
	totalRows, err := s.db.Query(fmt.Sprintf("SELECT host FROM requests %s", where), args...)
	if err != nil {
		return nil, 0, err
	}
	totalSet := make(map[string]struct{})
	for totalRows.Next() {
		var h string
		if err := totalRows.Scan(&h); err != nil {
			continue
		}
		totalSet[stripPort(h)] = struct{}{}
	}
	totalRows.Close()
	total = len(totalSet)

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

	// 去端口后合并: httpbin.org + httpbin.org:443 → httpbin.org (7)
	hostCounts := make(map[string]int)
	var hostOrder []string
	distinctTotal := 0
	for rows.Next() {
		var h string
		var cnt int
		if err := rows.Scan(&h, &cnt); err != nil {
			continue
		}
		baseHost := stripPort(h)
		if _, exists := hostCounts[baseHost]; !exists {
			hostOrder = append(hostOrder, baseHost)
			distinctTotal++
		}
		hostCounts[baseHost] += cnt
	}

	var hosts []string
	for _, h := range hostOrder {
		hosts = append(hosts, fmt.Sprintf("%s (%d)", h, hostCounts[h]))
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
		// 中间节点 — 之前若是叶子节点则重置
		if child.IsLeaf {
			child.IsLeaf = false
		}
		insertPath(child, remaining, fullPath, methods, notesMap)
	}
}

func stripPort(host string) string {
	// 去掉末尾端口号，如 httpbin.org:443 → httpbin.org
	if idx := strings.LastIndex(host, ":"); idx > 0 {
		// 排除 IPv6 [::1] 的情况
		if !strings.HasPrefix(host, "[") {
			return host[:idx]
		}
	}
	return host
}

func splitPath(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return []string{}
	}
	return strings.Split(path, "/")
}

// ========================================
// Intercept Logs
// ========================================

// SaveInterceptLog 保存一条拦截操作日志
func (s *Store) SaveInterceptLog(log *models.InterceptLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec(
		`INSERT INTO intercept_logs (action, request_url, request_method, request_host, rule_pattern, mode, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, datetime('now'))`,
		log.Action, log.RequestURL, log.RequestMethod, log.RequestHost, log.RulePattern, log.Mode,
	)
	if err != nil {
		return fmt.Errorf("insert intercept log: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("get last insert id: %w", err)
	}
	log.ID = id
	log.CreatedAt = time.Now().UTC().Format("2006-01-02 15:04:05")

	return nil
}

// ListInterceptLogs 查询拦截日志（支持按 action 过滤、分页）
func (s *Store) ListInterceptLogs(action, since string, limit, offset int) ([]models.InterceptLog, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	where := []string{"1=1"}
	args := []interface{}{}

	if action != "" {
		where = append(where, "action = ?")
		args = append(args, action)
	}
	if since != "" {
		where = append(where, "created_at >= ?")
		args = append(args, since)
	}

	whereClause := strings.Join(where, " AND ")

	var total int
	countSQL := fmt.Sprintf("SELECT COUNT(*) FROM intercept_logs WHERE %s", whereClause)
	if err := s.db.QueryRow(countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count intercept logs: %w", err)
	}

	querySQL := fmt.Sprintf(
		`SELECT id, action, request_url, request_method, request_host, rule_pattern, mode, created_at
		 FROM intercept_logs WHERE %s ORDER BY id DESC LIMIT ? OFFSET ?`, whereClause)
	args = append(args, limit, offset)

	rows, err := s.db.Query(querySQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query intercept logs: %w", err)
	}
	defer rows.Close()

	var logs []models.InterceptLog
	for rows.Next() {
		var l models.InterceptLog
		if err := rows.Scan(&l.ID, &l.Action, &l.RequestURL, &l.RequestMethod,
			&l.RequestHost, &l.RulePattern, &l.Mode, &l.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan intercept log: %w", err)
		}
		logs = append(logs, l)
	}

	if logs == nil {
		logs = []models.InterceptLog{}
	}

	return logs, total, nil
}

// ========================================
// Utilities
// ========================================

func (s *Store) Close() error {
	var errs []error
	if err := s.db.Close(); err != nil {
		errs = append(errs, err)
	}
	if s.dbRO != nil {
		if err := s.dbRO.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("close errors: %v", errs)
	}
	return nil
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

func isDuplicateColumnError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column") || strings.Contains(msg, "already exists")
}

// ========================================
// Settings
// ========================================

func (s *Store) GetSetting(key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var val string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return val, err
}

func (s *Store) SetSetting(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)", key, value)
	return err
}

// ========================================
// Intercept Rules
// ========================================

func (s *Store) SaveRule(rule *models.InterceptRule) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		"INSERT INTO intercept_rules (pattern, action, enabled) VALUES (?, ?, ?)",
		rule.Pattern, rule.Action, boolToInt(rule.Enabled),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) ListRules() ([]models.InterceptRule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query("SELECT id, pattern, action, enabled, created_at FROM intercept_rules ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []models.InterceptRule
	for rows.Next() {
		var r models.InterceptRule
		var ca string
		if err := rows.Scan(&r.ID, &r.Pattern, &r.Action, &r.Enabled, &ca); err != nil {
			continue
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339, ca)
		rules = append(rules, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	return rules, nil
}

func (s *Store) UpdateRule(id int64, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("UPDATE intercept_rules SET enabled = ? WHERE id = ?", boolToInt(enabled), id)
	return err
}

func (s *Store) DeleteRule(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM intercept_rules WHERE id = ?", id)
	return err
}
