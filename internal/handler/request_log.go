package handler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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

// RequestLogStore keeps a bounded, persistent recent-request history.
type RequestLogStore struct {
	mu      sync.RWMutex
	path    string
	max     int
	entries []requestLogEntry
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
}

func (s *RequestLogStore) append(entry requestLogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
	if len(s.entries) > s.max {
		s.entries = append([]requestLogEntry(nil), s.entries[len(s.entries)-s.max:]...)
	}
	s.saveLocked()
}

func (s *RequestLogStore) saveLocked() {
	if s.path == "" {
		return
	}
	data, err := json.Marshal(s.entries)
	if err != nil {
		return
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".aurora-request-logs-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmpName, s.path)
}

func (s *RequestLogStore) recent(limit int) ([]requestLogEntry, requestLogSummary) {
	s.mu.RLock()
	entries := append([]requestLogEntry(nil), s.entries...)
	s.mu.RUnlock()

	summary := requestLogSummary{Total: len(entries)}
	for _, entry := range entries {
		if entry.Success {
			summary.Success++
		}
	}
	summary.Failed = summary.Total - summary.Success
	if summary.Total > 0 {
		summary.SuccessRate = float64(summary.Success) * 100 / float64(summary.Total)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].CreatedAt > entries[j].CreatedAt })
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, summary
}

func (s *RequestLogStore) page(page, pageSize int) ([]requestLogEntry, requestLogSummary, requestLogPage) {
	if pageSize <= 0 {
		pageSize = 25
	}
	if page <= 0 {
		page = 1
	}
	entries, summary := s.recent(0)
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
		}
	}
	h.requestLogs.append(entry)
}
