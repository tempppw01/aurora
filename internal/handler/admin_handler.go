package handler

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"aurora/internal/accounts"
	"aurora/internal/chatgpt"
	"aurora/internal/config"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

//go:embed admin.html
var adminPage []byte

var accountFileMu sync.Mutex

type managedAccount struct {
	ID         string `json:"id"`
	Source     string `json:"source"`
	Token      string `json:"token"`
	TeamID     string `json:"team_id,omitempty"`
	Email      string `json:"email,omitempty"`
	ImportedAt string `json:"imported_at,omitempty"`
	Status     string `json:"status,omitempty"`
	Health     string `json:"health,omitempty"`
	CheckedAt  string `json:"checked_at,omitempty"`
}

type addManagedAccountRequest struct {
	Source string `json:"source"`
	Token  string `json:"token"`
	TeamID string `json:"team_id"`
}

type credentialBundle struct {
	AccessToken  string `json:"accessToken"`
	SessionToken string `json:"sessionToken"`
	User         struct {
		Email string `json:"email"`
	} `json:"user"`
	Account struct {
		Email string `json:"email"`
	} `json:"account"`
}

type accountMetadata struct {
	Email      string `json:"email,omitempty"`
	ImportedAt string `json:"imported_at"`
	Status     string `json:"status"`
	Health     string `json:"health,omitempty"`
	CheckedAt  string `json:"checked_at,omitempty"`
}

type apiKeyRequest struct {
	APIKey string `json:"api_key"`
}

type accountStatusRequest struct {
	Enabled *bool `json:"enabled"`
}

type schedulingRequest struct {
	Mode            string `json:"mode"`
	PreferredSource string `json:"preferred_source"`
	PreferredID     string `json:"preferred_id"`
}

// accountExport is a portable, intentionally credential-bearing backup. It is
// exposed only by the separately authorized management API and must never be
// returned from the normal account listing endpoint.
type accountExport struct {
	Format     string                   `json:"format"`
	Version    int                      `json:"version"`
	ExportedAt string                   `json:"exported_at"`
	Accounts   []exportedManagedAccount `json:"accounts"`
}

type exportedManagedAccount struct {
	Source string `json:"source"`
	Token  string `json:"token"`
	TeamID string `json:"team_id,omitempty"`
}

// AdminHandler exposes the local account files through a deliberately
// separate, token-protected management API.
type AdminHandler struct {
	pool         *accounts.Pool
	cfg          *config.Config
	files        map[string]string
	metadataPath string
	requestLogs  *RequestLogStore
}

func NewAdminHandler(pool *accounts.Pool, cfg *config.Config) *AdminHandler {
	handler := &AdminHandler{
		pool: pool,
		cfg:  cfg,
		files: map[string]string{
			"access":  "access_tokens.txt",
			"refresh": "refresh_tokens.txt",
			"session": "session_tokens.txt",
			"free":    "free_tokens.txt",
		},
		metadataPath: "account_metadata.json",
		requestLogs:  NewRequestLogStore("request_logs.json", 500),
	}
	handler.applyScheduling(config.LoadScheduling())
	return handler
}

func (h *AdminHandler) Page(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", adminPage)
}

// Authorize is intentionally stricter than the API middleware: account
// management is unavailable until an explicit ADMIN_TOKEN or Authorization
// value is configured.
func (h *AdminHandler) Authorize(c *gin.Context) {
	if h.cfg == nil || h.cfg.AdminToken == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account management requires ADMIN_TOKEN or Authorization"})
		c.Abort()
		return
	}
	provided := strings.TrimSpace(c.GetHeader("Authorization"))
	if len(provided) >= len("Bearer ") && strings.EqualFold(provided[:len("Bearer ")], "Bearer ") {
		provided = strings.TrimSpace(provided[len("Bearer "):])
	}
	if provided != h.cfg.AdminToken {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		c.Abort()
		return
	}
	c.Next()
}

func (h *AdminHandler) ListAccounts(c *gin.Context) {
	accountFileMu.Lock()
	defer accountFileMu.Unlock()

	var result []managedAccount
	metadata := h.loadMetadata()
	for source, path := range h.files {
		for _, raw := range accounts.LoadTokensFromFile(path) {
			id := managedAccountID(source, raw.Token)
			meta := metadata[id]
			email := meta.Email
			if email == "" {
				email = emailFromCredential(raw.Token)
			}
			result = append(result, managedAccount{
				ID:         id,
				Source:     source,
				Token:      maskCredential(raw.Token),
				TeamID:     raw.TeamID,
				Email:      email,
				ImportedAt: meta.ImportedAt,
				Status:     meta.Status,
				Health:     meta.Health,
				CheckedAt:  meta.CheckedAt,
			})
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}

// ListRequestLogs returns the most recent API requests and a success summary.
// It deliberately never exposes request/response content or credentials.
func (h *AdminHandler) ListRequestLogs(c *gin.Context) {
	page := 1
	if raw := strings.TrimSpace(c.Query("page")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			page = parsed
		}
	}
	pageSize := 25
	if raw := strings.TrimSpace(c.Query("page_size")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			pageSize = parsed
		}
	}
	if pageSize > 100 {
		pageSize = 100
	}
	if h.requestLogs == nil {
		c.JSON(http.StatusOK, gin.H{"data": []requestLogEntry{}, "summary": requestLogSummary{}, "pagination": requestLogPage{Page: 1, PageSize: pageSize}})
		return
	}
	entries, summary, pagination := h.requestLogs.page(page, pageSize)
	c.JSON(http.StatusOK, gin.H{"data": entries, "summary": summary, "pagination": pagination})
}

// requestAccountLabel finds the managed-account email without retaining the
// account token in a request log. Temporary external-token accounts do not
// have managed metadata, so they receive a non-sensitive local identifier.
func (h *AdminHandler) requestAccountLabel(account *accounts.Account) string {
	if account == nil {
		return ""
	}
	accountFileMu.Lock()
	defer accountFileMu.Unlock()
	metadata := h.loadMetadata()
	for source, path := range h.files {
		for _, raw := range accounts.LoadTokensFromFile(path) {
			if raw.Token != account.Token && raw.Token != account.RefreshToken && raw.Token != account.SessionToken {
				continue
			}
			meta := metadata[managedAccountID(source, raw.Token)]
			if meta.Email != "" {
				return meta.Email
			}
			if email := emailFromCredential(raw.Token); email != "" {
				return email
			}
		}
	}
	if account.ID == "" {
		return "临时账号"
	}
	if len(account.ID) > 12 {
		return "账号 " + account.ID[:8]
	}
	return "账号 " + account.ID
}

// ExportAccounts downloads every persisted account credential as a JSON
// backup. The records retain source and Team ID so a restore can retain each
// account's original type. This endpoint deliberately requires
// the same ADMIN_TOKEN protection as every other management action.
func (h *AdminHandler) ExportAccounts(c *gin.Context) {
	accountFileMu.Lock()
	defer accountFileMu.Unlock()

	now := time.Now().UTC()
	backup := accountExport{
		Format:     "aurora-account-export",
		Version:    1,
		ExportedAt: now.Format(time.RFC3339),
		Accounts:   make([]exportedManagedAccount, 0),
	}
	for _, source := range []string{"access", "session", "refresh", "free"} {
		for _, entry := range accounts.LoadTokensFromFile(h.files[source]) {
			backup.Accounts = append(backup.Accounts, exportedManagedAccount{
				Source: source,
				Token:  entry.Token,
				TeamID: entry.TeamID,
			})
		}
	}

	filename := "aurora-accounts-" + now.Format("20060102T150405Z") + ".json"
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.Header("X-Content-Type-Options", "nosniff")
	c.JSON(http.StatusOK, backup)
}

// CheckAccountHealth performs the same Sentinel authentication preparation as
// a request, without creating a conversation or sending user content.
func (h *AdminHandler) CheckAccountHealth(c *gin.Context) {
	source := strings.ToLower(strings.TrimSpace(c.Param("source")))
	id := strings.TrimSpace(c.Param("id"))
	path, ok := h.files[source]
	if !ok || id == "" {
		respondError(c, http.StatusBadRequest, errors.New("invalid account reference"))
		return
	}

	accountFileMu.Lock()
	var credential string
	for _, entry := range accounts.LoadTokensFromFile(path) {
		if managedAccountID(source, entry.Token) == id {
			credential = entry.Token
			break
		}
	}
	accountFileMu.Unlock()
	if credential == "" {
		respondError(c, http.StatusNotFound, errors.New("account not found"))
		return
	}

	acct := h.pool.FindByCredential(credential)
	if acct == nil {
		respondError(c, http.StatusConflict, errors.New("account is not loaded; restart the service and try again"))
		return
	}
	if acct.Client == nil {
		if err := acct.InitClient(); err != nil {
			respondError(c, http.StatusInternalServerError, fmt.Errorf("initialize account client: %w", err))
			return
		}
	}

	checkedAt := time.Now().UTC().Format(time.RFC3339)
	_, status, err := chatgpt.InitSentinel(acct.Client, acct, acct.Proxy, 0)
	alive := err == nil
	health := "healthy"
	message := "Sentinel authentication succeeded"
	if !alive {
		health = "unhealthy"
		message = err.Error()
		if status == http.StatusUnauthorized {
			h.pool.ReportFailure(acct)
		}
	}

	accountFileMu.Lock()
	metadata := h.loadMetadata()
	meta := metadata[id]
	meta.Health = health
	meta.CheckedAt = checkedAt
	metadata[id] = meta
	metadataErr := h.saveMetadata(metadata)
	accountFileMu.Unlock()
	if metadataErr != nil {
		respondError(c, http.StatusInternalServerError, fmt.Errorf("health check finished but result could not be saved: %w", metadataErr))
		return
	}
	c.JSON(http.StatusOK, gin.H{"alive": alive, "status": status, "health": health, "checked_at": checkedAt, "message": message})
}

// UpdateAccountStatus enables or disables a persisted account. Disabled
// accounts remain stored and can still be checked manually, but the pool will
// skip them when allocating work.
func (h *AdminHandler) UpdateAccountStatus(c *gin.Context) {
	source := strings.ToLower(strings.TrimSpace(c.Param("source")))
	id := strings.TrimSpace(c.Param("id"))
	path, ok := h.files[source]
	if !ok || id == "" {
		respondError(c, http.StatusBadRequest, errors.New("invalid account reference"))
		return
	}

	var req accountStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		respondError(c, http.StatusBadRequest, errors.New("enabled must be a boolean"))
		return
	}

	accountFileMu.Lock()
	defer accountFileMu.Unlock()
	credential := ""
	for _, entry := range accounts.LoadTokensFromFile(path) {
		if managedAccountID(source, entry.Token) == id {
			credential = entry.Token
			break
		}
	}
	if credential == "" {
		respondError(c, http.StatusNotFound, errors.New("account not found"))
		return
	}

	status := accounts.StatusDisabled
	if *req.Enabled {
		status = accounts.StatusActive
	}
	previous, found := h.pool.SetStatusByCredential(credential, status)
	if !found {
		respondError(c, http.StatusConflict, errors.New("account is not loaded; restart the service and try again"))
		return
	}

	metadata := h.loadMetadata()
	meta := metadata[id]
	if meta.Email == "" {
		meta.Email = emailFromCredential(credential)
	}
	meta.Status = status.String()
	metadata[id] = meta
	if err := h.saveMetadata(metadata); err != nil {
		h.pool.SetStatusByCredential(credential, previous)
		respondError(c, http.StatusInternalServerError, fmt.Errorf("account status changed in memory but could not be saved: %w", err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": status.String(), "enabled": *req.Enabled})
}

func (h *AdminHandler) AddAccount(c *gin.Context) {
	var req addManagedAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, errors.New("request must be proper JSON"))
		return
	}
	var detectedAs string
	var err error
	email := emailFromCredential(req.Token)
	req, detectedAs, err = normalizeManagedAccountRequest(req)
	if err != nil {
		respondError(c, http.StatusBadRequest, err)
		return
	}
	if _, ok := h.files[req.Source]; !ok || req.Token == "" {
		respondError(c, http.StatusBadRequest, errors.New("source must be access, refresh, session, or free; token is required"))
		return
	}
	if req.Source == "free" {
		if _, err := uuid.Parse(req.Token); err != nil {
			respondError(c, http.StatusBadRequest, errors.New("free account token must be a UUID device ID"))
			return
		}
	}

	accountFileMu.Lock()
	defer accountFileMu.Unlock()
	path := h.files[req.Source]
	for _, existing := range accounts.LoadTokensFromFile(path) {
		if existing.Token == req.Token {
			if email != "" {
				id := managedAccountID(req.Source, req.Token)
				metadata := h.loadMetadata()
				meta := metadata[id]
				meta.Email = email
				metadata[id] = meta
				if err := h.saveMetadata(metadata); err != nil {
					respondError(c, http.StatusInternalServerError, fmt.Errorf("account exists but email metadata could not be saved: %w", err))
					return
				}
				c.JSON(http.StatusOK, gin.H{
					"data":           managedAccount{ID: id, Source: req.Source, Token: maskCredential(req.Token), TeamID: existing.TeamID, Email: email, ImportedAt: meta.ImportedAt, Status: meta.Status},
					"status":         meta.Status,
					"detected_as":    detectedAs,
					"already_exists": true,
				})
				return
			}
			respondError(c, http.StatusConflict, errors.New("this token already exists"))
			return
		}
	}

	acct, state, err := h.newManagedAccount(req)
	if err != nil {
		respondError(c, http.StatusBadRequest, err)
		return
	}
	if email == "" {
		email = emailFromCredential(acct.Token)
	}
	if err := appendCredential(path, req.Token, req.TeamID); err != nil {
		respondError(c, http.StatusInternalServerError, err)
		return
	}
	metadata := h.loadMetadata()
	id := managedAccountID(req.Source, req.Token)
	metadata[id] = accountMetadata{Email: email, ImportedAt: time.Now().UTC().Format(time.RFC3339), Status: state}
	if err := h.saveMetadata(metadata); err != nil {
		respondError(c, http.StatusInternalServerError, fmt.Errorf("account was added but account metadata could not be saved: %w", err))
		return
	}
	h.pool.AddAccount(acct)
	c.JSON(http.StatusCreated, gin.H{
		"data":        managedAccount{ID: id, Source: req.Source, Token: maskCredential(req.Token), TeamID: req.TeamID, Email: email, ImportedAt: metadata[id].ImportedAt, Status: state},
		"status":      state,
		"detected_as": detectedAs,
	})
}

func (h *AdminHandler) GetAPIKey(c *gin.Context) {
	key := config.LoadAuthorization()
	c.JSON(http.StatusOK, gin.H{"configured": key != "", "hint": maskCredential(key)})
}

func (h *AdminHandler) UpdateAPIKey(c *gin.Context) {
	var req apiKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, errors.New("request must be proper JSON"))
		return
	}
	if err := config.SaveAuthorization(req.APIKey); err != nil {
		respondError(c, http.StatusBadRequest, errors.New("API key must not be empty"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"configured": true, "hint": maskCredential(req.APIKey)})
}

func (h *AdminHandler) GetScheduling(c *gin.Context) {
	settings := config.LoadScheduling()
	c.JSON(http.StatusOK, gin.H{
		"mode":             h.pool.SchedulingMode(),
		"preferred_source": settings.PreferredSource,
		"preferred_id":     settings.PreferredID,
	})
}

func (h *AdminHandler) UpdateScheduling(c *gin.Context) {
	var req schedulingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, errors.New("request must be proper JSON"))
		return
	}
	mode, ok := accounts.ParseSchedulingMode(strings.TrimSpace(req.Mode))
	if !ok {
		respondError(c, http.StatusBadRequest, errors.New("unsupported scheduling mode"))
		return
	}

	settings := config.SchedulingSettings{Mode: string(mode)}
	var preferred *accounts.Account
	if mode == accounts.SchedulePreferred {
		settings.PreferredSource = strings.ToLower(strings.TrimSpace(req.PreferredSource))
		settings.PreferredID = strings.TrimSpace(req.PreferredID)
		if settings.PreferredSource == "" || settings.PreferredID == "" {
			respondError(c, http.StatusBadRequest, errors.New("select an account for preferred scheduling"))
			return
		}
		var err error
		preferred, err = h.managedAccount(settings.PreferredSource, settings.PreferredID)
		if err != nil {
			respondError(c, http.StatusBadRequest, err)
			return
		}
		if preferred.Status != accounts.StatusActive {
			respondError(c, http.StatusConflict, errors.New("preferred account must be enabled and active"))
			return
		}
	}
	if err := config.SaveScheduling(settings); err != nil {
		respondError(c, http.StatusInternalServerError, fmt.Errorf("save scheduling settings: %w", err))
		return
	}
	h.pool.SetScheduling(mode, preferred)
	c.JSON(http.StatusOK, gin.H{
		"mode":             mode,
		"preferred_source": settings.PreferredSource,
		"preferred_id":     settings.PreferredID,
	})
}

func (h *AdminHandler) applyScheduling(settings config.SchedulingSettings) {
	mode, ok := accounts.ParseSchedulingMode(settings.Mode)
	if !ok {
		mode = accounts.ScheduleRoundRobin
	}
	var preferred *accounts.Account
	if mode == accounts.SchedulePreferred && settings.PreferredSource != "" && settings.PreferredID != "" {
		preferred, _ = h.managedAccount(settings.PreferredSource, settings.PreferredID)
	}
	h.pool.SetScheduling(mode, preferred)
}

func (h *AdminHandler) managedAccount(source, id string) (*accounts.Account, error) {
	path, ok := h.files[source]
	if !ok || id == "" {
		return nil, errors.New("invalid account reference")
	}
	accountFileMu.Lock()
	defer accountFileMu.Unlock()
	for _, entry := range accounts.LoadTokensFromFile(path) {
		if managedAccountID(source, entry.Token) == id {
			if account := h.pool.FindByCredential(entry.Token); account != nil {
				return account, nil
			}
			return nil, errors.New("account is not loaded; restart the service and try again")
		}
	}
	return nil, errors.New("account not found")
}

// normalizeManagedAccountRequest supports the management page's automatic
// detection while keeping the server authoritative. Full ChatGPT auth exports
// can contain both accessToken and sessionToken; the session token is preferred
// because it can be renewed by the existing background health check.
func normalizeManagedAccountRequest(req addManagedAccountRequest) (addManagedAccountRequest, string, error) {
	req.Source = strings.ToLower(strings.TrimSpace(req.Source))
	req.Token = strings.TrimSpace(req.Token)
	req.TeamID = strings.TrimSpace(req.TeamID)
	if req.Token == "" {
		return req, "", errors.New("token is required")
	}

	if strings.HasPrefix(req.Token, "{") {
		var bundle credentialBundle
		if err := json.Unmarshal([]byte(req.Token), &bundle); err != nil {
			return req, "", errors.New("credential JSON is invalid")
		}
		bundle.AccessToken = strings.TrimSpace(bundle.AccessToken)
		bundle.SessionToken = strings.TrimSpace(bundle.SessionToken)
		if bundle.AccessToken == "" && bundle.SessionToken == "" {
			return req, "", errors.New("credential JSON does not contain accessToken or sessionToken")
		}
		if req.Source == "access" && bundle.AccessToken != "" {
			req.Token = bundle.AccessToken
			return req, "access", nil
		}
		if req.Source == "session" && bundle.SessionToken != "" {
			req.Token = bundle.SessionToken
			return req, "session", nil
		}
		if req.Source != "auto" {
			return req, "", errors.New("credential JSON only supports access or session import")
		}
		if bundle.SessionToken != "" {
			req.Source = "session"
			req.Token = bundle.SessionToken
			return req, "session", nil
		}
		req.Source = "access"
		req.Token = bundle.AccessToken
		return req, "access", nil
	}

	if req.Source != "auto" {
		return req, req.Source, nil
	}
	if _, err := uuid.Parse(req.Token); err == nil {
		req.Source = "free"
		return req, "free", nil
	}
	if strings.HasPrefix(req.Token, "eyJ") && strings.Count(req.Token, ".") == 2 {
		req.Source = "access"
		return req, "access", nil
	}
	return req, "", errors.New("unable to distinguish this opaque token; choose refresh or session manually")
}

func (h *AdminHandler) DeleteAccount(c *gin.Context) {
	source := strings.ToLower(strings.TrimSpace(c.Param("source")))
	id := strings.TrimSpace(c.Param("id"))
	path, ok := h.files[source]
	if !ok || id == "" {
		respondError(c, http.StatusBadRequest, errors.New("invalid account reference"))
		return
	}

	accountFileMu.Lock()
	defer accountFileMu.Unlock()
	entries := accounts.LoadTokensFromFile(path)
	kept := make([]accounts.RawToken, 0, len(entries))
	removedToken := ""
	for _, entry := range entries {
		if managedAccountID(source, entry.Token) == id {
			removedToken = entry.Token
			continue
		}
		kept = append(kept, entry)
	}
	if removedToken == "" {
		respondError(c, http.StatusNotFound, errors.New("account not found"))
		return
	}
	if err := writeCredentials(path, kept); err != nil {
		respondError(c, http.StatusInternalServerError, err)
		return
	}
	metadata := h.loadMetadata()
	delete(metadata, id)
	if err := h.saveMetadata(metadata); err != nil {
		respondError(c, http.StatusInternalServerError, fmt.Errorf("account was removed but account metadata could not be updated: %w", err))
		return
	}
	h.pool.RemoveAccountByCredential(removedToken)
	c.Status(http.StatusNoContent)
}

func (h *AdminHandler) loadMetadata() map[string]accountMetadata {
	metadata := make(map[string]accountMetadata)
	data, err := os.ReadFile(h.metadataPath)
	if err != nil {
		return metadata
	}
	_ = json.Unmarshal(data, &metadata)
	return metadata
}

func (h *AdminHandler) saveMetadata(metadata map[string]accountMetadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(h.metadataPath)
	tmp, err := os.CreateTemp(dir, ".aurora-metadata-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, h.metadataPath)
}

func emailFromCredential(credential string) string {
	var bundle credentialBundle
	if json.Unmarshal([]byte(credential), &bundle) == nil {
		if email := strings.TrimSpace(bundle.User.Email); email != "" {
			return email
		}
		if email := strings.TrimSpace(bundle.Account.Email); email != "" {
			return email
		}
	}
	parts := strings.Split(credential, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]interface{}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	for _, key := range []string{"email", "https://api.openai.com/profile.email"} {
		if email, ok := claims[key].(string); ok {
			return strings.TrimSpace(email)
		}
	}
	return ""
}

func (h *AdminHandler) newManagedAccount(req addManagedAccountRequest) (*accounts.Account, string, error) {
	profiles := accounts.DefaultProfiles
	proxyURL := h.cfg.ProxyURL
	if proxyURL == "" {
		proxyURL = h.cfg.HTTPProxy
	}

	switch req.Source {
	case "access":
		acct := accounts.CreateAccount(req.Token, accounts.TypeFree, profiles)
		acct.TeamUserID = req.TeamID
		acct.ChatGPTAccountID = accounts.ExtractChatGPTAccountID(req.Token)
		acct.Proxy = proxyURL
		acct.Status = accounts.StatusActive
		if err := acct.InitClient(); err != nil {
			return nil, "", fmt.Errorf("initialize account client: %w", err)
		}
		return acct, "active", nil
	case "free":
		acct := accounts.CreateAccount(req.Token, accounts.TypeNoAuth, profiles)
		acct.Proxy = proxyURL
		acct.Status = accounts.StatusActive
		if err := acct.InitClient(); err != nil {
			return nil, "", fmt.Errorf("initialize account client: %w", err)
		}
		return acct, "active", nil
	case "refresh", "session":
		acct := accounts.CreateAccount("", accounts.TypeFree, profiles)
		acct.TeamUserID = req.TeamID
		acct.Proxy = proxyURL
		if err := acct.InitClient(); err != nil {
			return nil, "", fmt.Errorf("initialize account client: %w", err)
		}
		var result interface{}
		var err error
		if req.Source == "refresh" {
			acct.RefreshToken = req.Token
			result, _, err = chatgpt.GETTokenForRefreshToken(acct.Client, req.Token, "")
		} else {
			acct.SessionToken = req.Token
			result, _, err = chatgpt.GETTokenForSessionToken(acct.Client, req.Token, "")
		}
		if err != nil {
			acct.Status = accounts.StatusExpired
			return acct, "stored_pending_renewal", nil
		}
		if accessToken := accessTokenFromExchange(result, req.Source); accessToken != "" {
			acct.Token = accessToken
			acct.ChatGPTAccountID = accounts.ExtractChatGPTAccountID(accessToken)
			acct.Status = accounts.StatusActive
			return acct, "active", nil
		}
		acct.Status = accounts.StatusExpired
		return acct, "stored_pending_renewal", nil
	}
	return nil, "", errors.New("unsupported account source")
}

func accessTokenFromExchange(result interface{}, source string) string {
	if r, ok := result.(interface{ GetAccessToken() string }); ok {
		return r.GetAccessToken()
	}
	data, ok := result.(map[string]interface{})
	if !ok {
		return ""
	}
	key := "access_token"
	if source == "session" {
		key = "accessToken"
	}
	value, _ := data[key].(string)
	return value
}

func appendCredential(path, token, teamID string) error {
	line := token
	if teamID != "" {
		line += ":" + teamID
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(line + "\n")
	return err
}

func writeCredentials(path string, entries []accounts.RawToken) error {
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		line := entry.Token
		if entry.TeamID != "" {
			line += ":" + entry.TeamID
		}
		lines = append(lines, line)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".aurora-accounts-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	contents := strings.Join(lines, "\n")
	if contents != "" {
		contents += "\n"
	}
	if _, err := tmp.WriteString(contents); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func managedAccountID(source, token string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + token))
	return hex.EncodeToString(sum[:16])
}

func maskCredential(token string) string {
	if len(token) <= 12 {
		return "••••••••"
	}
	return token[:6] + "…" + token[len(token)-4:]
}
