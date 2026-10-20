package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"aurora/internal/accounts"
	"aurora/internal/config"

	"github.com/gin-gonic/gin"
)

func TestListModelsIncludesImageGenerationModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/v1/models", NewModelsHandler(accounts.NewPool(nil), &config.Config{}).ListModels)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode models response: %v", err)
	}
	ids := make(map[string]bool, len(payload.Data))
	for _, model := range payload.Data {
		ids[model.ID] = true
	}
	for _, want := range []string{"gpt-image-2.5", "gpt-6", "gpt-6-thinking"} {
		if !ids[want] {
			t.Fatalf("%s missing from models response: %#v", want, payload.Data)
		}
	}
}
