package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aurora/httpclient"
	"aurora/internal/accounts"
	"aurora/internal/chatgpt"
	"aurora/internal/config"
	"aurora/middlewares"

	"github.com/gin-gonic/gin"
)

type modelsTestClient struct {
	status  int
	body    string
	err     error
	calls   int
	method  httpclient.HttpMethod
	url     string
	headers httpclient.AuroraHeaders
	cookies []*http.Cookie
}

func (f *modelsTestClient) Request(method httpclient.HttpMethod, url string, headers httpclient.AuroraHeaders, cookies []*http.Cookie, body io.Reader) (*http.Response, error) {
	f.calls++
	f.method, f.url, f.headers, f.cookies = method, url, headers, cookies
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{StatusCode: f.status, Body: io.NopCloser(strings.NewReader(f.body))}, nil
}

func (f *modelsTestClient) SetProxy(string) error             { return nil }
func (f *modelsTestClient) SetCookies(string, []*http.Cookie) {}
func (f *modelsTestClient) GetCookies(string) []*http.Cookie  { return nil }

func modelTestAccount(kind accounts.AccountType, client *modelsTestClient) *accounts.Account {
	acct := accounts.NewAccount("model-test", kind, "upstream-token")
	acct.Status = accounts.StatusActive
	acct.Client = client
	acct.Fingerprint = accounts.BrowserFingerprint{OaiDeviceID: "test-device", UserAgent: "test-agent"}
	return acct
}

func requestModels(h *ModelsHandler, auth, team string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Request.Header.Set("Authorization", auth)
	c.Request.Header.Set("ChatGPT-Account-ID", team)
	h.ListModels(c)
	return w
}

func TestListModelsMapsUpstream(t *testing.T) {
	client := &modelsTestClient{status: 200, body: `{"models":[{"slug":"new-model","title":"New Model"},{"slug":"thinking-model"},{"slug":"new-model"},{"slug":""}]}`}
	acct := modelTestAccount(accounts.TypeFree, client)
	h := NewModelsHandler(accounts.NewPool([]*accounts.Account{acct}), &config.Config{Authorization: "service-key"})
	w := requestModels(h, "Bearer service-key", "workspace-id")
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Object != "list" || len(got.Data) != 2 || got.Data[0].ID != "new-model" || got.Data[1].ID != "thinking-model" {
		t.Fatalf("unexpected model list: %s", w.Body.String())
	}
	for _, model := range got.Data {
		if model.Object != "model" || model.OwnedBy != "openai" || model.Created != 0 {
			t.Fatalf("unexpected model metadata: %+v", model)
		}
	}
	if client.method != httpclient.GET || client.url != chatgpt.BaseURL+"/models" {
		t.Fatalf("upstream request = %s %s", client.method, client.url)
	}
	for key, want := range map[string]string{"Authorization": "Bearer upstream-token", "Chatgpt-Account-Id": "workspace-id", "User-Agent": "test-agent", "Oai-Device-Id": "test-device"} {
		if client.headers[key] != want {
			t.Errorf("header %s = %q, want %q", key, client.headers[key], want)
		}
	}
	if acct.TeamUserID != "" {
		t.Fatal("request team selection leaked into pooled account")
	}
	client.body = `{"models":[{"slug":"updated-model"}]}`
	w = requestModels(h, "Bearer service-key", "")
	if w.Code != 200 || client.calls != 2 || !strings.Contains(w.Body.String(), `"id":"updated-model"`) {
		t.Fatalf("model list did not refresh: %s", w.Body.String())
	}
	if client.headers["Chatgpt-Account-Id"] != "" {
		t.Fatal("workspace header leaked into next request")
	}
}

func TestListModelsErrors(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		err    error
		want   int
	}{
		{"unauthorized", 401, `{"detail":"expired"}`, nil, 401},
		{"forbidden", 403, "<html>blocked</html>", nil, 403},
		{"rate limited", 429, `{"detail":"limited"}`, nil, 429},
		{"server error", 503, "unavailable", nil, 503},
		{"invalid json", 200, "<html>challenge</html>", nil, 502},
		{"missing models", 200, `{}`, nil, 502},
		{"null models", 200, `{"models":null}`, nil, 502},
		{"wrong model type", 200, `{"models":"bad"}`, nil, 502},
		{"transport", 0, "", errors.New("connection failed"), 502},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &modelsTestClient{status: tt.status, body: tt.body, err: tt.err}
			acct := modelTestAccount(accounts.TypeFree, client)
			h := NewModelsHandler(accounts.NewPool([]*accounts.Account{acct}), &config.Config{})
			w := requestModels(h, "", "")
			if w.Code != tt.want || !strings.Contains(w.Body.String(), `"error":`) || strings.Contains(w.Body.String(), `"data":`) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tt.status == 401 && acct.Status != accounts.StatusExpired {
				t.Fatal("expired token was not reported to account pool")
			}
		})
	}
}

func TestListModelsEmptyAndPaidPool(t *testing.T) {
	client := &modelsTestClient{status: 200, body: `{"models":[]}`}
	acct := modelTestAccount(accounts.TypePUID, client)
	h := NewModelsHandler(accounts.NewPool([]*accounts.Account{acct}), &config.Config{})
	w := requestModels(h, "", "")
	if w.Code != 200 || w.Body.String() != `{"object":"list","data":[]}` {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestListModelsRequiresLoggedInAccount(t *testing.T) {
	for _, kind := range []accounts.AccountType{accounts.TypeNoAuth, accounts.TypeFree} {
		client := &modelsTestClient{status: 200, body: `{"models":[]}`}
		acct := modelTestAccount(kind, client)
		if kind == accounts.TypeFree {
			acct.Status = accounts.StatusDisabled
		}
		h := NewModelsHandler(accounts.NewPool([]*accounts.Account{acct}), &config.Config{})
		w := requestModels(h, "", "")
		if w.Code != 401 || client.calls != 0 {
			t.Fatalf("status=%d calls=%d body=%s", w.Code, client.calls, w.Body.String())
		}
	}
	h := NewModelsHandler(accounts.NewPool(nil), &config.Config{EnableExternalToken: false})
	w := requestModels(h, "Bearer eyJexternal", "")
	if w.Code != 401 {
		t.Fatalf("disabled external token status=%d", w.Code)
	}
	w = requestModels(h, "Bearer 00000000-0000-0000-0000-000000000001", "")
	if w.Code != 403 {
		t.Fatalf("anonymous token status=%d", w.Code)
	}
}

func TestListModelsMiddlewareExternalToken(t *testing.T) {
	t.Setenv("Authorization", "service-key")
	pool := accounts.NewPool(nil)
	acct := pool.GetOrCreateTempAccount("eyJexternal-token", "test-agent", "")
	client := &modelsTestClient{status: 200, body: `{"models":[{"slug":"external-model"}]}`}
	acct.Client = client
	h := NewModelsHandler(pool, &config.Config{Authorization: "service-key", EnableExternalToken: true})
	router := gin.New()
	router.GET("/v1/models", middlewares.Authorization, h.ListModels)
	for _, tt := range []struct {
		auth string
		want int
	}{
		{"Bearer wrong-key", 401},
		{"Bearer service-key eyJexternal-token,team-id", 200},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", tt.auth)
		router.ServeHTTP(w, req)
		if w.Code != tt.want {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	if acct.TeamUserID != "" {
		t.Fatal("external workspace selection leaked into cached account")
	}
	if client.calls != 1 || client.headers["Authorization"] != "Bearer eyJexternal-token" || client.headers["Chatgpt-Account-Id"] != "team-id" {
		t.Fatalf("external account request = %+v", client)
	}
}
