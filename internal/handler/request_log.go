package handler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"aurora/internal/accounts"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const requestLogAccountKey = "aurora.request_log.account"
const requestLogFailureKey = "aurora.request_log.failure"

// requestLogEntry intentionally contains operational metadata only. Request
// bodies, response bodies, Authorization headers, and credentials must never
// be added here because this file is persisted for the management console.
type requestLogEntry struct {
	ID          string `json:"id"`
	CreatedAt   string `json:"created_at"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	StatusCode  int    `json:"status_code"`
	Success     bool   `json:"success"`
	DurationMS  int64  `json:"duration_ms"`
	Account     string `json:"account,omitempty"`
	AccountType string `json:"account_type,omitempty"`
	ErrorCode   string `json:"error_code,omitempty"`
}

type requestLogSummary struct {
	Total       int     `json:"total"`
	Success     int     `json:"success"`
	Failed      int     `json:"failed"`
	SuccessRate float64 `json:"success_rate"`
}

type requestLogPage struct {
	Page       int `json:"page"`
	PageSize   int `json:"page_size"`
	TotalPages int `json:"total_pages"`
}

// requestLogFilter contains only non-sensitive fields which are already
// available in the request-log list. It must never be extended with request
// or response content.
type requestLogFilter struct {
	Result string
	Query  string
}

// RequestLogStore keeps a bounded, persistent recent-request history.
type RequestLogStore struct {
	mu      sync.RWMutex
	path    string
	max     int
	entries []requestLogEntry
	// lastAt 记录最后一条日志的时间戳。Windows 上 time.Now() 精度较粗，
	// 连续两次请求可能拿到完全相同的时间戳，导致排序出现并列。
	// 写入时保证时间戳严格递增即可消除这个问题。
	lastAt time.Time
}

func NewRequestLogStore(path string, max int) *RequestLogStore {
	if max <= 0 {
		max = 500
	}
	store := &RequestLogStore{path: path, max: max}
	store.load()
	return store
}

func (s *RequestLogStore) load() {
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var entries []requestLogEntry
	if json.Unmarshal(data, &entries) != nil {
		return
	}
	if len(entries) > s.max {
		entries = entries[len(entries)-s.max:]
	}
	s.entries = entries
	for _, entry := range entries {
		if ts, err := time.Parse(time.RFC3339Nano, entry.CreatedAt); err == nil && ts.After(s.lastAt) {
			s.lastAt = ts
		}
	}
}

func (s *RequestLogStore) append(entry requestLogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry.CreatedAt = s.nextTimestamp(entry.CreatedAt)
	s.entries = append(s.entries, entry)
	if len(s.entries) > s.max {
		s.entries = append([]requestLogEntry(nil), s.entries[len(s.entries)-s.max:]...)
	}
	_ = s.saveLocked()
}

// nextTimestamp 保证写入的时间戳严格递增。若当前时间戳不晚于上一条，
// 则在上一条基础上加 1 纳秒，避免同一纳秒内的多条日志顺序不确定。
func (s *RequestLogStore) nextTimestamp(raw string) string {
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		ts = time.Now().UTC()
	}
	if !ts.After(s.lastAt) {
		ts = s.lastAt.Add(time.Nanosecond)
	}
	s.lastAt = ts
	return ts.UTC().Format(time.RFC3339Nano)
}

func (s *RequestLogStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.Marshal(s.entries)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".aurora-request-logs-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

func (s *RequestLogStore) recent(limit int) ([]requestLogEntry, requestLogSummary) {
	entries, summary := s.filtered(requestLogFilter{})
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, summary
}

func (s *RequestLogStore) filtered(filter requestLogFilter) ([]requestLogEntry, requestLogSummary) {
	s.mu.RLock()
	entries := append([]requestLogEntry(nil), s.entries...)
	s.mu.RUnlock()

	query := strings.ToLower(strings.TrimSpace(filter.Query))
	filtered := make([]requestLogEntry, 0, len(entries))
	for _, entry := range entries {
		if filter.Result == "success" && !entry.Success {
			continue
		}
		if filter.Result == "failed" && entry.Success {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(strings.Join([]string{
			entry.Method,
			entry.Path,
			entry.Account,
			entry.AccountType,
			entry.ErrorCode,
			strconv.Itoa(entry.StatusCode),
		}, " ")), query) {
			continue
		}
		filtered = append(filtered, entry)
	}

	summary := requestLogSummary{Total: len(filtered)}
	for _, entry := range filtered {
		if entry.Success {
			summary.Success++
		}
	}
	summary.Failed = summary.Total - summary.Success
	if summary.Total > 0 {
		summary.SuccessRate = float64(summary.Success) * 100 / float64(summary.Total)
	}

	sort.Slice(filtered, func(i, j int) bool { return requestLogEntryNewer(filtered[i], filtered[j]) })
	return filtered, summary
}

// requestLogEntryNewer 判断 a 是否比 b 更新。
// CreatedAt 是 RFC3339Nano 字符串，末尾的零会被裁掉，直接按字符串比较会出错
// （例如 "…3163816Z" 会被认为比 "…31638161Z" 更新），所以按解析出的时间比较，
// 时间相同时用 ID 兜底保证顺序稳定。
func requestLogEntryNewer(a, b requestLogEntry) bool {
	at, aErr := time.Parse(time.RFC3339Nano, a.CreatedAt)
	bt, bErr := time.Parse(time.RFC3339Nano, b.CreatedAt)
	aOK, bOK := aErr == nil, bErr == nil
	switch {
	case aOK && bOK && !at.Equal(bt):
		return at.After(bt)
	case aOK != bOK:
		return aOK
	case a.CreatedAt != b.CreatedAt:
		return a.CreatedAt > b.CreatedAt
	default:
		return a.ID > b.ID
	}
}

func (s *RequestLogStore) page(page, pageSize int, filter requestLogFilter) ([]requestLogEntry, requestLogSummary, requestLogPage) {
	if pageSize <= 0 {
		pageSize = 25
	}
	if page <= 0 {
		page = 1
	}
	entries, summary := s.filtered(filter)
	paging := requestLogPage{PageSize: pageSize}
	if summary.Total == 0 {
		paging.Page = 1
		return entries, summary, paging
	}
	paging.TotalPages = (summary.Total + pageSize - 1) / pageSize
	if page > paging.TotalPages {
		page = paging.TotalPages
	}
	paging.Page = page
	start := (page - 1) * pageSize
	end := start + pageSize
	if end > len(entries) {
		end = len(entries)
	}
	return entries[start:end], summary, paging
}

// Clear removes every persisted request-log entry. The log file is replaced
// atomically so a restart cannot restore entries after a successful cleanup.
func (s *RequestLogStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = []requestLogEntry{}
	return s.saveLocked()
}

func rememberRequestAccount(c *gin.Context, account *accounts.Account) *accounts.Account {
	if account != nil {
		c.Set(requestLogAccountKey, account)
	}
	return account
}

func rememberRequestFailure(c *gin.Context, code string) {
	if code != "" {
		c.Set(requestLogFailureKey, code)
	}
}

// RequestLogger records only API requests that callers make to Aurora. It is
// installed around authorization so rejected API calls are logged as well.
func (h *AdminHandler) RequestLogger(c *gin.Context) {
	started := time.Now()
	c.Next()

	if h.requestLogs == nil {
		return
	}
	status := c.Writer.Status()
	if status == 0 {
		status = 200
	}
	entry := requestLogEntry{
		ID:         uuid.NewString(),
		CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		Method:     c.Request.Method,
		Path:       c.Request.URL.Path,
		StatusCode: status,
		Success:    status >= 200 && status < 300,
		DurationMS: time.Since(started).Milliseconds(),
	}
	if value, ok := c.Get(requestLogFailureKey); ok {
		if code, ok := value.(string); ok && code != "" {
			entry.Success = false
			entry.ErrorCode = code
		}
	}
	if value, ok := c.Get(requestLogAccountKey); ok {
		if account, ok := value.(*accounts.Account); ok && account != nil {
			entry.AccountType = account.Type.String()
			entry.Account = h.requestAccountLabel(account)
			h.recordRequestAccountIssue(account, entry.StatusCode, entry.ErrorCode)
		}
	}
	h.requestLogs.append(entry)
}
