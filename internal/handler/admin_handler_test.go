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

	router := gin.New()
	admin := router.Group("/admin/api").Use(h.Authorize)
	admin.GET("/accounts", h.ListAccounts)
	admin.POST("/accounts", h.AddAccount)
	admin.DELETE("/accounts/:source/:id", h.DeleteAccount)

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
