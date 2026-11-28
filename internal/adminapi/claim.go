// 套餐领取端点（/admin/api/claim/*）：对应 Python 版 admin_api.py 的
// claim_preview / claim_plans，以及入池自动领取触发点（批量添加 / OAuth）。
package adminapi

import (
	"bytes"
	"encoding/json"
	"io"
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

// scheduleAutoClaim 入池後背景自動領取（fire-and-forget）；領取成功時同步刷新額度。
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
		outcomes := svc.AutoClaimAllPlans(acc)
		if h.Quota != nil {
			for _, o := range outcomes {
				if ok, _ := o["ok"].(bool); ok {
					h.Quota.RefreshAccounts([]*model.Account{acc})
					break
				}
			}
		}
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

// handleClaim POST /admin/api/claim（body 可選 account_ids / plan_id）
// 空 body 視為預設全量領取；非空但 JSON 格式錯誤時回傳 400。
func (h *Handler) handleClaim(w http.ResponseWriter, r *http.Request) {
	payload := map[string]any{}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, errBadRequest("讀取請求體失敗"))
		return
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
			writeAPIError(w, errBadRequest("請求體不是合法 JSON 物件"))
			return
		}
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
	outcomes := make([]map[string]any, len(candidates))

	// 受控併發（上限 4 workers）+ 單號後間隔 400ms，避免大帳號池全量領取打爆出站代理池
	type claimJob struct {
		idx int
		acc *model.Account
	}
	workers := min(claim.BatchMaxWorkers, len(candidates))
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan claimJob, len(candidates))
	for i, acc := range candidates {
		jobs <- claimJob{idx: i, acc: acc}
	}
	close(jobs)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				acc := job.acc
				if !acc.IsSelectable(time.Now()) && acc.Status == model.StatusCooling {
					outcomes[job.idx] = map[string]any{
						"account_id": acc.ID, "account_name": acc.Name, "ok": false,
						"message": "賬號冷卻中（風控/限流），已跳過領取",
					}
					continue
				}
				svc := claim.NewService(h.Captcha)
				result, err := svc.Claim(acc, planID)
				if err != nil {
					web.Warn("claim", "账号 "+acc.Name+" 领取失败: "+err.Error())
					outcomes[job.idx] = map[string]any{
						"account_id": acc.ID, "account_name": acc.Name,
						"ok": false, "message": err.Error(),
					}
				} else {
					h.Quota.RefreshAccounts([]*model.Account{acc})
					outcome := map[string]any{
						"account_id": acc.ID, "account_name": acc.Name, "ok": true,
					}
					for k, v := range result {
						outcome[k] = v
					}
					outcomes[job.idx] = outcome
				}
				// 多帳號全量領取時節流避讓出站代理
				if len(candidates) > 1 {
					time.Sleep(claim.BatchAccountPause)
				}
			}
		}()
	}
	wg.Wait()
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
