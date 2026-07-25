package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"aurora/internal/accounts"
	"aurora/internal/config"

	"github.com/gin-gonic/gin"
)

func TestAdminHandlerAddsListsAndDeletesFreeAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pool := accounts.NewPool(nil)
	h := NewAdminHandler(pool, &config.Config{AdminToken: "admin-secret"})
	dir := t.TempDir()
	h.files = map[string]string{
		"access":  filepath.Join(dir, "access_tokens.txt"),
		"refresh": filepath.Join(dir, "refresh_tokens.txt"),
		"session": filepath.Join(dir, "session_tokens.txt"),
		"free":    filepath.Join(dir, "free_tokens.txt"),
	}
	h.metadataPath = filepath.Join(dir, "account_metadata.json")

	router := gin.New()
	admin := router.Group("/admin/api").Use(h.Authorize)
	admin.GET("/accounts", h.ListAccounts)
	admin.GET("/accounts/export", h.ExportAccounts)
	admin.POST("/accounts", h.AddAccount)
	admin.DELETE("/accounts/:source/:id", h.DeleteAccount)
	admin.POST("/accounts/:source/:id/status", h.UpdateAccountStatus)

	credential := "9ca8b1b6-707f-4c1b-a1e0-d007839ae597"
	body, _ := json.Marshal(addManagedAccountRequest{Source: "free", Token: credential})
	request := httptest.NewRequest(http.MethodPost, "/admin/api/accounts", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer admin-secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, err := pool.Acquire(accounts.TypeNoAuth); err != nil {
		t.Fatalf("added account not available in pool: %v", err)
	}

	request = httptest.NewRequest(http.MethodGet, "/admin/api/accounts", nil)
	request.Header.Set("Authorization", "Bearer admin-secret")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", response.Code, response.Body.String())
	}
	var listed struct {
		Data []managedAccount `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil || len(listed.Data) != 1 {
		t.Fatalf("GET response = %s, err = %v", response.Body.String(), err)
	}
	if listed.Data[0].Token == credential {
		t.Fatal("management API returned an unmasked credential")
	}

	enabled := false
	body, _ = json.Marshal(accountStatusRequest{Enabled: &enabled})
	request = httptest.NewRequest(http.MethodPost, "/admin/api/accounts/free/"+listed.Data[0].ID+"/status", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer admin-secret")
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("disable status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, err := pool.Acquire(accounts.TypeNoAuth); err != accounts.ErrNoAvailable {
		t.Fatalf("disabled account still available: %v", err)
	}

	request = httptest.NewRequest(http.MethodGet, "/admin/api/accounts", nil)
	request.Header.Set("Authorization", "Bearer admin-secret")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil || listed.Data[0].Status != "disabled" {
		t.Fatalf("disabled account listing = %s, err = %v", response.Body.String(), err)
	}

	enabled = true
	body, _ = json.Marshal(accountStatusRequest{Enabled: &enabled})
	request = httptest.NewRequest(http.MethodPost, "/admin/api/accounts/free/"+listed.Data[0].ID+"/status", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer admin-secret")
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("enable status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, err := pool.Acquire(accounts.TypeNoAuth); err != nil {
		t.Fatalf("enabled account not available: %v", err)
	}

	request = httptest.NewRequest(http.MethodGet, "/admin/api/accounts/export", nil)
	request.Header.Set("Authorization", "Bearer admin-secret")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("export status = %d, body = %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("export cache control = %q", response.Header().Get("Cache-Control"))
	}
	if response.Header().Get("Content-Disposition") == "" {
		t.Fatal("export must download as an attachment")
	}
	var exported accountExport
	if err := json.Unmarshal(response.Body.Bytes(), &exported); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if exported.Format != "aurora-account-export" || exported.Version != 1 || len(exported.Accounts) != 1 {
		t.Fatalf("unexpected export: %#v", exported)
	}
	if exported.Accounts[0].Source != "free" || exported.Accounts[0].Token != credential {
		t.Fatalf("export did not preserve credential: %#v", exported.Accounts[0])
	}

	request = httptest.NewRequest(http.MethodDelete, "/admin/api/accounts/free/"+listed.Data[0].ID, nil)
	request.Header.Set("Authorization", "Bearer admin-secret")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, err := pool.Acquire(accounts.TypeNoAuth); err != accounts.ErrNoAvailable {
		t.Fatalf("removed account still available: %v", err)
	}
}

func TestAdminHandlerRequiresConfiguredToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewAdminHandler(accounts.NewPool(nil), &config.Config{})
	router := gin.New()
	router.GET("/accounts", h.Authorize, h.ListAccounts)
	request := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestNormalizeManagedAccountRequest(t *testing.T) {
	request, detected, err := normalizeManagedAccountRequest(addManagedAccountRequest{
		Source: "auto",
		Token:  `{"accessToken":"eyJ.demo.token","sessionToken":"session-cookie"}`,
	})
	if err != nil {
		t.Fatalf("normalize bundle: %v", err)
	}
	if request.Source != "session" || request.Token != "session-cookie" || detected != "session" {
		t.Fatalf("bundle normalized to %#v, %q", request, detected)
	}

	request, detected, err = normalizeManagedAccountRequest(addManagedAccountRequest{
		Source: "auto",
		Token:  "9ca8b1b6-707f-4c1b-a1e0-d007839ae597",
	})
	if err != nil || request.Source != "free" || detected != "free" {
		t.Fatalf("UUID normalized to %#v, %q, %v", request, detected, err)
	}

	_, _, err = normalizeManagedAccountRequest(addManagedAccountRequest{Source: "auto", Token: "opaque-token"})
	if err == nil {
		t.Fatal("expected opaque auto-detection to require manual selection")
	}
}

func TestEmailFromCredential(t *testing.T) {
	email := emailFromCredential(`{"user":{"email":"person@example.test"},"accessToken":"not-used"}`)
	if email != "person@example.test" {
		t.Fatalf("email = %q", email)
	}
}

func TestAdminHandlerPreservesEmailFromCredentialBundle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewAdminHandler(accounts.NewPool(nil), &config.Config{AdminToken: "admin-secret"})
	dir := t.TempDir()
	h.files = map[string]string{
		"access":  filepath.Join(dir, "access_tokens.txt"),
		"refresh": filepath.Join(dir, "refresh_tokens.txt"),
		"session": filepath.Join(dir, "session_tokens.txt"),
		"free":    filepath.Join(dir, "free_tokens.txt"),
	}
	h.metadataPath = filepath.Join(dir, "account_metadata.json")
	router := gin.New()
	router.POST("/accounts", h.Authorize, h.AddAccount)

	body := bytes.NewBufferString(`{"source":"access","token":"{\"accessToken\":\"access-token\",\"user\":{\"email\":\"person@example.test\"}}"}`)
	request := httptest.NewRequest(http.MethodPost, "/accounts", body)
	request.Header.Set("Authorization", "Bearer admin-secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, body = %s", response.Code, response.Body.String())
	}
	var created struct {
		Data managedAccount `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.Data.Email != "person@example.test" {
		t.Fatalf("email = %q", created.Data.Email)
	}

	request = httptest.NewRequest(http.MethodPost, "/accounts", bytes.NewBufferString(`{"source":"access","token":"{\"accessToken\":\"access-token\",\"user\":{\"email\":\"updated@example.test\"}}"}`))
	request.Header.Set("Authorization", "Bearer admin-secret")
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("repeat POST status = %d, body = %s", response.Code, response.Body.String())
	}
	var repaired struct {
		Data          managedAccount `json:"data"`
		AlreadyExists bool           `json:"already_exists"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &repaired); err != nil || !repaired.AlreadyExists || repaired.Data.Email != "updated@example.test" {
		t.Fatalf("repair response = %s, err = %v", response.Body.String(), err)
	}
}
