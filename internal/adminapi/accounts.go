// 账号池、验证码、设置、导入导出端点。
// 对应 Python 版 admin_api.py 的对应段落；M6 OAuth 登录留 stub 接入点。
package adminapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/store"
)

func (h *Handler) handleVerify(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	quotaPool := map[string]int{}
	for _, p := range store.Providers {
		n := 0
		for _, a := range h.Store.ListAccounts(p) {
			if a.IsSelectable(now) {
				n++
			}
		}
		quotaPool[p] = n
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers":              store.Providers,
		"gateway_key_set":        h.Store.GatewayKey() != "",
		"quota_refresh_interval": h.Store.QuotaRefreshInterval(),
		"quota_pool":             quotaPool,
	})
}

func (h *Handler) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, stats := h.accountSnapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":  accounts,
		"stats":     stats,
		"providers": store.Providers,
		"models":    gateway.AvailableModels,
		"proxies":   h.Store.ListProxyProfiles(),
		"ts":        nowFloat(),
	})
}

func (h *Handler) handleAddAccounts(w http.ResponseWriter, r *http.Request) {
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}

	provider := model.ProviderZai
	if v, ok := payload["provider"]; ok {
		provider = strOf(v)
	}
	if !allowedProvider(provider) {
		writeAPIError(w, errBadRequest("不支持的 provider"))
		return
	}
	tokens := parseTokens(payload["tokens"])
	if len(tokens) == 0 {
		writeAPIError(w, errBadRequest("请输入至少一个 Token / API Key"))
		return
	}

	hasProfile := false
	if _, ok := payload["proxy_id"]; ok { // 按键存在判定，与 Python "proxy_id" in payload 一致
		hasProfile = true
	}
	profileID := strOf(firstTruthy(payload["proxy_id"]))
	if profileID != "" && !h.profileExists(profileID) {
		writeAPIError(w, errBadRequest("代理配置不存在"))
		return
	}

	hasProxy := false
	var proxyURL *string
	if v, ok := payload["proxy_url"]; ok && !hasProfile {
		u, err := proxy.NormalizeProxyURL(strOf(v))
		if err != nil {
			writeAPIError(w, errBadRequest(err.Error()))
			return
		}
		hasProxy = true
		proxyURL = u
	}

	added := []string{}
	seen := map[string]bool{}
	for _, tok := range tokens {
		if seen[tok] {
			continue // 去重保序（对齐 dict.fromkeys）
		}
		seen[tok] = true
		name := strOf(firstTruthy(payload["name"]))
		if name == "" {
			name = fmt.Sprintf("%s-%d", provider, len(h.Store.ListAccounts(provider))+1)
		}
		acc, err := h.Store.AddAccount(provider, name, tok)
		if err != nil {
			writeError500(w, err)
			return
		}
		if hasProfile {
			if _, err := h.Store.AssignProxyProfile(acc.ID, profileID); err != nil {
				writeError500(w, err)
				return
			}
		} else if hasProxy {
			if err := h.Store.Update(acc.Provider, acc.ID, func(a *model.Account) {
				a.ProxyURL = proxyURL
			}); err != nil {
				writeError500(w, err)
				return
			}
		}
		added = append(added, acc.ID)
	}
	// 对新增的 jwt 账号立即刷新一次额度（仅 zai；对齐 add_accounts 尾段）
	addedSet := map[string]bool{}
	for _, id := range added {
		addedSet[id] = true
	}
	fresh := []*model.Account{}
	for _, a := range h.Store.ListAccounts(provider) {
		if addedSet[a.ID] && a.Mode == "jwt" {
			fresh = append(fresh, a)
		}
	}
	if len(fresh) > 0 {
		h.Quota.RefreshAccounts(fresh)
		// 入池即自动领取（后台 fire-and-forget；对齐 Python add_accounts 尾段）
		for _, acc := range fresh {
			h.scheduleAutoClaim(acc)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(added), "ids": added})
}

// parseTokens 归一 tokens 字段：字符串按行拆分、数组逐项 strip，过滤空值。
func parseTokens(raw any) []string {
	var out []string
	appendToken := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	switch t := raw.(type) {
	case string:
		for _, line := range strings.Split(t, "\n") {
			appendToken(line)
		}
	case []any:
		for _, item := range t {
			appendToken(strOf(item))
		}
	}
	return out
}

func allowedProvider(p string) bool {
	for _, provider := range store.Providers {
		if provider == p {
			return true
		}
	}
	return false
}

func (h *Handler) profileExists(profileID string) bool {
	for _, p := range h.Store.ListProxyProfiles() {
		if p.ID == profileID {
			return true
		}
	}
	return false
}

// handleDeleteAccounts 请求体直接是账号 ID 字符串数组（对齐 Python Body(list[str])）。
func (h *Handler) handleDeleteAccounts(w http.ResponseWriter, r *http.Request) {
	var ids []string
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		writeAPIError(w, errBadRequest("请求体应为账号 ID 数组"))
		return
	}
	deleted := 0
	for _, aid := range ids {
		acc := h.Store.FindAny(aid)
		if acc == nil {
			continue
		}
		if ok, err := h.Store.RemoveAccount(acc.Provider, aid); err == nil && ok {
			deleted++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

func (h *Handler) handleEditAccount(w http.ResponseWriter, r *http.Request) {
	acc := h.Store.FindAny(r.PathValue("account_id"))
	if acc == nil {
		writeAPIError(w, errNotFound("账号不存在"))
		return
	}
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}

	hasProfile := false
	if _, ok := payload["proxy_id"]; ok {
		hasProfile = true
	}
	profileID := strOf(firstTruthy(payload["proxy_id"]))
	if profileID != "" && !h.profileExists(profileID) {
		writeAPIError(w, errBadRequest("代理配置不存在"))
		return
	}

	// 在快照上完成校验与组装，只记录「本次要改哪些字段」；
	// 落库时在 Store 锁内重新取当前对象，逐字段套用。
	// 不能整体覆盖：快照可能已陈旧（并发请求刚把账号标成 invalid/cooling），
	// 整体写回会把这些状态改动冲掉。
	var (
		setName           *string
		setSecret         *model.Account // 仅借其 Mode/JWTToken/APIKey 三字段
		setProxy          bool           // proxy_url 字段是否出现（值可为 nil = 清空）
		proxyURL          *string
		clearProxyID      bool
		setDisabledModels []string
	)
	if v, ok := payload["name"]; ok && truthy(v) {
		name := strings.TrimSpace(strOf(v))
		setName = &name
	}
	if secret := firstTruthy(payload["token"], payload["secret"]); truthy(secret) {
		s := strings.TrimSpace(strOf(secret))
		cred := &model.Account{}
		if strings.Count(s, ".") == 2 && acc.Provider == model.ProviderZai {
			cred.Mode = "jwt"
			cred.JWTToken = &s
		} else {
			cred.Mode = "apiKey"
			cred.APIKey = &s
		}
		setSecret = cred
	}
	if v, ok := payload["proxy_url"]; ok && !hasProfile {
		// 用 = 而非 := 赋值到外层 proxyURL：:= 会新建内层变量，
		// 闭包捕获的仍是外层那个，写入就变成了空操作。
		var err error
		proxyURL, err = proxy.NormalizeProxyURL(strOf(v))
		if err != nil {
			writeAPIError(w, errBadRequest(err.Error()))
			return
		}
		if !sameStringPtr(proxyURL, acc.ProxyURL) {
			clearProxyID = true // 改为手工代理时解除线路指派
		}
		// NormalizeProxyURL("") 返回 nil 表示「清空代理」，与「字段未提供」
		// 是两种语义，必须用 setProxy 区分，不能只看值是否为 nil。
		setProxy = true
	}
	if v, ok := payload["disabled_models"]; ok {
		models, apiErr := parseDisabledModels(v)
		if apiErr != nil {
			writeAPIError(w, apiErr)
			return
		}
		setDisabledModels = models
	}

	// 先指派线路再套用其他字段：AssignProxyProfile 自带锁，不能放进 Update
	// 闭包（会死锁），而它可能因 profile 已被并发删除而失败。放在前面，失败时
	// 其余字段尚未落库，避免「回 500 但 name/secret 已生效」的半套用。
	if hasProfile {
		if _, err := h.Store.AssignProxyProfile(acc.ID, profileID); err != nil {
			writeError500(w, err)
			return
		}
	}

	if err := h.Store.Update(acc.Provider, acc.ID, func(a *model.Account) {
		if setName != nil {
			a.Name = *setName
		}
		if setSecret != nil {
			a.Mode = setSecret.Mode
			a.JWTToken = setSecret.JWTToken
			a.APIKey = setSecret.APIKey
			a.Status = model.StatusActive
			a.LastError = nil
		}
		if setProxy {
			a.ProxyURL = proxyURL
			if clearProxyID {
				a.ProxyID = nil
			}
		}
		if setDisabledModels != nil {
			a.SetDisabledModels(setDisabledModels)
		}
	}); err != nil {
		writeError500(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// parseDisabledModels 校验停用模型列表：数组、不超过 64 项、每项为 ≤100 字符的字符串。
func parseDisabledModels(raw any) ([]string, *apiError) {
	list, ok := raw.([]any)
	if !ok {
		return nil, errBadRequest("停用模型必須是陣列")
	}
	if len(list) > 64 {
		return nil, errBadRequest("停用模型數量過多")
	}
	models := make([]string, 0, len(list))
	for _, item := range list {
		s, isStr := item.(string)
		if !isStr || len(strings.TrimSpace(s)) > 100 {
			return nil, errBadRequest("停用模型格式無效")
		}
		models = append(models, s)
	}
	return models, nil
}

func sameStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (h *Handler) handleSetEnabled(w http.ResponseWriter, r *http.Request) {
	acc := h.Store.FindAny(r.PathValue("account_id"))
	if acc == nil {
		writeAPIError(w, errNotFound("账号不存在"))
		return
	}
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	enabled := true
	if v, ok := payload["enabled"]; ok {
		enabled = truthy(v) // 对齐 bool(payload.get("enabled", True))：null 视为假
	}
	if ok, err := h.Store.SetEnabled(acc.Provider, acc.ID, enabled); err != nil {
		writeError500(w, err)
		return
	} else if !ok {
		writeAPIError(w, errNotFound("账号不存在"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleSetArchived(w http.ResponseWriter, r *http.Request) {
	acc := h.Store.FindAny(r.PathValue("account_id"))
	if acc == nil {
		writeAPIError(w, errNotFound("账号不存在"))
		return
	}
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	archived := true
	if v, ok := payload["archived"]; ok {
		archived = truthy(v)
	}
	if ok, err := h.Store.SetArchived(acc.Provider, acc.ID, archived); err != nil {
		writeError500(w, err)
		return
	} else if !ok {
		writeAPIError(w, errNotFound("账号不存在"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleRefreshAll(w http.ResponseWriter, r *http.Request) {
	// 对齐 refresh：payload.all → 全部 zai jwt 账号；否则按 ids 过滤（仅 jwt）。
	// 请求体可空（FastAPI Body(default=None) 语义）。
	payload := map[string]any{}
	if raw, err := io.ReadAll(r.Body); err == nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			writeAPIError(w, errBadRequest("请求体不是合法 JSON"))
			return
		}
	}
	// 与后台周期监控同一套筛选（quota.Monitor）：跳过已归档与已停用账号。
	// store.SetArchived 的契约是「调度、领取、刷新全部跳过」，而刷新还会经
	// handleBillingResponse 把归档账号的状态写回 active，与归档语义冲突。
	var targets []*model.Account
	if truthy(payload["all"]) {
		for _, a := range h.Store.ListAccounts(model.ProviderZai) {
			if a.Mode == "jwt" && a.ArchivedAt == nil && a.Status != model.StatusDisabled {
				targets = append(targets, a)
			}
		}
	} else {
		ids := map[string]bool{}
		if raw, ok := payload["ids"].([]any); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok {
					ids[s] = true
				}
			}
		}
		for _, a := range h.Store.ListAccounts("") {
			if ids[a.ID] && a.Mode == "jwt" && a.ArchivedAt == nil && a.Status != model.StatusDisabled {
				targets = append(targets, a)
			}
		}
	}
	summary := h.Quota.RefreshAccounts(targets)
	writeJSON(w, http.StatusOK, map[string]any{"summary": summary, "count": len(targets)})
}

func (h *Handler) handleRefreshAccount(w http.ResponseWriter, r *http.Request) {
	acc := h.Store.FindAny(r.PathValue("account_id"))
	if acc == nil {
		writeAPIError(w, errNotFound("账号不存在"))
		return
	}
	// 归档账号不参与刷新（与周期监控、批量刷新一致）：刷新会把它写回 active，
	// 与「归档即停止调用」的语义冲突。
	if acc.ArchivedAt != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"message": "账号已归档，不参与额度刷新",
		})
		return
	}
	if acc.Mode != "jwt" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"message": "仅 Coding Plan (JWT) 账号支持额度查询",
		})
		return
	}
	res := h.Quota.FetchQuota(acc)
	// FetchQuota 在锁内落库，这里重新取快照以反映最新状态
	updated := h.Store.FindAny(r.PathValue("account_id"))
	if updated == nil {
		updated = acc
	}
	_, hasErr := res["error"]
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      !hasErr,
		"result":  res,
		"account": updated.PublicView(time.Now()),
	})
}

func (h *Handler) handleResetStats(w http.ResponseWriter, r *http.Request) {
	acc := h.Store.FindAny(r.PathValue("account_id"))
	if acc == nil {
		writeAPIError(w, errNotFound("账号不存在"))
		return
	}
	if err := h.Store.Update(acc.Provider, acc.ID, func(a *model.Account) {
		a.ResetTokenStats()
	}); err != nil {
		writeError500(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleCaptchaConfig(w http.ResponseWriter, r *http.Request) {
	cfg := h.Captcha.FetchConfig(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": cfg.Enabled,
		"prefix":  cfg.Prefix,
		"region":  cfg.Region,
		"sceneId": cfg.SceneID,
	})
}

func (h *Handler) handleCaptchaSubmit(w http.ResponseWriter, r *http.Request) {
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	param := strings.TrimSpace(strOf(firstTruthy(payload["verify_param"])))
	if param == "" {
		writeAPIError(w, errBadRequest("verify_param 不能为空"))
		return
	}
	cfg := h.Captcha.FetchConfig(r.Context())
	if err := h.Captcha.SetManualParam(param, cfg.Region); err != nil {
		writeAPIError(w, errBadRequest(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"admin_key":              h.Store.AdminKey(),
		"gateway_key":            h.Store.GatewayKey(),
		"quota_refresh_interval": h.Store.QuotaRefreshInterval(),
	})
}

func (h *Handler) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	if v, ok := payload["admin_key"]; ok {
		key := strings.TrimSpace(strOf(v))
		if key == "" {
			writeAPIError(w, errBadRequest("后台密钥不能为空"))
			return
		}
		if err := h.Store.SetSetting("admin_key", key); err != nil {
			writeError500(w, err)
			return
		}
	}
	if v, ok := payload["gateway_key"]; ok {
		key := strings.TrimSpace(strOf(v))
		if key == "" {
			// 网关密钥必填：存空值会让 verify_gateway_key 拒绝所有请求，而非回到免鉴权
			writeAPIError(w, errBadRequest("网关 API Key 不能为空"))
			return
		}
		if err := h.Store.SetSetting("gateway_key", key); err != nil {
			writeError500(w, err)
			return
		}
	}
	if v, ok := payload["quota_refresh_interval"]; ok {
		interval, valid := pyInt(v)
		if !valid {
			writeAPIError(w, errBadRequest("刷新间隔必须是非负整数"))
			return
		}
		interval = max(0, interval)
		if err := h.Store.SetSetting("quota_refresh_interval", strconv.Itoa(interval)); err != nil {
			writeError500(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// pyInt 对应 Python int() 的宽松转换：float 截断、数字字符串解析、bool 转换；
// 其余类型（null/数组/对象）视为 TypeError。
func pyInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		return n, err == nil
	}
	return 0, false
}

// handleExport 导出含明文凭证，仅用于备份/迁移（对齐 Python 版）。
func (h *Handler) handleExport(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.Store.Export())
}

func (h *Handler) handleImport(w http.ResponseWriter, r *http.Request) {
	var payload store.ImportPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, errBadRequest("请求体不是合法 JSON"))
		return
	}
	count, err := h.Store.ImportAccounts(payload)
	if err != nil {
		writeError500(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": count})
}
