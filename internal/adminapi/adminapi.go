// Package adminapi 后台管理 API（/admin/api/*）：账号池、代理线路、
// 验证码、设置与用量监控。对应 Python 版 app/routes/admin_api.py；
// 错误形态沿用 FastAPI 的 {"detail": ...}。
package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/model"
	"zcode2api/internal/quota"
	"zcode2api/internal/store"
	"zcode2api/internal/util"
)

// Handler 后台管理 HTTP 层。
type Handler struct {
	Store     *store.Store
	Auth      *auth.Service
	Captcha   *captcha.Manager
	Quota     *quota.Service
	StartedAt time.Time
}

// New 创建后台管理处理器（StartedAt 对齐 Python 版模块导入时刻 _STARTED_AT）。
func New(st *store.Store, au *auth.Service, cm *captcha.Manager, qs *quota.Service) *Handler {
	return &Handler{Store: st, Auth: au, Captcha: cm, Quota: qs, StartedAt: time.Now()}
}

// Register 在 mux 上注册全部 /admin/api/* 路由；每个端点先过后台密钥鉴权。
func (h *Handler) Register(mux *http.ServeMux) {
	guard := func(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if e := h.Auth.VerifyAdminKey(r); e != nil {
				writeAuthError(w, e)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /admin/api/verify", guard(h.handleVerify))
	mux.HandleFunc("GET /admin/api/accounts", guard(h.handleListAccounts))
	mux.HandleFunc("POST /admin/api/accounts", guard(h.handleAddAccounts))
	mux.HandleFunc("DELETE /admin/api/accounts", guard(h.handleDeleteAccounts))
	mux.HandleFunc("PUT /admin/api/accounts/{account_id}", guard(h.handleEditAccount))
	mux.HandleFunc("POST /admin/api/accounts/{account_id}/enabled", guard(h.handleSetEnabled))
	mux.HandleFunc("POST /admin/api/accounts/{account_id}/archived", guard(h.handleSetArchived))
	mux.HandleFunc("POST /admin/api/accounts/refresh", guard(h.handleRefreshAll))
	mux.HandleFunc("POST /admin/api/accounts/{account_id}/refresh", guard(h.handleRefreshAccount))
	mux.HandleFunc("POST /admin/api/accounts/{account_id}/reset-stats", guard(h.handleResetStats))
	mux.HandleFunc("GET /admin/api/status", guard(h.handleStatus))
	mux.HandleFunc("GET /admin/api/proxies", guard(h.handleListProxies))
	mux.HandleFunc("POST /admin/api/proxies", guard(h.handleAddProxy))
	mux.HandleFunc("PUT /admin/api/proxies/{profile_id}", guard(h.handleUpdateProxy))
	mux.HandleFunc("DELETE /admin/api/proxies/{profile_id}", guard(h.handleDeleteProxy))
	mux.HandleFunc("POST /admin/api/proxies/test-current", guard(h.handleTestCurrentProxy))
	mux.HandleFunc("POST /admin/api/proxies/{profile_id}/test", guard(h.handleTestProxy))
	mux.HandleFunc("POST /admin/api/proxies/assign", guard(h.handleAssignProxy))
	mux.HandleFunc("GET /admin/api/monitor", guard(h.handleMonitor))
	mux.HandleFunc("GET /admin/api/usage", guard(h.handleUsage))
	mux.HandleFunc("GET /admin/api/captcha/config", guard(h.handleCaptchaConfig))
	mux.HandleFunc("POST /admin/api/captcha/submit", guard(h.handleCaptchaSubmit))
	mux.HandleFunc("POST /admin/api/login/start", guard(h.handleLoginStart))
	mux.HandleFunc("GET /admin/api/claim/preview", guard(h.handleClaimPreview))
	mux.HandleFunc("POST /admin/api/claim", guard(h.handleClaim))
	mux.HandleFunc("POST /admin/api/login/complete/{flow_id}", guard(h.handleLoginComplete))
	mux.HandleFunc("GET /admin/api/settings", guard(h.handleGetSettings))
	mux.HandleFunc("PUT /admin/api/settings", guard(h.handleUpdateSettings))
	mux.HandleFunc("GET /admin/api/export", guard(h.handleExport))
	mux.HandleFunc("POST /admin/api/import", guard(h.handleImport))
}

// apiError 对应 FastAPI 的 HTTPException(status, detail)。
type apiError struct {
	status  int
	message string
}

func errBadRequest(msg string) *apiError { return &apiError{http.StatusBadRequest, msg} }

func errNotFound(msg string) *apiError { return &apiError{http.StatusNotFound, msg} }

// writeJSON 与 Python JSONResponse 对齐：紧凑序列化、不转义 HTML、无尾部换行。
func writeJSON(w http.ResponseWriter, status int, body any) {
	data, err := util.MarshalJSON(body)
	if err != nil {
		http.Error(w, `{"detail":"响应序列化失败"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func writeAPIError(w http.ResponseWriter, e *apiError) {
	writeJSON(w, e.status, map[string]any{"detail": e.message})
}

// writeAuthError 鉴权错误返回 FastAPI 的 {"detail": ...} 形态（与 Python 版一致）。
func writeAuthError(w http.ResponseWriter, e *auth.AuthError) {
	writeJSON(w, e.Status, map[string]any{"detail": e.Message})
}

func writeError500(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
}

// marshalJSON 与 Python json.dumps(ensure_ascii=False) 对齐：不转义 HTML 字符。


// decodeBody 解析 JSON 请求体；非法 JSON 一律 400
// （FastAPI 为 422，仅错误码差异，detail 形态一致）。
func decodeBody(r *http.Request) (map[string]any, *apiError) {
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		return nil, errBadRequest("请求体不是合法 JSON")
	}
	return payload, nil
}

// truthy 对应 Python 的真值判定：None/空串/0/空容器为假。
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// firstTruthy 对应 Python 的 or 链：返回第一个真值，全假返回 nil。
func firstTruthy(values ...any) any {
	for _, v := range values {
		if truthy(v) {
			return v
		}
	}
	return nil
}

// strOf 对应 Python str(v or "")：空值链后转字符串；JSON 数值用十进制
// 格式化，避免大整数落入科学计数法（对齐 Python str(int)）。
func strOf(v any) string {
	if !truthy(v) {
		return ""
	}
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// accountSnapshot 对应 Python _account_snapshot：脱敏视图 + 概览统计。
func (h *Handler) accountSnapshot() ([]map[string]any, map[string]any) {
	now := time.Now()
	views := []map[string]any{}
	var active, exhausted, cooling, invalid, disabled, archivedCount int
	var calls, fail, tokensIn, tokensOut, tokensCache int
	for _, a := range h.Store.ListAccounts("") {
		view := a.PublicView(now)
		views = append(views, view)
		if a.ArchivedAt != nil {
			archivedCount++ // 已归档账号不计入统计（前端在归档区单独展示）
			continue
		}
		switch view["status"] {
		case model.StatusActive:
			active++
		case model.StatusExhausted:
			exhausted++
		case model.StatusCooling:
			cooling++
		case model.StatusInvalid:
			invalid++
		case model.StatusDisabled:
			disabled++
		}
		calls += a.UseCount
		fail += a.FailCount
		tokensIn += a.TotalInputTokens
		tokensOut += a.TotalOutputTokens
		tokensCache += a.TotalCacheCreationTokens + a.TotalCacheReadTokens
	}
	return views, map[string]any{
		"total":        len(views) - archivedCount,
		"active":       active,
		"exhausted":    exhausted,
		"cooling":      cooling,
		"invalid":      invalid,
		"disabled":     disabled,
		"calls":        calls,
		"fail":         fail,
		"tokens_in":    tokensIn,
		"tokens_out":   tokensOut,
		"tokens_cache": tokensCache,
	}
}

// nowFloat 对应 Python time.time()。
func nowFloat() float64 { return float64(time.Now().UnixNano()) / 1e9 }
