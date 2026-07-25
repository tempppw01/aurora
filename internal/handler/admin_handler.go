package handler

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

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
	ID     string `json:"id"`
	Source string `json:"source"`
	Token  string `json:"token"`
	TeamID string `json:"team_id,omitempty"`
}

type addManagedAccountRequest struct {
	Source string `json:"source"`
	Token  string `json:"token"`
	TeamID string `json:"team_id"`
}

// AdminHandler exposes the local account files through a deliberately
// separate, token-protected management API.
type AdminHandler struct {
	pool  *accounts.Pool
	cfg   *config.Config
	files map[string]string
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
	for source, path := range h.files {
		for _, raw := range accounts.LoadTokensFromFile(path) {
			result = append(result, managedAccount{
				ID:     managedAccountID(source, raw.Token),
				Source: source,
				Token:  maskCredential(raw.Token),
				TeamID: raw.TeamID,
			})
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}

func (h *AdminHandler) AddAccount(c *gin.Context) {
	var req addManagedAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, errors.New("request must be proper JSON"))
		return
	}
	req.Source = strings.ToLower(strings.TrimSpace(req.Source))
	req.Token = strings.TrimSpace(req.Token)
	req.TeamID = strings.TrimSpace(req.TeamID)
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
	h.pool.AddAccount(acct)
	c.JSON(http.StatusCreated, gin.H{
		"data":   managedAccount{ID: managedAccountID(req.Source, req.Token), Source: req.Source, Token: maskCredential(req.Token), TeamID: req.TeamID},
		"status": state,
	})
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
	h.pool.RemoveAccountByCredential(removedToken)
	c.Status(http.StatusNoContent)
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
