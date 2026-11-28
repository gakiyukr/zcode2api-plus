// 网关引擎：/v1/messages 的选号 + 验证码 + 上游调用 + 错误分类循环。
// 对应 Python 版 routes/gateway.py 的 messages() 与 _try_account()。
// M4 的 OpenAI 兼容层复用本引擎（通过 DeliverFunc 自定义交付）。
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/store"
	"zcode2api/internal/upstream"
	"zcode2api/internal/util"
	"zcode2api/internal/web"
)

// edgeHoldMap 按出口地址（代理 URL 或 "direct"）隔離的上游邊緣防火牆 405 熔斷
// 截止時間表（key: addrKey string, value: int64 unix nano）。不同出口 IP 互不阻塞。
var edgeHoldMap sync.Map

func normalizeEdgeKey(addrKey string) string {
	if strings.TrimSpace(addrKey) == "" {
		return "direct"
	}
	return addrKey
}

// HoldEdgeFor 開啟指定出口的邊緣熔斷：duration 內該出口不再發送請求，其他出口不受影響。
func HoldEdgeFor(addrKey string, duration time.Duration, now time.Time) {
	now = now.Round(0)
	edgeHoldMap.Store(normalizeEdgeKey(addrKey), now.Add(duration).UnixNano())
}

// EdgeHeldFor 判斷指定出口目前是否處於邊緣熔斷期。
func EdgeHeldFor(addrKey string, now time.Time) bool {
	now = now.Round(0)
	v, ok := edgeHoldMap.Load(normalizeEdgeKey(addrKey))
	if !ok {
		return false
	}
	until, _ := v.(int64)
	return until > 0 && now.UnixNano() < until
}

// HoldEdge 開啟直連出口的邊緣熔斷（相容舊介面包裝）。
func HoldEdge(duration time.Duration, now time.Time) {
	HoldEdgeFor("direct", duration, now)
}

// EdgeHeld 判斷直連出口是否處於邊緣熔斷期（相容舊介面包裝）。
func EdgeHeld(now time.Time) bool {
	return EdgeHeldFor("direct", now)
}

// Delivery 上游 200 响应的交付物；Body 已被 usage 收集器包装（流式）或已缓冲。
type Delivery struct {
	StatusCode  int
	ContentType string
	Header      http.Header
	Body        io.Reader
}

// DeliverFunc 交付上游 200 响应；返回 nil 表示客户端完整接收（计入 usage 统计），
// 返回错误表示客户端侧中断（不累计 usage，语义对齐 Python 版）。
type DeliverFunc func(d Delivery) error

// Engine 选号 + 验证码 + 上游调用 + 错误分类循环。
type Engine struct {
	Store   *store.Store
	Captcha *captcha.Manager
	Client  *http.Client
	// Gate 管理 Start Plan（JWT）雙層併發（單號 3 + 同出口 6）、自適應收窄與預防性忙線熔斷。
	Gate *StartPlanGate
	// Affinity 管理三級會話親和性路由（LRU 8192，TTL 30m），提升上游 Prompt Cache 命中率。
	Affinity *SessionAffinity

	// BusyRetryDelays 3008/3009/3010 併發准入的重試延遲（預設 1s/2s；測試可縮短）。
	BusyRetryDelays []time.Duration
	// ReplayDelay 未寫完請求體或複用斷連透明重放前的等待間隔（預設 250ms；測試可縮短）。
	ReplayDelay time.Duration
	// UploadStallTimeout 請求體上傳流控看門狗逾時（預設 90s；測試可縮短）。
	UploadStallTimeout time.Duration
	// OnQuotaRefresh 成功/耗尽后触发的额度刷新（M3 接入 quota 包；nil 跳过）。
	OnQuotaRefresh func(acc *model.Account)

	now func() time.Time
}

// NewEngine 创建引擎；client 为 nil 时使用默认上游客户端。
func NewEngine(st *store.Store, cm *captcha.Manager, client *http.Client) *Engine {
	if client == nil {
		client = defaultUpstreamClient()
	}
	return &Engine{
		Store:              st,
		Captcha:            cm,
		Client:             client,
		Gate:               NewStartPlanGate(),
		Affinity:           NewSessionAffinity(DefaultAffinityMaxEntries, DefaultAffinityTTL),
		BusyRetryDelays:    startPlanBusyRetryDelays,
		ReplayDelay:        DefaultReplayDelay,
		UploadStallTimeout: DefaultUploadStallTimeout,
		now:                time.Now,
	}
}

// SetNow 注入时钟（测试用）。
func (e *Engine) SetNow(fn func() time.Time) { e.now = fn }

func (e *Engine) wallNow() time.Time { return e.now().Round(0) }

func (e *Engine) replayDelay() time.Duration {
	if e.ReplayDelay > 0 {
		return e.ReplayDelay
	}
	return DefaultReplayDelay
}

func (e *Engine) uploadStallTimeout() time.Duration {
	if e.UploadStallTimeout > 0 {
		return e.UploadStallTimeout
	}
	return DefaultUploadStallTimeout
}

// closeIdleForAccount 驅逐該帳號所屬出口的髒閒置連線（401/403/405/斷連重放時呼叫）。
func (e *Engine) closeIdleForAccount(acc *model.Account) {
	if acc == nil || acc.ProxyURL == nil || strings.TrimSpace(*acc.ProxyURL) == "" {
		if e.Client != nil {
			e.Client.CloseIdleConnections()
		}
		proxy.CloseIdleFor("")
		return
	}
	proxy.CloseIdleFor(*acc.ProxyURL)
}

// clientFor 返回账号出站客户端：配置了代理（proxy_url，含代理线路指派）时
// 走代理 Transport，否则用引擎默认客户端。代理构造失败回退直连并记日志。
func (e *Engine) clientFor(acc *model.Account) *http.Client {
	if acc == nil || acc.ProxyURL == nil || *acc.ProxyURL == "" {
		return e.Client
	}
	t, err := proxy.TransportFor(*acc.ProxyURL)
	if err != nil {
		web.Warn("gateway", fmt.Sprintf("账号 %s 代理无效，回退直连: %v", acc.Name, err))
		return e.Client
	}
	return &http.Client{Transport: t}
}

// defaultUpstreamClient 對齊官方與長串流語義：強制啟用 HTTP/2、SmartDialContext（IPv6 零位元組自動降級）、
// 響應頭最長 120s、響應體串流讀取不設總逾時。
func defaultUpstreamClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           proxy.SmartDialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// runResult 循环结束状态：Delivered=true 表示已通过 deliver 交付上游 200
// （handler 不得再写响应）；否则 Status/Body 是要回给客户端的错误 JSON。
type runResult struct {
	Delivered bool
	Status    int
	Body      any
}

// emptyReply 暫存非串流 HTTP 200 空回覆（無文字也無 tool_use），供全池皆空時原樣交回。
type emptyReply struct {
	acc         *model.Account
	contentType string
	buffered    []byte
}

// attemptResult 單次帳號嘗試的出路：retryCaptcha（同帳號換驗證碼令牌重試）、
// skippedCaptchaRetry（免驗遇 3007/挑戰頭，不扣減重試次數立即補驗）、
// retryBusy（同帳號併發准入睡前釋放槽位延遲重試）、requeueGate（觸發自適應收窄，歸還槽位排回隊首）、
// switchAccount（換下一個帳號）、bare429（疑似請求級裸 429）、edgeBlocked（命中出口邊緣 405）、final（終止並返回）。
type attemptResult struct {
	retryCaptcha        bool
	skippedCaptchaRetry bool
	retryBusy           bool
	busyDelay           time.Duration
	requeueGate         bool
	switchAccount       bool
	bare429             bool
	edgeBlocked         bool
	empty               *emptyReply
	final               runResult
}

// ResolveModel 解析請求模型名：若屬退役旗艦別名（如 GLM-5.3），且帳號池中無
// 可選帳號對原模型標記 available、但存在可選帳號對 LiveFlagshipModel（GLM-5.4）
// 標記 available，則自動改寫為 GLM-5.4，讓舊客戶端無感渡過上游改名。
func ResolveModel(st *store.Store, modelName string, now time.Time) string {
	alias := FlagshipAlias(modelName)
	if alias == "" || st == nil {
		return modelName
	}
	hasOrig, hasAlias := false, false
	for _, a := range st.ListAccounts(model.ProviderZai) {
		if !a.IsSelectable(now) {
			continue
		}
		if a.ModelAvailability(modelName) == "available" {
			hasOrig = true
			break
		}
		if a.ModelAvailability(alias) == "available" {
			hasAlias = true
		}
	}
	if !hasOrig && hasAlias {
		return alias
	}
	return modelName
}

// RunMessages 执行完整循环。body 是入口 NormalizeBody 后的请求体；
// incomingHeaders 为客户端透传头；deliver 在上游 200 时被调用一次。
func (e *Engine) RunMessages(ctx context.Context, body map[string]any, incomingHeaders map[string]string, deliver DeliverFunc) runResult {
	modelName, _ := body["model"].(string)
	if resolved := ResolveModel(e.Store, modelName, e.wallNow()); resolved != modelName {
		web.Warn("gateway", fmt.Sprintf("旗艦模型自動相容改寫: %s → %s", modelName, resolved))
		modelName = resolved
		body = shallowCopyBody(body)
		body["model"] = modelName
	}
	stream := bodyBool(body, "stream")
	reqID := util.RandomHex(3)
	web.Req(reqID, orDash(modelName), stream)

	lookupKey, bindKey := DeriveAffinityKeys(body, incomingHeaders)
	preferredID := ""
	if e.Affinity != nil && lookupKey != "" {
		preferredID = e.Affinity.Lookup(lookupKey, e.wallNow())
	}

	tried := map[string]bool{}
	var lastEmpty *emptyReply
	edgeBlocked := false
	bare429Count := 0
	var firstBare429Acc *model.Account

	for range MaxAccountAttempts {
		now := e.wallNow()
		var inFlightFn func(string) int
		if e.Gate != nil {
			inFlightFn = e.Gate.AccountInFlight
		}
		acc := e.Store.SelectSmart(model.ProviderZai, tried, modelName, store.SelectOptions{
			Now:         now,
			PreferredID: preferredID,
			InFlight:    inFlightFn,
			CanStick: func(cand *model.Account) bool {
				ak := AddressKeyFor(cand)
				if EdgeHeldFor(ak, now) {
					return false
				}
				if cand.Mode == "jwt" && e.Gate != nil {
					if held, _ := e.Gate.BusyHeld(ak); held {
						return false
					}
					if e.Gate.AccountInFlight(cand.ID) >= e.Gate.AccountCap(cand.ID) {
						return false
					}
				}
				return true
			},
		})
		if acc == nil {
			break
		}
		tried[acc.ID] = true
		if EdgeHeldFor(AddressKeyFor(acc), now) {
			edgeBlocked = true
			if acc.ID == preferredID && e.Affinity != nil {
				e.Affinity.EvictIfAccount(lookupKey, acc.ID)
			}
			web.Warn(reqID, fmt.Sprintf("帳號 %s 所屬出口處於邊緣熔斷期，跳過並嘗試其他出口", acc.Name))
			continue
		}
		res := e.tryAccount(ctx, reqID, acc, body, modelName, stream, true, incomingHeaders, deliver)
		if res.empty != nil {
			lastEmpty = res.empty
		}
		if res.edgeBlocked {
			edgeBlocked = true
		}

		// 請求級裸 429 防雪崩：第 1 號標記探測冷卻並換號探測；連續第 2 號再回裸 429 則僅平反探測冷卻並立即止損
		if res.bare429 {
			bare429Count++
			if acc.ID == preferredID && e.Affinity != nil {
				e.Affinity.EvictIfAccount(lookupKey, acc.ID)
			}
			if bare429Count == 1 {
				firstBare429Acc = acc
				e.mark(acc, model.StatusCooling, Bare429ProbeReason)
				web.Warn(reqID, fmt.Sprintf("帳號 %s 收到裸 429，暫標冷卻並換號探測", acc.Name))
				continue
			}
			if firstBare429Acc != nil {
				e.Store.Update(firstBare429Acc.Provider, firstBare429Acc.ID, func(a *model.Account) {
					if a.Status == model.StatusCooling && a.LastError != nil && *a.LastError == Bare429ProbeReason {
						a.Status = model.StatusActive
						a.CoolingUntil = nil
						a.LastError = nil
					}
				})
			}
			web.Warn(reqID, fmt.Sprintf("連續 2 個帳號觸發無業務碼裸 429（疑似請求級限流），已恢復探路帳號並立即止損"))
			return res.final
		}
		bare429Count = 0
		firstBare429Acc = nil

		if res.retryCaptcha {
			continue
		}
		if res.switchAccount {
			if acc.ID == preferredID && e.Affinity != nil {
				e.Affinity.EvictIfAccount(lookupKey, acc.ID)
			}
			continue
		}
		if res.final.Delivered && e.Affinity != nil && bindKey != "" {
			e.Affinity.Bind(bindKey, acc.ID, e.wallNow())
		}
		return res.final
	}

	// 若全部可選帳號均回傳非串流 200 空回覆，則誠實交付最後一次空回覆
	if lastEmpty != nil {
		res := e.deliverJSONPayload(reqID, lastEmpty.acc, lastEmpty.contentType, lastEmpty.buffered, deliver).final
		if res.Delivered && e.Affinity != nil && bindKey != "" {
			e.Affinity.Bind(bindKey, lastEmpty.acc.ID, e.wallNow())
		}
		return res
	}

	if edgeBlocked {
		web.Warn(reqID, "目前可用出口均處於邊緣熔斷期，拒絕請求 zcode.z.ai")
		return errResult(http.StatusServiceUnavailable, "upstream_edge_blocked",
			"上游邊緣防火牆已封鎖目前出口，稍後自動恢復（可在後台更換出口代理）")
	}

	if modelName != "" {
		web.ReqErr(reqID, fmt.Sprintf("模型 %s 無可用帳號（未提供此模型或額度已用完）", modelName))
		return errResult(http.StatusServiceUnavailable, "no_available_account",
			fmt.Sprintf("模型 %s 目前無可用帳號（帳號未提供此模型或額度已用完），請在後台檢查帳號狀態", modelName))
	}
	web.ReqErr(reqID, "无可用账号 / 额度均已耗尽")
	return errResult(http.StatusServiceUnavailable, "no_available_account",
		"所有账号均不可用或额度已用完，请在后台检查账号状态")
}

// CollectModels 聚合全部账号额度快照中的模型清单（去重、字典序）。
// 上游没有公开的模型列表端点；billing/balance 的 balances[].model 即上游
// 为该账号提供的实际模型，额度刷新（quota.Service）会持续同步进快照。
// 无账号或快照为空时回退 AvailableModels 静态清单，保证端点永远可用。
func (e *Engine) CollectModels() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, acc := range e.Store.ListAccounts(model.ProviderZai) {
		for _, entry := range acc.Quota {
			name, _ := entry["model"].(string)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return append([]string{}, AvailableModels...)
	}
	sort.Strings(out)
	return out
}

// TestAccountResult 定向测试单个账号的结果。
type TestAccountResult struct {
	OK     bool   // 上游返回了可交付的 200 响应
	Status int    // 失败时的 HTTP 状态码（OK 时为 200）
	Reason string // 失败原因（面向调用方的简述，不含凭证）
}

// TestAccount 对指定账号发起一次真实的最小请求，验证其确实可用。
//
// 与 RunMessages 的区别：不做选号轮询，只测这一个账号；不交付给客户端，
// 只判断上游是否给出了可用的 200 响应。用于访客提交账号时「测试调用一次
// 才能入池」——OAuth 授权只证明访客持有该账号，不证明它当下能服务请求。
//
// 注意：本方法会写账号状态（401 标 invalid、429 标 cooling 等），这正是
// 期望行为——测试失败就该如实反映在账号状态上。
func (e *Engine) TestAccount(ctx context.Context, acc *model.Account, modelName string) TestAccountResult {
	if acc == nil {
		return TestAccountResult{OK: false, Status: http.StatusBadRequest, Reason: "账号不存在"}
	}
	if modelName == "" {
		modelName = AvailableModels[len(AvailableModels)-1]
	}

	body := map[string]any{
		"model":      modelName,
		"max_tokens": float64(1),
		"messages": []any{
			map[string]any{"role": "user", "content": "ping"},
		},
	}
	reqID := util.RandomHex(3)
	web.Req(reqID, orDash(modelName), false)

	// 收集型 deliver：TestAccount 只关心上游是否给了 200，不把内容写出去。
	// 必须读完 Body，否则引擎的 usage 统计与连接复用都会受影响。
	var delivered bool
	deliver := func(d Delivery) error {
		delivered = true
		_, _ = io.Copy(io.Discard, d.Body)
		return nil
	}

	res := e.tryAccount(ctx, reqID, acc, body, modelName, false, false, nil, deliver)
	if res.bare429 {
		e.mark(acc, model.StatusCooling, "上游限流 HTTP 429")
	}
	if delivered {
		web.ReqOk(reqID, 0)
		return TestAccountResult{OK: true, Status: http.StatusOK}
	}

	// 未交付：res.final 携带失败原因（switchAccount/retryCaptcha 在单账号
	// 语境下等价于「这次尝试没能拿到 200」）。
	status := res.final.Status
	if status == 0 {
		status = http.StatusBadGateway
	}
	reason := "上游未返回可用响应"
	if body, ok := res.final.Body.(map[string]any); ok {
		if errObj, ok := body["error"].(map[string]any); ok {
			if msg, ok := errObj["message"].(string); ok && msg != "" {
				reason = msg
			}
		}
	}
	web.ReqErr(reqID, reason)
	return TestAccountResult{OK: false, Status: status, Reason: reason}
}

// tryAccount 單個帳號的嘗試：外層處理 Start Plan 雙層閘門與收窄隊首重排，
// 內層處理驗證碼與併發准入重試（對齊 MaxCaptchaRetries）。
func (e *Engine) tryAccount(
	ctx context.Context,
	reqID string,
	acc *model.Account,
	body map[string]any,
	modelName string,
	stream bool,
	retryEmpty bool,
	incomingHeaders map[string]string,
	deliver DeliverFunc,
) attemptResult {
	needsCaptcha := acc.Mode == "jwt"
	addrKey := AddressKeyFor(acc)

	for requeueCount := 0; requeueCount <= MaxGateRequeue; requeueCount++ {
		var lease *GateLease
		if needsCaptcha && e.Gate != nil {
			var err error
			lease, err = e.Gate.Acquire(ctx, acc.ID, addrKey, requeueCount > 0)
			if err != nil {
				if isCanceled(ctx, err) {
					web.Warn(reqID, "客户端已断开，终止等待")
					return canceledResult()
				}
				var holdErr *BusyHoldError
				if errors.As(err, &holdErr) {
					web.Warn(reqID, holdErr.Error())
					return attemptResult{final: errResult(http.StatusTooManyRequests, "start_plan_busy_hold", holdErr.Error())}
				}
				var waitErr *GateWaitError
				if errors.As(err, &waitErr) {
					web.Warn(reqID, waitErr.Error())
					return attemptResult{final: errResult(http.StatusTooManyRequests, "rate_limit_error", waitErr.Error())}
				}
				return attemptResult{final: errResult(http.StatusTooManyRequests, "rate_limit_error", err.Error())}
			}
		}

		res := e.tryAccountWithLease(ctx, reqID, acc, addrKey, body, modelName, stream, retryEmpty, needsCaptcha, requeueCount, incomingHeaders, deliver, &lease)
		if lease != nil {
			lease.Release()
			lease = nil
		}
		if res.requeueGate && requeueCount < MaxGateRequeue {
			web.Warn(reqID, fmt.Sprintf("帳號 %s 併發上限已自適應收窄，歸還槽位並排回隊首重試（第 %d 次）", acc.Name, requeueCount+1))
			continue
		}
		return res
	}
	return attemptResult{final: errResult(http.StatusTooManyRequests, "rate_limit_error", "併發重排次數已達上限")}
}

// tryAccountWithLease 在已持有 Start Plan 閘門租約期間執行上游呼叫與內層重試。
// 傳入 leasePtr 供 3008/3009/3010 延遲重試前主動歸還槽位、醒來後隊首重取，避免睡眠佔槽。
func (e *Engine) tryAccountWithLease(
	ctx context.Context,
	reqID string,
	acc *model.Account,
	addrKey string,
	body map[string]any,
	modelName string,
	stream bool,
	retryEmpty bool,
	needsCaptcha bool,
	requeueCount int,
	incomingHeaders map[string]string,
	deliver DeliverFunc,
	leasePtr **GateLease,
) attemptResult {
	captchaAttempt := 0
	busyAttempt := 0
	forceCaptcha := false
	for captchaAttempt < MaxCaptchaRetries {
		var verifyParam, verifyRegion string
		if needsCaptcha {
			token, err := e.Captcha.GetVerifyParamForModel(ctx, addrKey, forceCaptcha)
			if err != nil {
				// 客戶端斷線不得清空全域驗證碼快取
				if isCanceled(ctx, err) {
					web.Warn(reqID, "客户端已断开，终止验证码获取")
					return canceledResult()
				}
				e.Captcha.InvalidateForAddr(addrKey)
				captchaAttempt++
				if captchaAttempt < MaxCaptchaRetries {
					web.Warn(reqID, fmt.Sprintf("验证码自动求解失败，刷新令牌重试（第 %d 次）", captchaAttempt))
					continue
				}
				return attemptResult{final: e.captchaRequired(reqID, err.Error())}
			}
			if token != nil {
				verifyParam, verifyRegion = token.VerifyParam, token.Region
			}
		}
		skippedCaptcha := needsCaptcha && verifyParam == ""

		req, err := upstream.BuildRequest(acc, verifyParam, verifyRegion, incomingHeaders)
		if err != nil {
			e.mark(acc, model.StatusInvalid, err.Error())
			web.Warn(reqID, fmt.Sprintf("账号 %s 凭证无效，切换下一个", acc.Name))
			return attemptResult{switchAccount: true}
		}

		// 每個帳號在副本上進行 ToolSearch 清洗、QueryID 計算與 NormalizeBodyFor
		// （須於 DressSystem 插入日期前綴之前計算 QueryID，確保使用者輪次計數正確）
		actualBody := shallowCopyBody(body)
		toolSearchRenamed := RewriteToolSearchRequest(actualBody)
		upstream.SetQueryID(&req, actualBody)
		NormalizeBodyFor(actualBody, needsCaptcha, upstream.AccountDeviceMid(acc), req.Headers["x-session-id"])
		payload, err := util.MarshalJSON(actualBody)
		if err != nil {
			web.Err(reqID, fmt.Sprintf("请求体序列化失败: %v", err))
			return attemptResult{final: errResult(http.StatusBadRequest, "invalid_request", "请求体无法序列化")}
		}

		// 出站呼叫 + 未寫完請求體／複用斷連 1 次透明重放（250ms）+ 上傳流控看門狗
		var (
			resp          *http.Response
			reqCleanup    func()
			sendAttempted attemptResult
			sendFailed    bool
		)
		for netAttempt := 0; netAttempt < 2; netAttempt++ {
			tracedCtx, traceState := AttachReplayTrace(ctx)
			reqCtx, reqBody, cleanup, stalled := WrapBodyWithStallGuard(tracedCtx, payload, e.uploadStallTimeout(), traceState)

			httpReq, reqErr := http.NewRequestWithContext(reqCtx, http.MethodPost, req.URL, reqBody)
			if reqErr != nil {
				cleanup()
				web.Err(reqID, fmt.Sprintf("上游地址无效（检查 ZAI_UPSTREAM_URL）: %v", reqErr))
				return attemptResult{final: errResult(http.StatusBadGateway, "invalid_upstream_url",
					"上游地址配置无效，请检查 ZAI_UPSTREAM_URL")}
			}
			httpReq.ContentLength = int64(len(payload))
			for k, v := range req.Headers {
				httpReq.Header.Set(k, v)
			}

			r, doErr := e.clientFor(acc).Do(httpReq)
			if doErr == nil {
				resp = r
				reqCleanup = cleanup
				break
			}
			wasStalled := stalled()
			cleanup()
			if wasStalled {
				e.closeIdleForAccount(acc)
				web.Warn(reqID, fmt.Sprintf("帳號 %s 上游請求體上傳流控停滯逾時，驅逐閒置連線並切換帳號", acc.Name))
				sendAttempted = attemptResult{switchAccount: true}
				sendFailed = true
				break
			}
			if isCanceled(ctx, doErr) {
				web.Warn(reqID, "客户端已断开，终止重试")
				sendAttempted = canceledResult()
				sendFailed = true
				break
			}
			if netAttempt == 0 && ShouldTransparentReplay(ctx, doErr, traceState, false) {
				e.closeIdleForAccount(acc)
				web.Warn(reqID, fmt.Sprintf("帳號 %s 出站連線未送達即斷開，%dms 後透明重放 1 次", acc.Name, e.replayDelay().Milliseconds()))
				select {
				case <-ctx.Done():
					sendAttempted = canceledResult()
					sendFailed = true
				case <-time.After(e.replayDelay()):
					continue
				}
				break
			}
			e.mark(acc, model.StatusCooling, "连接失败: "+doErr.Error())
			web.Warn(reqID, fmt.Sprintf("账号 %s 连接失败，切换下一个", acc.Name))
			sendAttempted = attemptResult{switchAccount: true}
			sendFailed = true
			break
		}
		if sendFailed {
			return sendAttempted
		}

		if resp.StatusCode >= 400 {
			res := e.handleUpstreamError(ctx, reqID, acc, addrKey, modelName, needsCaptcha, skippedCaptcha, resp, captchaAttempt, busyAttempt, requeueCount)
			reqCleanup()
			if res.skippedCaptchaRetry {
				forceCaptcha = true
				continue
			}
			if res.retryCaptcha {
				captchaAttempt++
				continue
			}
			if res.retryBusy {
				// 睡眠前主動歸還 Start Plan 槽位，避免等待期間阻塞其他併發請求；醒來隊首重取
				if leasePtr != nil && *leasePtr != nil {
					(*leasePtr).Release()
					*leasePtr = nil
				}
				select {
				case <-ctx.Done():
					return canceledResult()
				case <-time.After(res.busyDelay):
				}
				if needsCaptcha && e.Gate != nil {
					if held, rem := e.Gate.BusyHeld(addrKey); held {
						holdErr := &BusyHoldError{Remaining: rem}
						web.Warn(reqID, holdErr.Error())
						return attemptResult{final: errResult(http.StatusTooManyRequests, "start_plan_busy_hold", holdErr.Error())}
					}
					newLease, acqErr := e.Gate.Acquire(ctx, acc.ID, addrKey, true)
					if acqErr != nil {
						if isCanceled(ctx, acqErr) {
							web.Warn(reqID, "客户端已断开，终止等待")
							return canceledResult()
						}
						var holdErr *BusyHoldError
						if errors.As(acqErr, &holdErr) {
							web.Warn(reqID, holdErr.Error())
							return attemptResult{final: errResult(http.StatusTooManyRequests, "start_plan_busy_hold", holdErr.Error())}
						}
						var waitErr *GateWaitError
						if errors.As(acqErr, &waitErr) {
							web.Warn(reqID, waitErr.Error())
							return attemptResult{final: errResult(http.StatusTooManyRequests, "rate_limit_error", waitErr.Error())}
						}
						return attemptResult{final: errResult(http.StatusTooManyRequests, "rate_limit_error", acqErr.Error())}
					}
					if leasePtr != nil {
						*leasePtr = newLease
					}
				}
				busyAttempt++
				continue
			}
			return res
		}

		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/json"
		}

		// HTTP 200 且為 JSON：限長 32 MiB 緩衝，處理業務錯誤、靜默空拒絕與空回覆故障轉移
		if strings.Contains(contentType, "application/json") {
			buffered, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
			_ = resp.Body.Close()
			reqCleanup()
			if err != nil {
				if isCanceled(ctx, err) {
					web.Warn(reqID, "客戶端已斷開，終止讀取回應")
					return canceledResult()
				}
				e.bumpFail(acc)
				web.ReqErr(reqID, fmt.Sprintf("上游回應體讀取失敗（帳號 %s）", acc.Name))
				return attemptResult{final: errResult(http.StatusBadGateway, "upstream_error", textPreview(err.Error()))}
			}
			if toolSearchRenamed {
				buffered = RestoreToolSearchJSON(buffered)
			}
			res := e.handleUpstreamJSON(reqID, acc, addrKey, modelName, needsCaptcha, skippedCaptcha, stream, retryEmpty, contentType, buffered, captchaAttempt, deliver)
			if res.skippedCaptchaRetry {
				forceCaptcha = true
				continue
			}
			if res.retryCaptcha {
				captchaAttempt++
				continue
			}
			return res
		}

		// SSE 成功：串流交付結束並關閉 Body 後再釋放看門狗上下文
		defer reqCleanup()
		return e.deliverStream(reqID, acc, contentType, resp, toolSearchRenamed, deliver)
	}

	// 验证码重试次数耗尽
	if needsCaptcha {
		return attemptResult{final: e.captchaRequired(reqID, "验证码重试次数已耗尽")}
	}
	return attemptResult{switchAccount: true}
}

// handleUpstreamError 处理上游 >=400 的响应（分类链顺序对齐 PLAN §5.2，不可变）。
func (e *Engine) handleUpstreamError(
	ctx context.Context,
	reqID string,
	acc *model.Account,
	addrKey string,
	modelName string,
	needsCaptcha bool,
	skippedCaptcha bool,
	resp *http.Response,
	captchaAttempt int,
	busyAttempt int,
	requeueCount int,
) attemptResult {
	// 错误响应体限长：正常路径是流式转发（边读边发），只有错误分支才整段读进
	// 内存。上游或账号级代理异常时可能回一个极大的 body，无上限会直接吃光内存。
	// 错误体只需够分类（业务码 + 简短 msg），64KB 足够。
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	_ = resp.Body.Close()
	if err != nil {
		// 读不出错误体：客户端断开同样会让读失败，此时不能归咎于账号
		if isCanceled(ctx, err) {
			web.Warn(reqID, "客户端已断开，终止重试")
			return canceledResult()
		}
		e.mark(acc, model.StatusCooling, "连接失败: "+err.Error())
		return attemptResult{switchAccount: true}
	}
	text := string(body)

	// 1) 验证码挑战（响应头 / code=3007 / F001 与文本仅限 400/403）
	if needsCaptcha && IsCaptchaError(text, resp.StatusCode, resp.Header) {
		if skippedCaptcha {
			e.Captcha.InvalidateModelSkip()
			web.Warn(reqID, fmt.Sprintf("帳號 %s 免驗請求遭遇上游驗證碼挑戰，已失效 skip_model_request 並立即補驗", acc.Name))
			return attemptResult{skippedCaptchaRetry: true}
		}
		e.Captcha.InvalidateForAddr(addrKey)
		web.Warn(reqID, fmt.Sprintf("账号 %s 验证码失效，刷新重试（第 %d 次）", acc.Name, captchaAttempt+1))
		if captchaAttempt+1 >= MaxCaptchaRetries {
			detail := "上游连续拒绝验证码"
			if hasCaptchaChallengeHeader(resp.Header) {
				detail = "上游持续返回验证码挑战"
			}
			return attemptResult{final: e.captchaRequired(reqID, detail)}
		}
		return attemptResult{retryCaptcha: true}
	}

	// 1.5) 上游风控（405）：先区分边缘拦截页与账号级拒绝——两者共用同一句
	// "request has been blocked" 文案，处置完全不同。
	if IsZcodeEdgeBlocked(resp.StatusCode, text) {
		// 邊緣攔截頁 = 該出口 IP 被封：熔斷該出口、驅逐髒連線並換下一個其他出口帳號
		HoldEdgeFor(addrKey, time.Duration(config.EdgeHoldSeconds)*time.Second, e.wallNow())
		e.closeIdleForAccount(acc)
		e.bumpFail(acc)
		web.Warn(reqID, fmt.Sprintf("上游邊緣防火牆攔截出口（%s），熔斷 %d s 並嘗試切換其他出口帳號", addrKey, config.EdgeHoldSeconds))
		return attemptResult{
			switchAccount: true,
			edgeBlocked:   true,
			final: errResult(http.StatusServiceUnavailable, "upstream_edge_blocked",
				"上游邊緣防火牆已封鎖目前出口，稍後自動恢復（可在後台更換出口代理）"),
		}
	}
	if IsZcodeRestricted(resp.StatusCode, text) {
		// 3012 = 账号被风控限制（官方 App 同样被拒）：驅逐髒連線、長冷卻休息並換號
		e.closeIdleForAccount(acc)
		e.bumpFail(acc)
		MarkAccountCooling(e.Store, acc.Provider, acc.ID,
			"上游风控 3012 拒绝（HTTP 405）", time.Duration(config.RestrictedCooldownSeconds)*time.Second, e.wallNow())
		web.Warn(reqID, fmt.Sprintf("账号 %s 被上游风控限制（3012），冷却 %d s 后重试", acc.Name, config.RestrictedCooldownSeconds))
		return attemptResult{switchAccount: true}
	}

	// 2) 鉴权失败是强信号（JWT 上游为裸 401 空 body；api.z.ai 为 type=1000/1001/1003）
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		e.closeIdleForAccount(acc)
		e.mark(acc, model.StatusInvalid, fmt.Sprintf("鉴权失败 HTTP %d", resp.StatusCode))
		web.Warn(reqID, fmt.Sprintf("账号 %s 鉴权失败 %d，切换下一个", acc.Name, resp.StatusCode))
		return attemptResult{switchAccount: true}
	}

	// 3) 402 → 该模型耗尽
	if resp.StatusCode == http.StatusPaymentRequired {
		e.markModelExhausted(acc, modelName, fmt.Sprintf("%s 額度已用完", OrCurrent(modelName)))
		web.Warn(reqID, fmt.Sprintf("账号 %s 的 %s 額度用完，切換下一個", acc.Name, OrCurrent(modelName)))
		e.fireRefresh(acc)
		return attemptResult{switchAccount: true}
	}

	// 4) 429 且 code∈{3008,3009,3010}：併發准入限制，帳號仍可用，先於持槽期回報閘門再交由外層釋槽睡眠重試
	if IsModelConcurrencyLimit(resp.StatusCode, text) {
		if needsCaptcha && e.Gate != nil {
			requeue, holdTriggered := e.Gate.RecordConcurrencyRefusal(acc.ID, addrKey, busyAttempt)
			if holdTriggered {
				_, rem := e.Gate.BusyHeld(addrKey)
				holdErr := &BusyHoldError{Remaining: rem}
				web.Warn(reqID, holdErr.Error())
				return attemptResult{final: errResult(http.StatusTooManyRequests, "start_plan_busy_hold", holdErr.Error())}
			}
			if requeue && requeueCount < MaxGateRequeue {
				return attemptResult{requeueGate: true}
			}
		}
		if busyAttempt < len(e.BusyRetryDelays) {
			delay := e.BusyRetryDelays[busyAttempt]
			web.Warn(reqID, fmt.Sprintf("模型併發准入受限，先歸還槽位並於 %g s 後隊首重取重試", delay.Seconds()))
			return attemptResult{retryBusy: true, busyDelay: delay}
		}
		web.Warn(reqID, fmt.Sprintf("模型併發准入持續受限（帳號 %s），保留帳號狀態", acc.Name))
		return attemptResult{final: runResult{
			Status: resp.StatusCode,
			Body:   passthroughBodyWithType(text, "upstream_rate_limit"),
		}}
	}

	// 5) 429：官方用量上限码族 → 该模型耗尽；無業務碼裸 429 → 交由 RunMessages 雙號探測止損；其餘瞬時限流 → 冷卻
	if resp.StatusCode == http.StatusTooManyRequests {
		if quotaExhaustedCodes[UpstreamBusinessCode(text)] {
			e.markModelExhausted(acc, modelName, fmt.Sprintf("%s 額度/用量上限已達", OrCurrent(modelName)))
			web.Warn(reqID, fmt.Sprintf("账号 %s 的 %s 觸發用量上限，切換下一個", acc.Name, OrCurrent(modelName)))
			e.fireRefresh(acc)
			return attemptResult{switchAccount: true}
		}
		if IsBare429(resp.StatusCode, text, resp.Header) {
			msg := text
			if strings.TrimSpace(msg) == "" {
				msg = Bare429FallbackMessage
			}
			return attemptResult{
				switchAccount: true,
				bare429:       true,
				final: runResult{
					Status: resp.StatusCode,
					Body:   passthroughBodyWithType(msg, "upstream_rate_limit"),
				},
			}
		}
		e.mark(acc, model.StatusCooling, "上游限流 HTTP 429")
		web.Warn(reqID, fmt.Sprintf("账号 %s 被限流 429，切换下一个", acc.Name))
		return attemptResult{switchAccount: true}
	}

	// 6) 503 → 冷却换号
	if resp.StatusCode == http.StatusServiceUnavailable {
		e.bumpFail(acc)
		e.mark(acc, model.StatusCooling, "上游服務不可用 HTTP 503")
		web.Warn(reqID, fmt.Sprintf("账号 %s 上游返回 503，進入冷卻並切換下一個", acc.Name))
		return attemptResult{switchAccount: true}
	}

	// 7) 其余错误：非已知信号，不做账号状态推断，直接原样继承上游响应
	e.bumpFail(acc)
	web.ReqErr(reqID, fmt.Sprintf("上游错误 HTTP %d（账号 %s）", resp.StatusCode, acc.Name))
	return attemptResult{final: runResult{
		Status: resp.StatusCode,
		Body:   passthroughBodyWithType(text, "upstream_error"),
	}}
}

// handleUpstreamJSON 处理 HTTP 200 且 content-type 为 JSON 的响应：
// ZCode 的业务错误有时仍使用 HTTP 200，不能当成 Anthropic 成功回應。
func (e *Engine) handleUpstreamJSON(
	reqID string,
	acc *model.Account,
	addrKey string,
	modelName string,
	needsCaptcha bool,
	skippedCaptcha bool,
	stream bool,
	retryEmpty bool,
	contentType string,
	buffered []byte,
	captchaAttempt int,
	deliver DeliverFunc,
) attemptResult {
	text := string(buffered)
	code := UpstreamBusinessCode(text)

	switch {
	case code == "1005":
		e.markModelExhausted(acc, modelName, fmt.Sprintf("%s 每日額度已用完", OrCurrent(modelName)))
		web.Warn(reqID, fmt.Sprintf("帳號 %s 的 %s 每日額度用完，切換下一個", acc.Name, OrCurrent(modelName)))
		e.fireRefresh(acc)
		return attemptResult{switchAccount: true}

	case needsCaptcha && code == "3007":
		if skippedCaptcha {
			e.Captcha.InvalidateModelSkip()
			web.Warn(reqID, fmt.Sprintf("帳號 %s 免驗請求收到 HTTP 200 業務碼 3007，已失效 skip_model_request 並立即補驗", acc.Name))
			return attemptResult{skippedCaptchaRetry: true}
		}
		e.Captcha.InvalidateForAddr(addrKey)
		web.Warn(reqID, fmt.Sprintf("帳號 %s 驗證碼失效，刷新重試（第 %d 次）", acc.Name, captchaAttempt+1))
		if captchaAttempt+1 >= MaxCaptchaRetries {
			return attemptResult{final: e.captchaRequired(reqID, "上游連續拒絕驗證碼")}
		}
		return attemptResult{retryCaptcha: true}

	case code != "" && code != "0":
		e.bumpFail(acc)
		web.ReqErr(reqID, fmt.Sprintf("上游業務錯誤 code=%s（帳號 %s）", code, acc.Name))
		return attemptResult{final: runResult{
			Status: http.StatusBadGateway,
			Body: map[string]any{"error": map[string]any{
				"message": MessageFromJSON(text, buffered),
				"type":    "upstream_error",
				"code":    code,
			}},
		}}

	case IsRefusedEnvelope(buffered):
		// 上游靜默版本拒絕空信封：不記帳號失敗、不換號空轉，直接回報客戶端版本異常
		web.ReqErr(reqID, fmt.Sprintf("上游返回靜默空拒絕信封（帳號 %s，疑似版本過舊）", acc.Name))
		return attemptResult{final: errResult(http.StatusBadGateway, "upstream_version_refused",
			"上游靜默拒絕請求（返回空信封，疑似客戶端版本號過舊，請檢查 ZCODE_CLIENT_VERSION）")}

	case stream:
		e.bumpFail(acc)
		web.ReqErr(reqID, fmt.Sprintf("上游串流請求返回非 SSE JSON（帳號 %s）", acc.Name))
		return attemptResult{final: errResult(http.StatusBadGateway, "invalid_upstream_response", "上游未返回有效的 SSE 串流")}
	}

	// 非串流空回覆（HTTP 200 且具 content 陣列但無非空 text 亦無 tool_use）：
	// 換號重試並保留本次空回覆，若全池皆空才原樣交付（對齊 D3-vin/Zcode2Api）。
	if retryEmpty && isEmptyAnthropicReply(buffered) {
		web.Warn(reqID, fmt.Sprintf("帳號 %s 回傳空回覆（無 text/tool_use），切換下一個帳號重試", acc.Name))
		return attemptResult{
			switchAccount: true,
			empty:         &emptyReply{acc: acc, contentType: contentType, buffered: buffered},
		}
	}

	return e.deliverJSONPayload(reqID, acc, contentType, buffered, deliver)
}

func (e *Engine) deliverJSONPayload(reqID string, acc *model.Account, contentType string, buffered []byte, deliver DeliverFunc) attemptResult {
	e.success(acc)
	usage := NewUsageCollector(strings.Contains(contentType, "text/event-stream"))
	usage.Feed(buffered)
	usage.Finish()
	err := deliver(Delivery{
		StatusCode:  http.StatusOK,
		ContentType: contentType,
		Header:      http.Header{},
		Body:        bytes.NewReader(buffered),
	})
	return e.finishDelivery(reqID, acc, usage, err)
}

// isEmptyAnthropicReply 判斷非串流 JSON 回覆是否為「含 content 陣列但無非空 text 且無 tool_use」的空回覆。
// 不含 content 欄位的非標準 JSON 視為非空，避免誤判。
func isEmptyAnthropicReply(buffered []byte) bool {
	var payload map[string]any
	if err := json.Unmarshal(buffered, &payload); err != nil {
		return false
	}
	blocks, ok := payload["content"].([]any)
	if !ok {
		return false
	}
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			if t, _ := block["text"].(string); strings.TrimSpace(t) != "" {
				return false
			}
		case "thinking":
			// 空白 thinking 區塊僅為佔位結構，須含實質思考文字才視為非空回覆
			if t, _ := block["thinking"].(string); strings.TrimSpace(t) != "" {
				return false
			}
		case "tool_use", "redacted_thinking":
			return false
		}
	}
	return true
}

// deliverStream 交付 SSE 流式响应；usage 随读取同步收集，客户端完整接收后计入。
func (e *Engine) deliverStream(reqID string, acc *model.Account, contentType string, resp *http.Response, restoreToolSearch bool, deliver DeliverFunc) attemptResult {
	e.success(acc)
	usage := NewUsageCollector(strings.Contains(contentType, "text/event-stream"))
	var bodyReader io.Reader = resp.Body
	if restoreToolSearch {
		bodyReader = RestoreToolSearchStream(bodyReader)
	}
	err := deliver(Delivery{
		StatusCode:  resp.StatusCode,
		ContentType: contentType,
		Header:      resp.Header,
		Body:        &teeReader{r: bodyReader, c: usage},
	})
	_ = resp.Body.Close()
	return e.finishDelivery(reqID, acc, usage, err)
}

// finishDelivery 交付收尾：完整交付 → 累计 usage；客户端中断 → 只记日志不累计。
func (e *Engine) finishDelivery(reqID string, acc *model.Account, usage *UsageCollector, err error) attemptResult {
	if err != nil {
		web.ReqErr(reqID, fmt.Sprintf("流传输中断: %v", err))
		return attemptResult{final: runResult{Delivered: true}}
	}
	usage.Finish()
	got := usage.AsDict()
	e.Store.Update(acc.Provider, acc.ID, func(a *model.Account) {
		a.AccumulateTokens(got)
	})
	web.ReqOk(reqID, got.Output)
	return attemptResult{final: runResult{Delivered: true}}
}

// ── 账号状态标记（对齐 Python 版 _mark / _mark_model_exhausted）──────────────
//
// MarkAccount / MarkModelExhausted 同时导出，供 asyncpool 复用同一套状态机；
// 两条请求路径对同一账号必须标出相同状态（曾因 asyncpool 自行实现而分歧）。
//
// 二者按账号 ID 定位并在 Store 锁内修改真实对象：调用方持有的是 Select
// 返回的深拷贝，直接改它既不会落库、也会与并发读取竞争。

// MarkAccount 设置账号状态；status 为 cooling 时按配置写入冷却截止时间（使用牆鐘時間）。
// 账号不存在时静默返回（可能已被后台删除）。
func MarkAccount(st *store.Store, provider, id, status, errMsg string, now time.Time) {
	now = now.Round(0)
	st.Update(provider, id, func(acc *model.Account) {
		acc.Status = status
		acc.LastError = &errMsg
		if status == model.StatusCooling {
			until := float64(now.Add(time.Duration(config.CoolingSeconds)*time.Second).UnixNano()) / 1e9
			acc.CoolingUntil = &until
		}
	})
}

// MarkAccountCooling 以自定义冷却时长标记账号 cooling（使用牆鐘時間）。
func MarkAccountCooling(st *store.Store, provider, id, errMsg string, cooldown time.Duration, now time.Time) {
	now = now.Round(0)
	st.Update(provider, id, func(acc *model.Account) {
		acc.Status = model.StatusCooling
		until := float64(now.Add(cooldown).UnixNano()) / 1e9
		acc.CoolingUntil = &until
		acc.LastError = &errMsg
	})
}

// MarkModelExhausted 只停用已耗尽的请求模型；所有已知模型皆耗尽时才停用整号。
//
// 只负责「额度」这一类信号，因此不覆盖更强的状态：invalid（凭据已失效）、
// cooling（刚被上游限流）、disabled（人工停用）都保持原状。额度信号没有
// 资格断言账号已恢复——否则并发请求里 A 刚标出的 invalid 会被 B 的额度
// 信号刷成 active，失效账号重新进入轮询，每次选中都是一次白费的上游调用。
func MarkModelExhausted(st *store.Store, provider, id string, modelName any, errMsg string) {
	st.Update(provider, id, func(acc *model.Account) {
		if !acc.MarkModelExhausted(modelName) {
			// 模型名无法归一化：无法做模型级停用，退回整号停用
			if isStrongStatus(acc.Status) {
				return
			}
			acc.Status = model.StatusExhausted
			acc.LastError = &errMsg
			return
		}
		if isStrongStatus(acc.Status) {
			// 模型级标记已写入 ExhaustedModels，这里不动状态与 last_error：
			// 把「刚被 429 限流」改写成「额度用完」会掩盖真实原因。
			return
		}
		anyState := false
		allExhausted := true
		for name, quota := range acc.Quota {
			entryModel, _ := quota["model"].(string)
			if entryModel == "" {
				entryModel = name
			}
			anyState = true
			if acc.ModelAvailability(entryModel) != "exhausted" {
				allExhausted = false
				break
			}
		}
		if anyState && allExhausted {
			acc.Status = model.StatusExhausted
		} else {
			acc.Status = model.StatusActive
		}
		acc.CoolingUntil = nil
		acc.LastError = &errMsg
	})
}

// isStrongStatus 判断账号是否处于「比额度耗尽更强」的状态：
// 这些状态由凭据校验或上游限流直接判定，额度信号不得覆盖。
func isStrongStatus(status string) bool {
	switch status {
	case model.StatusInvalid, model.StatusCooling, model.StatusDisabled:
		return true
	}
	return false
}

func (e *Engine) mark(acc *model.Account, status, errMsg string) {
	MarkAccount(e.Store, acc.Provider, acc.ID, status, errMsg, e.wallNow())
}

func (e *Engine) markModelExhausted(acc *model.Account, modelName any, errMsg string) {
	MarkModelExhausted(e.Store, acc.Provider, acc.ID, modelName, errMsg)
}

// MarkSuccess 记录一次成功调用：累计调用次数与最后使用时间，并把
// cooling/exhausted 复位为 active（有成功响应即证明账号当前可用）。
//
// 导出供 async 池复用：两条请求路径对同一账号必须记出相同的统计与状态，
// 否则后台用量页会漏算 async 流量，冷却到期的账号也只能等下一轮额度轮询
// 才恢复调度。
func MarkSuccess(st *store.Store, provider, id string, now time.Time) {
	now = now.Round(0)
	ts := float64(now.UnixNano()) / 1e9
	st.Update(provider, id, func(a *model.Account) {
		a.UseCount++
		a.LastUsedAt = &ts
		if a.Status == model.StatusCooling || a.Status == model.StatusExhausted {
			a.Status = model.StatusActive
		}
	})
}

// success 记录成功调用的账号状态；并异步触发一次额度刷新
// （对齐 Python 200 成功路径的 create_task(_safe_refresh)）。
func (e *Engine) success(acc *model.Account) {
	MarkSuccess(e.Store, acc.Provider, acc.ID, e.wallNow())
	e.fireRefresh(acc)
}

func (e *Engine) bumpFail(acc *model.Account) {
	e.Store.Update(acc.Provider, acc.ID, func(a *model.Account) {
		a.FailCount++
	})
}

// maxErrorBodyBytes 错误响应体的读取上限。
//
// 正常响应走流式转发不受影响；错误分支要整段读进内存做分类，而上游或代理
// 异常时可能回一个任意大的 body。64KB 足以容纳业务码与错误消息。
const maxErrorBodyBytes = 64 << 10

// fireRefresh 触发额度刷新（M3 接入 quota 包；仅 JWT 账号，对齐 _safe_refresh）。
func (e *Engine) fireRefresh(acc *model.Account) {
	if e.OnQuotaRefresh == nil {
		return
	}
	if acc.Provider != model.ProviderZai || acc.Mode != "jwt" {
		return
	}
	go e.OnQuotaRefresh(acc)
}

// captchaRequired 验证码不可用的统一 503 响应。
func (e *Engine) captchaRequired(reqID, detail string) runResult {
	web.ReqErr(reqID, "验证码不可用: "+detail)
	return errResult(http.StatusServiceUnavailable, "captcha_required",
		"自动验证码暂时失败，请稍后重试；如仍失败再打开后台 /admin/captcha")
}

// ── 小工具 ──────────────────────────────────────────────────────────────────

// marshalJSON 与 Python json.dumps(ensure_ascii=False) 对齐：不转义 HTML 字符。

// teeReader 在读取时同步餵入 usage 收集器。
type teeReader struct {
	r io.Reader
	c *UsageCollector
}

func (t *teeReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.c.Feed(p[:n])
	}
	return n, err
}

func shallowCopyBody(body map[string]any) map[string]any {
	out := make(map[string]any, len(body)+1)
	maps.Copy(out, body)
	return out
}

func bodyBool(body map[string]any, key string) bool {
	b, _ := body[key].(bool)
	return b
}

func errResult(status int, errType, msg string) runResult {
	return runResult{
		Status: status,
		Body:   map[string]any{"error": map[string]any{"message": msg, "type": errType}},
	}
}

// canceledResult 客户端主动断开时的统一响应。
//
// 此时响应通常已写不出去（连接已断），返回它只是为了终止重试循环、
// 让调用方走正常收尾路径；关键是**不写任何账号状态**——中断与账号健康无关。
func canceledResult() attemptResult {
	return attemptResult{final: errResult(http.StatusServiceUnavailable, "canceled", "请求已取消")}
}

// isCanceled 判断上游调用失败是否源于 ctx 取消/超时（而非账号或网络问题）。
//
// http.Client.Do 会把底层错误包进 *url.Error，context.Canceled 在 errors.Is
// 下仍可穿透，因此同时检查 ctx 自身状态作为兜底（例如 cancel 与 Do 竞态时
// 返回的是连接层错误）。
func isCanceled(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// passthroughBodyWithType 解析上游错误体透传；解析失败时构造兜底错误结构。
func passthroughBodyWithType(text, fallbackType string) any {
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err == nil {
		return payload
	}
	return map[string]any{"error": map[string]any{
		"message": textPreview(text),
		"type":    fallbackType,
	}}
}

// MessageFromJSON 取业务错误的 msg/message 字段，回退到正文预览。
// 导出供 async 池复用：两条路径对同一上游响应必须给出相同的错误文案。
func MessageFromJSON(text string, raw []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err == nil {
		for _, key := range []string{"msg", "message"} {
			if s, ok := payload[key].(string); ok && s != "" {
				return s
			}
		}
	}
	return textPreview(text)
}

func textPreview(text string) string {
	if len(text) > 500 {
		return text[:500]
	}
	return text
}

func hasCaptchaChallengeHeader(header http.Header) bool {
	for _, name := range captchaHeaders {
		if header.Get(name) != "" {
			return true
		}
	}
	return false
}

// OrCurrent 模型名为空时的占位文案。
// 导出供 asyncpool 复用：两条路径对同一情境必须给出相同文案。
func OrCurrent(modelName string) string {
	if modelName == "" {
		return "當前模型"
	}
	return modelName
}

func orDash(modelName string) string {
	if modelName == "" {
		return "-"
	}
	return modelName
}
