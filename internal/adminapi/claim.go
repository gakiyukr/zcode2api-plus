// 套餐领取端点（/admin/api/claim/*）：对应 Python 版 admin_api.py 的
// claim_preview / claim_plans，以及入池自动领取触发点（批量添加 / OAuth）。
package adminapi

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/claim"
	"zcode2api/internal/model"
	"zcode2api/internal/web"
)

// autoClaimTasks 强引用持有后台自动领取任务（对齐 Python _auto_claim_tasks）。
var autoClaimTasks sync.WaitGroup

// jwtAccounts 全部/指定 ID 的 JWT 账号（对齐 Python _jwt_accounts）。
func (h *Handler) jwtAccounts(ids []string) []*model.Account {
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	var out []*model.Account
	for _, acc := range h.Store.ListAccounts(model.ProviderZai) {
		if len(wanted) > 0 && !wanted[acc.ID] {
			continue
		}
		// 已归档账号不参与批量领取（显式指定单个账号时仍允许，便于排查）
		if acc.ArchivedAt != nil && len(wanted) == 0 {
			continue
		}
		if acc.Mode == "jwt" && acc.JWTToken != nil && *acc.JWTToken != "" {
			out = append(out, acc)
		}
	}
	return out
}

// scheduleAutoClaim 入池后后台自动领取（fire-and-forget；对齐 _schedule_auto_claim）。
func (h *Handler) scheduleAutoClaim(acc *model.Account) {
	svc := claim.NewService(h.Captcha)
	autoClaimTasks.Add(1)
	go func() {
		defer autoClaimTasks.Done()
		defer func() {
			if r := recover(); r != nil {
				web.Warn("claim", "自动领取任务异常（已兜底）")
			}
		}()
		_ = svc.AutoClaimAllPlans(acc)
	}()
}

// handleClaimPreview GET /admin/api/claim/preview?account_id=
// 立即拉取可领取套餐（全部/单个 JWT 账号）；先上报激活事件（失败不阻断）。
func (h *Handler) handleClaimPreview(w http.ResponseWriter, r *http.Request) {
	var ids []string
	if v := strings.TrimSpace(r.URL.Query().Get("account_id")); v != "" {
		ids = []string{v}
	}
	svc := claim.NewService(h.Captcha)
	out := []map[string]any{}
	for _, acc := range h.jwtAccounts(ids) {
		now := time.Now()
		if !acc.IsSelectable(now) && acc.Status == model.StatusCooling {
			out = append(out, map[string]any{
				"account_id": acc.ID, "account_name": acc.Name, "plans": []any{},
				"error":     "賬號冷卻中（風控/限流），已跳過上游查詢",
				"activated": false, "activation_error": nil,
			})
			continue
		}
		activationError := claim.ReportActivationEvents(acc)
		activated := activationError == ""
		entry := map[string]any{
			"account_id": acc.ID, "account_name": acc.Name,
			"plans": []any{}, "error": nil,
			"activated": activated, "activation_error": nil,
		}
		if activationError != "" {
			entry["activation_error"] = activationError
		}
		plans, err := svc.PreviewPlans(acc)
		if err != nil {
			entry["error"] = err.Error()
		} else {
			entry["plans"] = plans
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"preview": out})
}

// handleClaim POST /admin/api/claim（body 可选 account_ids / plan_id）
// 缺省对全部 JWT 账号自动选最优套餐；冷却账号跳过（上游写流量，风控期不加剧）。
func (h *Handler) handleClaim(w http.ResponseWriter, r *http.Request) {
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		payload = map[string]any{} // Python Body(default=None)：空体全量领取
	}
	var ids []string
	if raw, ok := payload["account_ids"].([]any); ok {
		for _, item := range raw {
			if s, isStr := item.(string); isStr && s != "" {
				ids = append(ids, s)
			}
		}
	}
	planID := strings.TrimSpace(strOf(payload["plan_id"]))

	candidates := h.jwtAccounts(ids)
	outcomes := []map[string]any{}
	for _, acc := range candidates {
		// 用 IsSelectable 判断（与 preview 一致）：EffectiveStatus 把「冷却
		// 已到期」视为 active，只看原始 Status 会让同一账号 preview 可查、
		// claim 被拒，用户看到自相矛盾的结果。
		if !acc.IsSelectable(time.Now()) && acc.Status == model.StatusCooling {
			outcomes = append(outcomes, map[string]any{
				"account_id": acc.ID, "account_name": acc.Name, "ok": false,
				"message": "賬號冷卻中（風控/限流），已跳過領取",
			})
			continue
		}
		svc := claim.NewService(h.Captcha)
		result, err := svc.Claim(acc, planID)
		if err != nil {
			web.Warn("claim", "账号 "+acc.Name+" 领取失败: "+err.Error())
			outcomes = append(outcomes, map[string]any{
				"account_id": acc.ID, "account_name": acc.Name,
				"ok": false, "message": err.Error(),
			})
			continue
		}
		h.Quota.RefreshAccounts([]*model.Account{acc})
		outcome := map[string]any{
			"account_id": acc.ID, "account_name": acc.Name, "ok": true,
		}
		for k, v := range result {
			outcome[k] = v
		}
		outcomes = append(outcomes, outcome)
	}
	ok := 0
	for _, o := range outcomes {
		if b, isBool := o["ok"].(bool); isBool && b {
			ok++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"outcomes": outcomes,
		"summary":  map[string]any{"ok": ok, "fail": len(outcomes) - ok},
	})
}
