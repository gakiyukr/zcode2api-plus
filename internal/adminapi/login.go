// OAuth 登录端点（/admin/api/login/*）：对应 Python 版 admin_api.py 的
// login_start / login_complete 与 _save_oauth_account 收尾链。
package adminapi

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/oauth"
	"zcode2api/internal/web"
)

// loginFlowTTL 登录会话有效期（对齐 Python _LOGIN_TTL_SECONDS = 600）。
const loginFlowTTL = 600 * time.Second

var (
	loginFlowsMu sync.Mutex
	loginFlows   = map[string]*oauth.Flow{}
)

// cleanupLoginFlows 清扫过期会话。
func cleanupLoginFlows() {
	loginFlowsMu.Lock()
	defer loginFlowsMu.Unlock()
	cutoff := time.Now().Add(-loginFlowTTL)
	for id, flow := range loginFlows {
		if flow.CreatedAt.Before(cutoff) {
			delete(loginFlows, id)
		}
	}
}

// errUpstream 对齐 Python HTTPException(502)（上游初始化/兑换失败）。
func errUpstream(msg string) *apiError { return &apiError{http.StatusBadGateway, msg} }

func (h *Handler) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	cleanupLoginFlows()
	flow := oauth.NewFlow()
	flowID, authorizeURL, err := flow.Init()
	if err != nil {
		writeAPIError(w, errUpstream(fmt.Sprintf("登录初始化失败: %v", err)))
		return
	}
	loginFlowsMu.Lock()
	loginFlows[flowID] = flow
	loginFlowsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"flow_id": flowID, "authorize_url": authorizeURL})
}

func (h *Handler) handleLoginComplete(w http.ResponseWriter, r *http.Request) {
	cleanupLoginFlows()
	flowID := r.PathValue("flow_id")
	loginFlowsMu.Lock()
	flow := loginFlows[flowID]
	loginFlowsMu.Unlock()
	if flow == nil {
		writeAPIError(w, errNotFound("登录会话不存在或已过期"))
		return
	}

	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	callbackURL := strings.TrimSpace(strOf(payload["callback_url"]))
	code, state, oauthErr, err := oauth.ParseCallbackURL(callbackURL)
	if err != nil {
		writeAPIError(w, errBadRequest(err.Error()))
		return
	}
	if err == nil && oauthErr != "" {
		writeAPIError(w, errBadRequest(fmt.Sprintf("Z.AI 拒绝授权: %s", oauthErr)))
		return
	}
	if !flow.MatchesState(state) {
		writeAPIError(w, errBadRequest("回调地址与当前登录会话不匹配"))
		return
	}

	result, err := flow.ExchangeCode(code, state)
	if err != nil {
		writeAPIError(w, errBadRequest(err.Error()))
		return
	}
	account, apiErr := h.saveOAuthAccount(result)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	loginFlowsMu.Lock()
	delete(loginFlows, flowID)
	loginFlowsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "account": account.PublicView(time.Now())})
}

// saveOAuthAccount 落库登录凭证：JWT 入池（邮箱命名）→ 兑换 API Key 回填同账号
// → 刷新额度。兑换/刷新失败不影响 JWT 已入池（对齐 Python _save_oauth_account）。
func (h *Handler) saveOAuthAccount(result *oauth.ExchangeResult) (*model.Account, *apiError) {
	email := ""
	if result.Email != nil {
		email = strings.TrimSpace(*result.Email)
	}
	name := email
	if name == "" {
		name = "oauth-login"
	}
	account, err := h.Store.AddAccount(model.ProviderZai, name, result.Token)
	if err != nil {
		return nil, errUpstream(fmt.Sprintf("凭证入池失败: %v", err))
	}
	if email != "" {
		if err := h.Store.Update(account.Provider, account.ID, func(a *model.Account) {
			a.Email = &email
			if a.Name == "oauth-login" {
				a.Name = email
			}
		}); err != nil {
			return nil, errUpstream(fmt.Sprintf("账号信息落库失败: %v", err))
		}
	}
	if result.AccessToken != "" {
		if apiKey, err := oauth.ExchangeAPIKey(result.AccessToken); err == nil && apiKey != "" {
			if err := h.Store.Update(account.Provider, account.ID, func(a *model.Account) {
				a.APIKey = &apiKey
			}); err != nil {
				web.Warn("adminapi", fmt.Sprintf("API Key 落库失败: %v", err))
			}
		} else if err != nil {
			web.Warn("adminapi", fmt.Sprintf("兑换 API Key 失败: %v", err))
		}
	}
	if account.Mode == "jwt" {
		h.Quota.RefreshAccounts([]*model.Account{account})
		// 授权完成即激活 + 自动领取（入池即吃满活动；对齐 Python _save_oauth_account）
		h.scheduleAutoClaim(account)
	}
	// 上面的 Update 改的是 Store 内部对象，account 仍是 AddAccount 时的副本；
	// 重新取快照，避免调用方渲染出 email/name 尚未写入的旧值。
	if fresh := h.Store.Find(account.Provider, account.ID); fresh != nil {
		account = fresh
	}
	return account, nil
}
