package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"packetlab/internal/models"

	_ "modernc.org/sqlite"
)

// ErrRequestNotFound 表示 SetStarred 等操作的目标请求不存在。
// 调用方可用 errors.Is(err, store.ErrRequestNotFound) 区分 404 与其他错误。
var ErrRequestNotFound = errors.New("request not found")

// requestsInsertStmt requests 表 INSERT 语句。saveLocked 与 SaveBatch 共用，
// 避免两处列清单漂移——增列时必须同时同步这里的列与占位符数量。
const requestsInsertStmt = `INSERT INTO requests (method, url, host, path, protocol, is_https, req_headers, req_body, status_code, res_headers, res_body, duration_ms, size_bytes, captured_at, capture_mode, process_pid, process_name, is_sse, sse_events, truncated)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// Store 持久化存储
type Store struct {
	db   *sql.DB
	dbRO *sql.DB // 只读连接，支持 WAL 并发读
	mu   sync.RWMutex

	// ListHosts 缓存（仅 search="" 时缓存）
	hostCache    hostCacheEntry
	hostCacheTTL time.Duration
}

type hostCacheEntry struct {
	hosts []string
	total int
	at    time.Time
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

	s.hostCacheTTL = 5 * time.Minute

	return s, nil
}

func (s *Store) initReadConn(dbPath string) {
	dbRO, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		slog.Warn("failed to open read-only DB, reads will use write conn", "error", err)
		return
	}
	dbRO.SetMaxOpenConns(4)
	dbRO.SetConnMaxLifetime(0)
	// 只读连接也设置 busy_timeout，避免 WAL checkpoint 时读阻塞报错
	if _, err := dbRO.Exec("PRAGMA busy_timeout=5000"); err != nil {
		slog.Warn("read-only busy_timeout failed", "error", err)
	}
	s.dbRO = dbRO
}

// readDB 返回用于读操作的连接：优先 dbRO（WAL 并发读），失败回退到主连接。
// 读操作不需要 s.mu 锁：SQLite WAL 模式天然支持一写多读并发。
func (s *Store) readDB() *sql.DB {
	if s.dbRO != nil {
		return s.dbRO
	}
	return s.db
}

// migrate 建表（版本化迁移）
func (s *Store) migrate() error {
	if _, err := s.db.Exec("CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)"); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var currentVersion int
	if err := s.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&currentVersion); err != nil {
		return fmt.Errorf("query schema version: %w", err)
	}

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
		{17, `ALTER TABLE requests ADD COLUMN is_sse INTEGER NOT NULL DEFAULT 0`},
		{18, `ALTER TABLE requests ADD COLUMN sse_events TEXT NOT NULL DEFAULT ''`},
		{19, `ALTER TABLE intercept_rules ADD COLUMN method TEXT NOT NULL DEFAULT ''`},
		{20, `ALTER TABLE requests ADD COLUMN starred INTEGER NOT NULL DEFAULT 0`},
		{21, `CREATE INDEX IF NOT EXISTS idx_requests_starred ON requests(starred)`},
		{22, `ALTER TABLE requests ADD COLUMN is_llm INTEGER NOT NULL DEFAULT 0`},
		{23, `ALTER TABLE requests ADD COLUMN llm_data TEXT NOT NULL DEFAULT ''`},
		{24, `CREATE INDEX IF NOT EXISTS idx_requests_is_llm ON requests(is_llm)`},
		{25, `ALTER TABLE requests ADD COLUMN truncated INTEGER NOT NULL DEFAULT 0`},
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
		requestsInsertStmt,
		req.Method, req.URL, req.Host, req.Path, req.Protocol, boolToInt(req.IsHTTPS),
		string(reqHeadersJSON), truncateStr(req.ReqBody, 2*1024*1024),
		req.StatusCode, string(resHeadersJSON),
		truncateStr(req.ResBody, 4*1024*1024), req.DurationMs, req.SizeBytes,
		req.CapturedAt.UTC().Format(time.RFC3339),
		req.CaptureMode, req.ProcessPID, req.ProcessName,
		boolToInt(req.IsSSE), truncateStr(req.SSEEvents, 4*1024*1024),
		boolToInt(req.Truncated),
	)
	if err != nil {
		return 0, fmt.Errorf("insert request: %w", err)
	}

	s.hostCache = hostCacheEntry{}
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

	stmt, err := tx.Prepare(requestsInsertStmt)
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
			string(reqHeadersJSON), truncateStr(req.ReqBody, 2*1024*1024),
			req.StatusCode, string(resHeadersJSON), truncateStr(req.ResBody, 4*1024*1024),
			req.DurationMs, req.SizeBytes, req.CapturedAt.UTC().Format(time.RFC3339),
			req.CaptureMode, req.ProcessPID, req.ProcessName,
			boolToInt(req.IsSSE), truncateStr(req.SSEEvents, 4*1024*1024),
			boolToInt(req.Truncated),
		)
		if err != nil {
			// 事务已由 defer tx.Rollback() 回滚，前 K-1 个 LastInsertId 均无效，
			// 必须返回 nil ids 防止调用方误用已回滚的 ID。
			return nil, fmt.Errorf("exec batch: %w", err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("last insert id: %w", err)
		}
		ids = append(ids, id)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	s.hostCache = hostCacheEntry{}
	return ids, nil
}

// List 获取请求列表（分页 + 过滤）
func (s *Store) List(method, search, host string, errorOnly bool, limit, offset int) ([]models.RequestListItem, int, error) {
	where := []string{"1=1"}
	args := []interface{}{}

	if method != "" && method != "all" {
		where = append(where, "method = ?")
		args = append(args, method)
	}
	if host != "" {
		where = append(where, "(host = ? OR host = ? OR host LIKE (? || ':%') ESCAPE '\\')")
		hostNoPort := stripPort(host)
		args = append(args, host, hostNoPort, escapeLike(hostNoPort))
	}
	if search != "" {
		where = append(where, "(url LIKE ? ESCAPE '\\' OR host LIKE ? ESCAPE '\\' OR CAST(status_code AS TEXT) LIKE ? ESCAPE '\\')")
		pattern := "%" + escapeLike(search) + "%"
		args = append(args, pattern, pattern, pattern)
	}
	if errorOnly {
		where = append(where, "status_code >= 400")
	}

	whereClause := strings.Join(where, " AND ")

	var total int
	countSQL := fmt.Sprintf("SELECT COUNT(*) FROM requests WHERE %s", whereClause)
	if err := s.readDB().QueryRow(countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count: %w", err)
	}

	querySQL := fmt.Sprintf(
		`SELECT %s FROM requests WHERE %s ORDER BY id DESC LIMIT ? OFFSET ?`, requestListColumns, whereClause)
	args = append(args, limit, offset)

	rows, err := s.readDB().Query(querySQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var items []models.RequestListItem
	for rows.Next() {
		item, err := scanRequestListItem(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan: %w", err)
		}
		items = append(items, item)
	}

	return items, total, nil
}

// Get 获取单条请求完整详情
func (s *Store) Get(id int64) (*models.CapturedRequest, error) {
	var req models.CapturedRequest
	var reqHeadersJSON, resHeadersJSON, capturedAt string
	var isSSE, truncated int

	err := s.readDB().QueryRow(
		`SELECT id, method, url, host, path, protocol, is_https, req_headers, req_body, status_code, res_headers, res_body, duration_ms, size_bytes, captured_at, capture_mode, process_pid, process_name, is_sse, sse_events, truncated
		 FROM requests WHERE id = ?`, id,
	).Scan(&req.ID, &req.Method, &req.URL, &req.Host, &req.Path, &req.Protocol, &req.IsHTTPS,
		&reqHeadersJSON, &req.ReqBody, &req.StatusCode, &resHeadersJSON, &req.ResBody,
		&req.DurationMs, &req.SizeBytes, &capturedAt,
		&req.CaptureMode, &req.ProcessPID, &req.ProcessName, &isSSE, &req.SSEEvents, &truncated)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("request not found: %d", id)
	}
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}

	json.Unmarshal([]byte(reqHeadersJSON), &req.ReqHeaders)
	json.Unmarshal([]byte(resHeadersJSON), &req.ResHeaders)
	req.CapturedAt, _ = time.Parse(time.RFC3339, capturedAt)
	req.IsSSE = intToBool(isSSE)
	req.Truncated = intToBool(truncated)

	if req.ReqHeaders == nil {
		req.ReqHeaders = make(map[string]string)
	}
	if req.ResHeaders == nil {
		req.ResHeaders = make(map[string]string)
	}

	return &req, nil
}

// listWhere 构建 ListFull/ForEachFull 共用的 WHERE 子句与参数。
func (s *Store) listWhere(method, search, host string, errorOnly bool) (string, []interface{}) {
	where := []string{"1=1"}
	args := []interface{}{}

	if method != "" && method != "all" {
		where = append(where, "method = ?")
		args = append(args, method)
	}
	if host != "" {
		where = append(where, "(host = ? OR host = ? OR host LIKE (? || ':%') ESCAPE '\\')")
		hostNoPort := stripPort(host)
		args = append(args, host, hostNoPort, escapeLike(hostNoPort))
	}
	if search != "" {
		where = append(where, "(url LIKE ? ESCAPE '\\' OR host LIKE ? ESCAPE '\\' OR CAST(status_code AS TEXT) LIKE ? ESCAPE '\\')")
		pattern := "%" + escapeLike(search) + "%"
		args = append(args, pattern, pattern, pattern)
	}
	if errorOnly {
		where = append(where, "status_code >= 400")
	}

	return strings.Join(where, " AND "), args
}

// ForEachFull 流式遍历请求完整详情（分页 + 过滤，消除 N+1 查询）。
// fn 返回错误时中止遍历并返回该错误。
// 用于 HAR 导出等大结果集场景：逐条回调，避免把全部（含最大 6MB 的
// 请求/响应体）记录同时载入内存造成 OOM。
func (s *Store) ForEachFull(method, search, host string, errorOnly bool, limit, offset int, fn func(*models.CapturedRequest) error) error {
	whereClause, args := s.listWhere(method, search, host, errorOnly)

	querySQL := fmt.Sprintf(
		`SELECT id, method, url, host, path, protocol, is_https, req_headers, req_body, status_code, res_headers, res_body, duration_ms, size_bytes, captured_at, capture_mode, process_pid, process_name, is_sse, sse_events, truncated
		 FROM requests WHERE %s ORDER BY id DESC LIMIT ? OFFSET ?`, whereClause)
	args = append(args, limit, offset)

	rows, err := s.readDB().Query(querySQL, args...)
	if err != nil {
		return fmt.Errorf("query full: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var req models.CapturedRequest
		var reqHeadersJSON, resHeadersJSON, capturedAt string
		var isSSE, truncated int
		if err := rows.Scan(&req.ID, &req.Method, &req.URL, &req.Host, &req.Path, &req.Protocol, &req.IsHTTPS,
			&reqHeadersJSON, &req.ReqBody, &req.StatusCode, &resHeadersJSON, &req.ResBody,
			&req.DurationMs, &req.SizeBytes, &capturedAt,
			&req.CaptureMode, &req.ProcessPID, &req.ProcessName, &isSSE, &req.SSEEvents, &truncated); err != nil {
			slog.Warn("scan failed", "error", err)
			continue
		}
		req.IsSSE = intToBool(isSSE)
		req.Truncated = intToBool(truncated)
		json.Unmarshal([]byte(reqHeadersJSON), &req.ReqHeaders)
		json.Unmarshal([]byte(resHeadersJSON), &req.ResHeaders)
		req.CapturedAt, _ = time.Parse(time.RFC3339, capturedAt)
		if req.ReqHeaders == nil {
			req.ReqHeaders = make(map[string]string)
		}
		if req.ResHeaders == nil {
			req.ResHeaders = make(map[string]string)
		}
		if err := fn(&req); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate full: %w", err)
	}
	return nil
}

// ListFull 获取请求完整详情列表（分页 + 过滤，消除 N+1 查询）
func (s *Store) ListFull(method, search, host string, errorOnly bool, limit, offset int) ([]models.CapturedRequest, error) {
	var items []models.CapturedRequest
	if err := s.ForEachFull(method, search, host, errorOnly, limit, offset, func(req *models.CapturedRequest) error {
		items = append(items, *req)
		return nil
	}); err != nil {
		return nil, err
	}
	return items, nil
}

// Delete 删除单条请求
func (s *Store) Delete(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM requests WHERE id = ?", id)
	if err != nil {
		return err
	}
	s.hostCache = hostCacheEntry{}
	return nil
}

// SetStarred 标记/取消标记请求为收藏（starred=1 收藏，0 取消）
func (s *Store) SetStarred(id int64, starred bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := 0
	if starred {
		v = 1
	}
	res, err := s.db.Exec("UPDATE requests SET starred = ? WHERE id = ?", v, id)
	if err != nil {
		return fmt.Errorf("set starred: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %d", ErrRequestNotFound, id)
	}
	s.hostCache = hostCacheEntry{}
	return nil
}

// ListStarred 获取所有标星收藏的请求（按 id 倒序）
func (s *Store) ListStarred(limit int) ([]models.RequestListItem, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	querySQL := `SELECT ` + requestListColumns + `
			 FROM requests WHERE starred = 1 ORDER BY id DESC LIMIT ?`
	rows, err := s.readDB().Query(querySQL, limit)
	if err != nil {
		return nil, fmt.Errorf("query starred: %w", err)
	}
	defer rows.Close()

	var items []models.RequestListItem
	for rows.Next() {
		item, err := scanRequestListItem(rows)
		if err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		items = append(items, item)
	}
	return items, nil
}

// UpdateResBody 更新响应体和 SSE 事件（流式捕获 SSE 时增量更新）
func (s *Store) UpdateResBody(id int64, resBody, sseEvents string, sizeBytes int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		"UPDATE requests SET res_body = ?, sse_events = ?, size_bytes = ? WHERE id = ?",
		truncateStr(resBody, 4*1024*1024), truncateStr(sseEvents, 4*1024*1024), sizeBytes, id,
	)
	return err
}

// Clear 清空所有记录（requests + intercept_logs）
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec("DELETE FROM requests"); err != nil {
		return err
	}
	if _, err := s.db.Exec("DELETE FROM intercept_logs"); err != nil {
		return err
	}
	s.hostCache = hostCacheEntry{}
	return nil
}

// Cleanup 清理超过保留期的请求与拦截日志，返回各自删除条数。
// retentionDays <= 0 时从 settings 表读取 'retention_days'，仍为 0 则跳过。
// 上限 36500 天（≈100 年），避免 time.Duration 乘法溢出导致 cutoff 变成未来时间清空全表。
func (s *Store) Cleanup(retentionDays int) (deletedRequests, deletedLogs int64, appliedDays int, err error) {
	if retentionDays <= 0 {
		if v, gerr := s.GetSetting("retention_days"); gerr == nil {
			if d, _ := strconv.Atoi(v); d > 0 {
				retentionDays = d
			}
		}
	}
	if retentionDays <= 0 {
		return 0, 0, 0, nil
	}
	// 上限校验：防止 retentionDays 极大值导致 time.Duration(retentionDays)*24*time.Hour 溢出为负
	// 一旦溢出为负，time.Now().Add(-负值) 会得到未来时间，DELETE 会匹配全表造成数据丢失
	const maxRetentionDays = 36500 // ≈100 年
	if retentionDays > maxRetentionDays {
		retentionDays = maxRetentionDays
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 用 AddDate 避免 time.Duration 乘法溢出（AddDate 内部按日计算不会溢出）。
	// UTC 与 requests.captured_at 存储格式保持一致，否则含偏移（+08:00）与 Z 格式
	// 的字符串按字典序比较会出错，导致清理误删/漏删。
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays).Format(time.RFC3339)

	res, err := s.db.Exec("DELETE FROM requests WHERE captured_at < ?", cutoff)
	if err != nil {
		return 0, 0, retentionDays, fmt.Errorf("cleanup requests: %w", err)
	}
	deletedRequests, _ = res.RowsAffected()

	res, err = s.db.Exec("DELETE FROM intercept_logs WHERE created_at < ?", cutoff)
	if err != nil {
		return deletedRequests, 0, retentionDays, fmt.Errorf("cleanup logs: %w", err)
	}
	deletedLogs, _ = res.RowsAffected()

	s.hostCache = hostCacheEntry{}
	return deletedRequests, deletedLogs, retentionDays, nil
}

// Stats 统计信息
func (s *Store) Stats() (total int, errors int, totalSize int64, err error) {
	if err := s.readDB().QueryRow("SELECT COUNT(*), COALESCE(SUM(size_bytes),0) FROM requests").Scan(&total, &totalSize); err != nil {
		return 0, 0, 0, fmt.Errorf("stats count: %w", err)
	}
	// 错误计数失败不影响主统计返回
	if scanErr := s.readDB().QueryRow("SELECT COUNT(*) FROM requests WHERE status_code >= 400").Scan(&errors); scanErr != nil {
		slog.Warn("stats error count failed", "error", scanErr)
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

	now := time.Now().UTC().Format(time.RFC3339)
	result, err := s.db.Exec(
		`INSERT INTO api_notes (host, path, method, note, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(host, path, method) DO UPDATE SET note = excluded.note, updated_at = ?`,
		note.Host, note.Path, note.Method, note.Note, now, now, now,
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
	rows, err := s.readDB().Query("SELECT id, host, path, method, note, created_at, updated_at FROM api_notes ORDER BY host, path")
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
	// 去端口：httpbin.org:443 → httpbin.org，解决 host 精确匹配不到非 CONNECT 请求的问题
	hostNoPort := stripPort(host)

	// 查询该 host 下所有不同路径+方法的组合，及出现次数和状态码分布
	// 匹配: 精确host / 去端口host / 去端口host:任意端口（如 httpbin.org → 匹配 httpbin.org:443 等）
	rows, err := s.readDB().Query(
		`SELECT path, method, COUNT(*) as cnt, status_code
		 FROM requests WHERE (host = ? OR host = ? OR host LIKE (? || ':%') ESCAPE '\\') AND path != ''
		 GROUP BY path, method, status_code
		 ORDER BY path, method`, host, hostNoPort, escapeLike(hostNoPort),
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
	noteRows, err := s.readDB().Query("SELECT path, method, note, id FROM api_notes WHERE host = ? OR host = ? OR host LIKE (? || ':%') ESCAPE '\\'", host, hostNoPort, escapeLike(hostNoPort))
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

// ListHosts 获取捕获过的 host 列表（支持搜索 + 分页 + 缓存）
func (s *Store) ListHosts(search string, limit, offset int) ([]string, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	// 缓存命中（仅 search 为空时缓存）
	if search == "" {
		s.mu.RLock()
		entry := s.hostCache
		ttl := s.hostCacheTTL
		s.mu.RUnlock()
		if ttl > 0 && entry.total > 0 && time.Since(entry.at) < ttl {
			end := offset + limit
			if end > len(entry.hosts) {
				end = len(entry.hosts)
			}
			if offset > len(entry.hosts) {
				return nil, entry.total, nil
			}
			return entry.hosts[offset:end], entry.total, nil
		}
	}

	where := "WHERE host != ''"
	args := []interface{}{}
	if search != "" {
		where += " AND host LIKE ? ESCAPE '\\'"
		args = append(args, "%"+escapeLike(search)+"%")
	}

	// 单次查询拿 host + count（按 count 降序）
	query := fmt.Sprintf(
		"SELECT host, COUNT(*) as cnt FROM requests %s GROUP BY host ORDER BY cnt DESC",
		where,
	)
	rows, err := s.readDB().Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	// 在内存中合并去端口（httpbin.org + httpbin.org:443 → httpbin.org）
	hostCounts := make(map[string]int)
	var hostOrder []string
	for rows.Next() {
		var h string
		var cnt int
		if err := rows.Scan(&h, &cnt); err != nil {
			slog.Warn("scan host failed", "error", err)
			continue
		}
		baseHost := stripPort(h)
		if _, exists := hostCounts[baseHost]; !exists {
			hostOrder = append(hostOrder, baseHost)
		}
		hostCounts[baseHost] += cnt
	}

	// 转换为 []string（格式 "host (count)"）
	allHosts := make([]string, 0, len(hostOrder))
	for _, h := range hostOrder {
		allHosts = append(allHosts, fmt.Sprintf("%s (%d)", h, hostCounts[h]))
	}
	total := len(allHosts)

	// 缓存（仅 search 为空时）
	if search == "" {
		s.mu.Lock()
		s.hostCache = hostCacheEntry{hosts: allHosts, total: total, at: time.Now()}
		s.mu.Unlock()
	}

	// 分页切片
	end := offset + limit
	if end > len(allHosts) {
		end = len(allHosts)
	}
	if offset > len(allHosts) {
		return nil, total, nil
	}
	return allHosts[offset:end], total, nil
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
		// 中间节点 — 之前若是叶子节点则清空方法数据，避免残留统计
		if child.IsLeaf {
			child.IsLeaf = false
			child.Methods = nil
			child.Statuses = nil
			child.Note = ""
			child.NoteID = 0
		}
		insertPath(child, remaining, fullPath, methods, notesMap)
	}
}

func stripPort(host string) string {
	// net.SplitHostPort 正确处理 IPv6（含 [::1]:port 形式）和无端口情况：
	// - "httpbin.org:443" → ("httpbin.org", "443", nil)
	// - "[::1]:443"       → ("::1", "443", nil)
	// - "httpbin.org"     → ("", "", error)  → fallthrough 返回原 host
	// - "::1"             → ("", "", error)  → fallthrough 返回原 host（避免误截断 IPv6）
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
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

	now := time.Now().UTC().Format(time.RFC3339)
	result, err := s.db.Exec(
		`INSERT INTO intercept_logs (action, request_url, request_method, request_host, rule_pattern, mode, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		log.Action, log.RequestURL, log.RequestMethod, log.RequestHost, log.RulePattern, log.Mode, now,
	)
	if err != nil {
		return fmt.Errorf("insert intercept log: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("get last insert id: %w", err)
	}
	log.ID = id

	// 从数据库读回真实的 UTC 时间戳
	err = s.db.QueryRow(
		`SELECT created_at FROM intercept_logs WHERE id = ?`, id,
	).Scan(&log.CreatedAt)
	if err != nil {
		// 如果查询失败但插入已经成功，忽略读回，日志已保存到数据库
		slog.Warn("failed to get created_at from db after insert", "error", err)
	}

	return nil
}

// ListInterceptLogs 查询拦截日志（支持按 action/host/pattern 过滤、分页）
// host/pattern 支持模糊匹配（LIKE %value%），均为空则不过滤。
func (s *Store) ListInterceptLogs(action, since, host, pattern string, limit, offset int) ([]models.InterceptLog, int, error) {
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
	if host != "" {
		where = append(where, "request_host LIKE ? ESCAPE '\\'")
		args = append(args, "%"+escapeLike(host)+"%")
	}
	if pattern != "" {
		where = append(where, "rule_pattern LIKE ? ESCAPE '\\'")
		args = append(args, "%"+escapeLike(pattern)+"%")
	}

	whereClause := strings.Join(where, " AND ")

	var total int
	countSQL := fmt.Sprintf("SELECT COUNT(*) FROM intercept_logs WHERE %s", whereClause)
	if err := s.readDB().QueryRow(countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count intercept logs: %w", err)
	}

	querySQL := fmt.Sprintf(
		`SELECT id, action, request_url, request_method, request_host, rule_pattern, mode, created_at
		 FROM intercept_logs WHERE %s ORDER BY id DESC LIMIT ? OFFSET ?`, whereClause)
	args = append(args, limit, offset)

	rows, err := s.readDB().Query(querySQL, args...)
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

// intToBool 与 boolToInt 对称：SQLite 以 0/1 存储布尔。
func intToBool(v int) bool { return v != 0 }

// requestListColumns 列表查询的列清单（List 与 ListStarred 共用，与
// scanRequestListItem 的 Scan 顺序严格对应；增列必须两处同步）。
const requestListColumns = "id, method, url, host, status_code, duration_ms, size_bytes, captured_at, is_https, capture_mode, process_pid, process_name, truncated, is_llm"

// scanRequestListItem 扫描一行 RequestListItem（列顺序对应 requestListColumns）。
func scanRequestListItem(rows *sql.Rows) (models.RequestListItem, error) {
	var item models.RequestListItem
	var capturedAt string
	var truncated int
	var isLLM int
	err := rows.Scan(&item.ID, &item.Method, &item.URL, &item.Host,
		&item.StatusCode, &item.DurationMs, &item.SizeBytes, &capturedAt, &item.IsHTTPS,
		&item.CaptureMode, &item.ProcessPID, &item.ProcessName, &truncated, &isLLM)
	item.Truncated = intToBool(truncated)
	item.IsLLM = intToBool(isLLM)
	item.CapturedAt, _ = time.Parse(time.RFC3339, capturedAt)
	return item, err
}

// truncateStr 截断字符串到 maxLen 字节，不会切断多字节 UTF-8 字符：
// 若截断点落在某个 rune 中间，则回退丢弃该不完整 rune，
// 保证写入 SQLite 的 body/headers 始终是合法 UTF-8。
// Truncates to maxLen bytes without splitting a multi-byte UTF-8 rune:
// an incomplete trailing rune is dropped, keeping stored text valid UTF-8.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	i := maxLen - 1
	for i >= 0 && i > maxLen-4 && s[i]&0xC0 == 0x80 {
		i-- // 跳过连续字节，找到最近的 UTF-8 起始字节
	}
	if i < 0 || i <= maxLen-4 {
		return s[:maxLen] // 边界前 4 字节内无起始字节（rune 恰好完整落在边界内）
	}
	b := s[i]
	var runeLen int
	switch {
	case b&0x80 == 0:
		runeLen = 1
	case b&0xE0 == 0xC0:
		runeLen = 2
	case b&0xF0 == 0xE0:
		runeLen = 3
	case b&0xF8 == 0xF0:
		runeLen = 4
	default:
		return s[:maxLen]
	}
	if i+runeLen > maxLen {
		return s[:i] // rune 跨过截断边界，丢弃整个 rune
	}
	return s[:maxLen]
}

// escapeLike 转义 LIKE 模式中的通配符（%、_、\），配合 ESCAPE '\' 使用。
// 防止用户搜索/过滤输入中的通配符被当作模式匹配：
// 例如搜索 "100%" 应只匹配字面 "100%"，而不是所有以 "100" 开头的记录。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
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
	var val string
	err := s.readDB().QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&val)
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
		"INSERT INTO intercept_rules (pattern, method, action, enabled) VALUES (?, ?, ?, ?)",
		rule.Pattern, rule.Method, rule.Action, boolToInt(rule.Enabled),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) ListRules() ([]models.InterceptRule, error) {
	rows, err := s.readDB().Query("SELECT id, pattern, method, action, enabled, created_at FROM intercept_rules ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []models.InterceptRule
	for rows.Next() {
		var r models.InterceptRule
		var ca string
		if err := rows.Scan(&r.ID, &r.Pattern, &r.Method, &r.Action, &r.Enabled, &ca); err != nil {
			slog.Warn("scan failed", "error", err)
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
