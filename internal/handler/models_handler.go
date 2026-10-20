package handler

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"aurora/internal/accounts"
	"aurora/internal/authresolver"
	"aurora/internal/chatgpt"
	"aurora/internal/config"
	officialtypes "aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// modelsCacheTTL 上游模型列表的缓存时长。
// new-api 会周期拉取 /v1/models，每次都打上游既慢又容易被风控。
const modelsCacheTTL = 5 * time.Minute

// staticModels 是 aurora 自身保证可用的模型列表（静态兜底）。
// 上游 /backend-api/models 不包含图片模型，所以 gpt-image-2.5 必须常驻，
// 否则 new-api 侧的图片渠道会选不到模型。
var staticModels = []string{
	"auto",
	"gpt-6",
	"gpt-6-instant",
	"gpt-6-thinking",
	"gpt-6-pro",
	"gpt-5-6",
	"gpt-5-6-t-mini",
	"gpt-5-6-thinking",
	"gpt-5-6-pro",
	"gpt-5-5-instant",
	"gpt-5-5-thinking",
	"gpt-5-5-pro",
	"gpt-5",
	"gpt-4o",
	"gpt-4o-mini",
	"o3",
	"o4-mini",
	"o4-mini-high",
	"gpt-image-2.5",
}

type ModelsHandler struct {
	accountPool *accounts.Pool
	cfg         *config.Config

	cacheMu    sync.Mutex
	cached     []string
	cachedAt   time.Time
	cacheValid bool
}

func NewModelsHandler(pool *accounts.Pool, cfg *config.Config) *ModelsHandler {
	return &ModelsHandler{accountPool: pool, cfg: cfg}
}

// ListModels 返回模型列表。
// 上游实时列表优先，这样 gpt-6 这类新模型上线后会自动出现在 new-api 里；
// 上游不可用（没有登录态账号 / 网络异常）时回退到静态列表，
// 保证 new-api 永远拿得到一份可用的模型清单。
func (h *ModelsHandler) ListModels(c *gin.Context) {
	ids := h.modelIDs(c)

	result := officialtypes.ModelsResponse{Object: "list", Data: make([]officialtypes.Model, 0, len(ids))}
	for _, id := range ids {
		result.Data = append(result.Data, officialtypes.Model{
			ID:      id,
			Object:  "model",
			Created: 1685474247,
			OwnedBy: "openai",
		})
	}
	c.JSON(http.StatusOK, result)
}

// modelIDs 组装最终返回的模型 ID：静态基线 + 上游实时模型（去重）。
func (h *ModelsHandler) modelIDs(c *gin.Context) []string {
	seen := make(map[string]bool, len(staticModels)+16)
	ids := make([]string, 0, len(staticModels)+16)
	appendID := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}

	for _, id := range staticModels {
		appendID(id)
	}
	for _, id := range h.upstreamModels(c) {
		appendID(id)
	}

	return ids
}

// upstreamModels 拉取上游实时模型，带 5 分钟缓存。
// 拉取失败时返回 nil，由调用方回退到静态列表。
func (h *ModelsHandler) upstreamModels(c *gin.Context) []string {
	h.cacheMu.Lock()
	if h.cacheValid && time.Since(h.cachedAt) < modelsCacheTTL {
		cached := h.cached
		h.cacheMu.Unlock()
		return cached
	}
	h.cacheMu.Unlock()

	account, err := h.resolveAccount(c)
	if err != nil || account == nil {
		return nil
	}

	models, _, err := chatgpt.ListModels(account.Client, account)
	if err != nil {
		return nil
	}

	ids := make([]string, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.Slug)
		if id == "" {
			id = strings.TrimSpace(model.ID)
		}
		if id != "" {
			ids = append(ids, id)
		}
	}

	h.cacheMu.Lock()
	h.cached = ids
	h.cachedAt = time.Now()
	h.cacheValid = true
	h.cacheMu.Unlock()

	return ids
}

// resolveAccount 解析一个可用于查询上游模型的登录态账号。
// /backend-api/models 只接受登录态账号，匿名设备账号拿不到该接口。
func (h *ModelsHandler) resolveAccount(c *gin.Context) (*accounts.Account, error) {
	resolver := authresolver.AccessTokenResolver{CustomerKey: h.cfg.Authorization}
	token, isCustomerKey, err := resolver.ResolveAccessToken(c)
	if err != nil {
		return nil, err
	}

	// 外部传入的 access_token：临时账号，团队选择只作用于本次请求。
	if !isCustomerKey && strings.HasPrefix(token, "eyJ") {
		if !h.cfg.EnableExternalToken {
			return nil, errors.New("external access token disabled (set ENABLE_EXTERNAL_TOKEN=true)")
		}
		proxyURL := h.cfg.ProxyURL
		if proxyURL == "" {
			proxyURL = h.cfg.HTTPProxy
		}
		account := h.accountPool.GetOrCreateTempAccount(token, c.GetHeader("User-Agent"), proxyURL)
		return withRequestTeamAccount(c, account), nil
	}

	// 外部传入的其它凭证（refresh_token 等）：复用通用解析逻辑换取登录态账号。
	if !isCustomerKey && token != "" {
		account, _, err := resolveAccount(c, h.accountPool, h.cfg, true)
		if err == nil && account != nil && account.Type != accounts.TypeNoAuth {
			return withRequestTeamAccount(c, account), nil
		}
	}

	// 其余情况（全局密钥 / UUID / 刷新令牌）都从账号池取一个登录态账号。
	for _, kind := range []accounts.AccountType{accounts.TypePUID, accounts.TypeFree} {
		account, err := h.accountPool.Acquire(kind)
		if err != nil || account == nil {
			continue
		}
		return withRequestTeamAccount(c, account), nil
	}
	return nil, ErrNoAvailable
}

// withRequestTeamAccount 返回账号的副本并带上本次请求选择的团队，
// 避免把请求级的团队选择写回池内共享账号。
func withRequestTeamAccount(c *gin.Context, account *accounts.Account) *accounts.Account {
	if account == nil {
		return nil
	}
	requestAccount := *account
	teamAccountID := ""
	for _, header := range []string{"ChatGPT-Account-ID", "Chatgpt-Account-Id", "Team-Account-ID", "X-ChatGPT-Account-ID"} {
		if value := strings.TrimSpace(c.GetHeader(header)); value != "" {
			teamAccountID = value
			break
		}
	}
	if teamAccountID != "" {
		requestAccount.TeamUserID = teamAccountID
	}
	return &requestAccount
}
