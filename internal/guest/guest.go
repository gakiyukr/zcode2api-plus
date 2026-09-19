// Package guest 访客账号提交入口（/guest/*）。
//
// 设计约束（与后台添加账号的关键差异）：
//  1. 只接受 OAuth 登录，不提供令牌输入框。OAuth 授权能证明提交者确实持有
//     该账号，而粘贴一串 JWT 无法证明任何事——后者既容易伪造，也会让本服务
//     成为别人账号的囤积点。
//  2. 账号必须通过一次真实的上游调用才入池。OAuth 授权只说明「现在持有」，
//     不说明「当下可用」：账号可能已被封、额度已耗尽、或地区受限。先测后存
//     可以避免把不可用账号写进池子拖累轮询。
//  3. 凭证不经本包日志，响应体不回显任何账号信息（ID/邮箱/额度一律不给）。
//
// 与 /admin/api/login/* 的关系：两者共用 internal/oauth 的授权链，但后台
// 那条是管理员操作（已鉴权），本包是公开入口，因此额外要求邀请码与按 IP
// 的每日配额，且落库前多一道实测。
package guest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/auth"
	"zcode2api/internal/capverify"
	"zcode2api/internal/captcha"
	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/oauth"
	"zcode2api/internal/quota"
	"zcode2api/internal/store"
	"zcode2api/internal/util"
	"zcode2api/internal/web"
)

// flowTTL 访客登录会话有效期。与后台一致（10 分钟）。
const flowTTL = 600 * time.Second

// testTimeout 入池前实测的超时上限。
// 覆盖验证码求解与一次完整的上游往返；超过即视为不可用，不入池。
const testTimeout = 90 * time.Second

// Handler 访客提交 HTTP 层。
type Handler struct {
	Store   *store.Store
	Auth    *auth.Service
	Captcha *captcha.Manager
	Quota   *quota.Service
	Engine  *gateway.Engine
	Cap     *capverify.Client

	mu    sync.Mutex
	flows map[string]*guestFlow
}

// guestFlow 一次访客登录会话；绑定来源 IP，防止 A 发起、B 完成的接力。
type guestFlow struct {
	flow      *oauth.Flow
	createdAt time.Time
	host      string
}

// New 创建访客处理器。
func New(st *store.Store, authSvc *auth.Service, cm *captcha.Manager, qs *quota.Service, eng *gateway.Engine) *Handler {
	return &Handler{
		Store:   st,
		Auth:    authSvc,
		Captcha: cm,
		Quota:   qs,
		Engine:  eng,
		Cap:     capverify.New(),
		flows:   map[string]*guestFlow{},
	}
}

// Register 注册访客端点。
//
// 全部挂在 /guest/api/* 下，与 /admin/api/* 完全分离：后台路由带 guard，
// 这里带邀请码校验，两套凭据互不影响。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /guest/api/info", h.handleInfo)
	mux.HandleFunc("POST /guest/api/start", h.handleStart)
	mux.HandleFunc("POST /guest/api/complete", h.handleComplete)
}

// handleInfo 返回访客入口是否开放，以及前端渲染人机验证所需的信息。
//
// 邀请码绝不回显（否则任何访问者都能拿到）；Cap 的 endpoint 必须回显——
// 它就是浏览器要访问的公开地址，widget 靠它取题。secret 只留在服务端，
// 绝不能出现在响应里，否则任何人都能自造 token。
func (h *Handler) handleInfo(w http.ResponseWriter, r *http.Request) {
	capCfg := h.Auth.CapConfig()
	resp := map[string]any{
		"enabled": h.Auth.InviteCode() != "",
		"captcha": capCfg.Enabled(),
	}
	if capCfg.Enabled() {
		// 拼好的地址而非三个原始值：前端拿到的就是 widget 该用的值，
		// 拼接规则只在 capverify 一处维护。
		resp["cap_endpoint"] = capCfg.Endpoint()
	}
	gateway.WriteJSON(w, http.StatusOK, resp)
}

// verifyCaptcha 校验请求携带的 Cap token。
//
// 返回 false 表示已写回错误响应，调用方应立即返回。
// 未配置 Cap 时直接放行——自建实例地址因部署而异，无法给出默认值，
// 因此「未配置」是合法的关闭状态而非配置错误。
func (h *Handler) verifyCaptcha(w http.ResponseWriter, r *http.Request) bool {
	cfg := h.Auth.CapConfig()
	if !cfg.Enabled() {
		return true
	}
	token := strings.TrimSpace(r.Header.Get("x-cap-token"))
	err := h.Cap.Verify(r.Context(), cfg, token)
	switch {
	case err == nil:
		return true
	case errors.Is(err, capverify.ErrInvalidToken):
		writeDetail(w, http.StatusBadRequest, "人机验证未通过，请重新验证")
	default:
		// 服务端故障与「访客没解对」是两回事：前者要管理员去查配置，
		// 报成「验证失败」会把排查方向带偏。
		web.Warn("guest", "人机验证服务不可用: "+err.Error())
		writeDetail(w, http.StatusBadGateway, "人机验证服务暂时不可用，请稍后再试")
	}
	return false
}

func (h *Handler) handleStart(w http.ResponseWriter, r *http.Request) {
	if e := h.Auth.VerifyInvite(r); e != nil {
		gateway.WriteAuthError(w, e)
		return
	}

	host := auth.ClientHost(r)
	h.cleanupFlows()

	// 同一 IP 只保留一个进行中会话：已有会话时这次请求是「换个链接」，
	// 替换旧的即可，既不重复扣当日配额、也不要求再解一次人机验证。
	//
	// 换链接不产生任何上游开销，也不增加内存（旧会话先被删掉），重新解一次
	// PoW 只会让访客白等；而真正昂贵的 complete（真实上游实测）仍然每次都
	// 需要一枚新 token，所以放宽这里不构成绕过。
	replacing := h.hasFlowForHost(host)
	if !replacing {
		// 人机验证在配额之前：机器人刷 start 会先被挡在这里，不至于白扣访客的
		// 当日额度（配额是给人用的，不该被自动化请求消耗）。
		if !h.verifyCaptcha(w, r) {
			return
		}
		// 配额在首次生成时扣减，这是当日额度的实际闸门：complete 会消耗掉会话，
		// 下次生成又算首次，故一个来源最多提交 3 次。若不在这里扣，昂贵的实测
		// （真实上游调用 + 验证码求解，上限 90s）就能被无限重试。
		if !h.Auth.AllowGuestSubmission(r) {
			writeDetail(w, http.StatusTooManyRequests, "今日提交次数已用完")
			return
		}
	}

	flow := oauth.NewFlow()
	flowID, authorizeURL, err := flow.Init()
	if err != nil {
		gateway.WriteJSON(w, http.StatusBadGateway, map[string]any{"detail": "授权初始化失败"})
		return
	}

	// 先构造好新会话再替换旧的：Init 失败时旧会话仍在，访客不至于两头落空。
	if replacing {
		h.dropFlowForHost(host)
	}
	h.mu.Lock()
	h.flows[flowID] = &guestFlow{flow: flow, createdAt: time.Now(), host: host}
	h.mu.Unlock()
	gateway.WriteJSON(w, http.StatusOK, map[string]any{"flow_id": flowID, "authorize_url": authorizeURL})
}

// hasFlowForHost 该来源 IP 是否已有进行中的会话。
func (h *Handler) hasFlowForHost(host string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, gf := range h.flows {
		if gf.host == host {
			return true
		}
	}
	return false
}

// dropFlowForHost 删除该来源 IP 的进行中会话。
//
// 一个 IP 同时只保留一个会话：会话带 TTL 且只在下次请求时才清扫，允许同一
// 来源反复生成会让内存随请求次数增长；而「换个链接」本来也不需要保留旧的
// ——旧 state 一旦被替换就再没有对应的会话，用旧链接完成会得到明确的
// 「会话不存在」而不是含糊的失败。
func (h *Handler) dropFlowForHost(host string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, gf := range h.flows {
		if gf.host == host {
			delete(h.flows, id)
		}
	}
}

// handleComplete 完成授权：兑换凭证 → 实测 → 通过才入池。
//
// 响应刻意不含任何账号信息（ID、邮箱、额度、状态一律不返回）。访客只需知道
// 成功与否；账号入池后的归属与状态是管理员的事。
func (h *Handler) handleComplete(w http.ResponseWriter, r *http.Request) {
	if e := h.Auth.VerifyInvite(r); e != nil {
		gateway.WriteAuthError(w, e)
		return
	}
	// 第二步再验一次：Cap token 是一次性的，start 用过的那枚已经失效，
	// 前端会为这一步重新求解。两处都验可以防止「先解一次，然后脚本化
	// 重复调用 complete」。
	if !h.verifyCaptcha(w, r) {
		return
	}
	payload, ok := decodeBody(r)
	if !ok {
		writeDetail(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	flowID := strings.TrimSpace(strOf(payload["flow_id"]))
	callbackURL := strings.TrimSpace(strOf(payload["callback_url"]))

	h.cleanupFlows()
	// 取出即移除：并发的两个 complete 请求若都拿到同一个会话，会各自向上游
	// 兑换一次（会话在下方才被删，检查已经通过）。改成在锁内一次完成「取出 +
	// 移除」，只有一个请求能拿到会话，另一个得到 404。
	//
	// 代价是「格式错误/state 不匹配」的重试也要重新 start（会再扣一次配额）。
	// 这是有意的取舍：那些错误在本地就能判定，而放行并发兑换意味着一次授权
	// 可以换来任意次上游往返。
	h.mu.Lock()
	gf := h.flows[flowID]
	delete(h.flows, flowID)
	h.mu.Unlock()
	if gf == nil {
		writeDetail(w, http.StatusNotFound, "登录会话不存在或已过期")
		return
	}
	// 会话与来源 IP 绑定：授权链接可能被转发，允许跨 IP 完成会让邀请码
	// 泄露后的滥用面扩大。
	if gf.host != auth.ClientHost(r) {
		writeDetail(w, http.StatusForbidden, "登录会话与请求来源不匹配")
		return
	}

	code, state, oauthErr, err := oauth.ParseCallbackURL(callbackURL)
	if err != nil {
		writeDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	if oauthErr != "" {
		writeDetail(w, http.StatusBadRequest, fmt.Sprintf("Z.AI 拒绝授权: %s", oauthErr))
		return
	}
	if !gf.flow.MatchesState(state) {
		writeDetail(w, http.StatusBadRequest, "回调地址与当前登录会话不匹配")
		return
	}

	// 配额已在 start 阶段扣减（一次授权 = 一次配额），此处不再重复扣。
	result, err := gf.flow.ExchangeCode(code, state)
	if err != nil {
		writeDetail(w, http.StatusBadRequest, err.Error())
		return
	}

	h.finishSubmission(r.Context(), w, result)
}

// finishSubmission 实测凭证，通过才入池。
func (h *Handler) finishSubmission(ctx context.Context, w http.ResponseWriter, result *oauth.ExchangeResult) {
	// 先用临时账号做实测，不入池。AddAccount 会落库，所以这里只能手工构造
	// 一个内存对象——它携带真实凭证，但不在 Store 里，测失败直接丢弃。
	probe := model.Create(model.ProviderZai, "guest-probe", result.Token)
	if probe.Mode != "jwt" {
		// OAuth 兑换出来的必然是 JWT；走到这里说明上游返回了异常形态
		gateway.WriteJSON(w, http.StatusBadGateway, map[string]any{"detail": "授权返回的凭证形态异常"})
		return
	}

	// 真实对话实测：这是唯一能证明「账号当下可用」的手段。
	//
	// 不能用 quota.FetchQuota 代替：它绑定 Store（先取快照再写状态），而这里
	// 的 probe 刻意不入池，FetchQuota 会直接回「账号已不存在」。何况额度端点
	// 只覆盖计费接口，无法证明对话链路（含验证码）可用——而这正是要验证的。
	testCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	res := h.Engine.TestAccount(testCtx, probe, "")
	if !res.OK {
		web.Warn("guest", "访客账号实测未通过: "+res.Reason)
		writeDetail(w, http.StatusBadRequest, fmt.Sprintf("账号实测未通过: %s", res.Reason))
		return
	}

	// 实测通过，正式入池。
	acc, err := h.Store.AddAccount(model.ProviderZai, guestAccountName(result), result.Token)
	if err != nil {
		web.Warn("guest", "访客账号落库失败: "+err.Error())
		writeDetail(w, http.StatusInternalServerError, "账号保存失败")
		return
	}
	if result.Email != nil && strings.TrimSpace(*result.Email) != "" {
		email := strings.TrimSpace(*result.Email)
		if err := h.Store.Update(acc.Provider, acc.ID, func(a *model.Account) {
			a.Email = &email
		}); err != nil {
			web.Warn("guest", "访客账号邮箱落库失败: "+err.Error())
		}
	}
	// 兑换 API Key 作为备用凭证（失败不影响 JWT 已入池）
	if result.AccessToken != "" {
		if apiKey, err := oauth.ExchangeAPIKey(result.AccessToken); err == nil && apiKey != "" {
			if err := h.Store.Update(acc.Provider, acc.ID, func(a *model.Account) {
				a.APIKey = &apiKey
			}); err != nil {
				web.Warn("guest", "访客账号 API Key 落库失败: "+err.Error())
			}
		}
	}

	// 入池后刷新一次额度，与后台添加账号一致：账号卡片立刻显示真实额度，
	// 而不是等下一轮周期监控（默认 60s）。
	if fresh := h.Store.Find(acc.Provider, acc.ID); fresh != nil {
		h.Quota.RefreshAccounts([]*model.Account{fresh})
	}

	web.Ok("guest", "访客提交账号已入池")
	// 不回显账号 ID / 邮箱 / 额度：访客只需知道结果。
	gateway.WriteJSON(w, http.StatusOK, map[string]any{
		"status":  "accepted",
		"message": "账号已通过校验并入池，感谢提交",
	})
}

// guestAccountName 访客账号的显示名。
// 用邮箱（可读）或时间戳兜底；不暴露给访客，仅供后台辨识来源。
func guestAccountName(result *oauth.ExchangeResult) string {
	if result.Email != nil {
		if email := strings.TrimSpace(*result.Email); email != "" {
			return email
		}
	}
	return "guest-" + util.RandomHex(4)
}

func (h *Handler) cleanupFlows() {
	h.mu.Lock()
	defer h.mu.Unlock()
	cutoff := time.Now().Add(-flowTTL)
	for id, gf := range h.flows {
		if gf.createdAt.Before(cutoff) {
			delete(h.flows, id)
		}
	}
}

// ── HTTP 小工具 ─────────────────────────────────────────────────────────────

// writeDetail 输出 FastAPI 形态的错误体（{"detail": ...}），与后台 API 一致，
// 前端可统一处理。
func writeDetail(w http.ResponseWriter, status int, message string) {
	gateway.WriteJSON(w, status, map[string]any{"detail": message})
}

func decodeBody(r *http.Request) (map[string]any, bool) {
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		return nil, false
	}
	if payload == nil {
		payload = map[string]any{}
	}
	return payload, true
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

