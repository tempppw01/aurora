package chatgpt

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"aurora/httpclient"
	"aurora/internal/accounts"
)

// Model 是上游 /backend-api/models 返回的原始条目。
// Slug 是网页端实际接受的模型标识，ID 作为 Slug 缺失时的兜底。
type Model struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

// ListModels 获取当前登录账号可见的网页模型。
// 需要登录态账号（free / paid），匿名设备账号拿不到该接口。
func ListModels(client httpclient.AuroraHttpClient, account *accounts.Account) ([]Model, int, error) {
	if account == nil || account.Type == accounts.TypeNoAuth || account.Token == "" {
		return nil, http.StatusForbidden, errors.New("model listing requires a logged-in ChatGPT account")
	}
	if client == nil {
		return nil, http.StatusInternalServerError, errors.New("account HTTP client is not initialized")
	}

	headers := conversationHeaders(account, nil, "application/json", "/backend-api/models", "", "")
	cookies := append([]*http.Cookie{}, BasicCookies...)
	if deviceID := account.Fingerprint.OaiDeviceID; deviceID != "" {
		cookies = append(cookies, &http.Cookie{Name: "oai-did", Value: deviceID, Path: "/"})
	}
	response, err := client.Request(httpclient.GET, strings.TrimRight(BaseURL, "/")+"/models", headers, cookies, nil)
	if err != nil {
		return nil, http.StatusBadGateway, errors.New("ChatGPT models request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		status := response.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		return nil, status, fmt.Errorf("ChatGPT models request failed (HTTP %d)", response.StatusCode)
	}

	const maxModelsResponseSize = 4 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxModelsResponseSize+1))
	if err != nil || len(body) > maxModelsResponseSize {
		return nil, http.StatusBadGateway, errors.New("failed to read ChatGPT models response")
	}
	var result struct {
		Models []Model `json:"models"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Models == nil {
		return nil, http.StatusBadGateway, errors.New("invalid ChatGPT models response: expected models array")
	}
	return result.Models, http.StatusOK, nil
}
