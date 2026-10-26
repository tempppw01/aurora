package handler

import (
	"errors"
	"net/http"
	"strings"

	"aurora/internal/accounts"
	"aurora/internal/authresolver"
	"aurora/internal/chatgpt"
	"aurora/internal/config"
	officialtypes "aurora/typings/official"

	"github.com/gin-gonic/gin"
)

type ModelsHandler struct {
	accountPool *accounts.Pool
	cfg         *config.Config
}

func NewModelsHandler(pool *accounts.Pool, cfg *config.Config) *ModelsHandler {
	return &ModelsHandler{accountPool: pool, cfg: cfg}
}

// ListModels 将 ChatGPT 网页模型 slug 映射为 OpenAI 模型 ID。
func (h *ModelsHandler) ListModels(c *gin.Context) {
	account, status, err := h.resolveAccount(c)
	if err != nil {
		respondError(c, status, err)
		return
	}

	// 工作区覆盖仅作用于本次请求，不修改池内共享账号。
	requestAccount := *account
	if parts := strings.SplitN(c.GetHeader("Authorization"), ",", 2); len(parts) == 2 {
		requestAccount.TeamUserID = strings.TrimSpace(parts[1])
	}
	for _, header := range []string{"ChatGPT-Account-ID", "Team-Account-ID", "X-ChatGPT-Account-ID"} {
		if value := strings.TrimSpace(c.GetHeader(header)); value != "" {
			requestAccount.TeamUserID = value
			break
		}
	}
	models, status, err := chatgpt.ListModels(account.Client, &requestAccount)
	if err != nil {
		if status == http.StatusUnauthorized {
			h.accountPool.ReportFailure(account)
		}
		respondError(c, status, err)
		return
	}

	result := officialtypes.ModelsResponse{Object: "list", Data: make([]officialtypes.Model, 0, len(models))}
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.Slug)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		result.Data = append(result.Data, officialtypes.Model{
			ID: id, Object: "model", OwnedBy: "openai",
			// 网页接口未提供创建时间，使用 0 表示未知。
			Created: 0,
		})
	}
	c.JSON(http.StatusOK, result)
}

func (h *ModelsHandler) resolveAccount(c *gin.Context) (*accounts.Account, int, error) {
	resolver := authresolver.AccessTokenResolver{CustomerKey: h.cfg.Authorization}
	token, _, err := resolver.ResolveAccessToken(c)
	if err != nil {
		return nil, http.StatusUnauthorized, err
	}
	if strings.HasPrefix(token, "eyJ") {
		if !h.cfg.EnableExternalToken {
			return nil, http.StatusUnauthorized, errors.New("external access token disabled (set ENABLE_EXTERNAL_TOKEN=true)")
		}
		proxyURL := h.cfg.ProxyURL
		if proxyURL == "" {
			proxyURL = h.cfg.HTTPProxy
		}
		return h.accountPool.GetOrCreateTempAccount(token, c.GetHeader("User-Agent"), proxyURL), http.StatusOK, nil
	}
	if token != "" {
		return resolveAccount(c, h.accountPool, h.cfg, true)
	}
	// /backend-api/models 需要登录态，匿名 UUID 不参与模型查询。
	for _, kind := range []accounts.AccountType{accounts.TypeFree, accounts.TypePUID} {
		if account, err := h.accountPool.Acquire(kind); err == nil {
			return account, http.StatusOK, nil
		}
	}
	return nil, http.StatusUnauthorized, ErrNoAvailable
}
