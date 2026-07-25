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

// AdminHandler exposes the local account files through a deliberately
// separate, token-protected management API.
type AdminHandler struct {
	pool         *accounts.Pool
	cfg          *config.Config
	files        map[string]string
	metadataPath string
}

func NewAdminHandler(pool *accounts.Pool, cfg *config.Config) *AdminHandler {
	return &AdminHandler{
		pool: pool,
		cfg:  cfg,
		files: map[string]string{
			"access":  "access_tokens.txt",
			"refresh": "refresh_tokens.txt",
			"session": "session_tokens.txt",
			"free":    "free_tokens.txt",
		},
		metadataPath: "account_metadata.json",
	}
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
			respondError(c, http.StatusConflict, errors.New("this token already exists"))
			return
		}
	}

	acct, state, err := h.newManagedAccount(req)
	if err != nil {
		respondError(c, http.StatusBadRequest, err)
		return
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
