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
	"net"
	"net/http"
	"strings"
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

	// BusyRetryDelays 3010 并发准入的重试延迟（默认 1s/2s；测试可缩短）。
	BusyRetryDelays []time.Duration
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
		Store:           st,
		Captcha:         cm,
		Client:          client,
		BusyRetryDelays: startPlanBusyRetryDelays,
		now:             time.Now,
	}
}

// SetNow 注入时钟（测试用）。
func (e *Engine) SetNow(fn func() time.Time) { e.now = fn }

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

// defaultUpstreamClient 对齐 Python 版超时语义：连接 30s、响应头最长 120s、
// 响应体流式读取不设超时（长连接 SSE）。
func defaultUpstreamClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
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

// attemptResult 单次账号尝试的出路：retryCaptcha（同账号换令牌重试）、
// switchAccount（换下一个账号）、final（终止并返回）。
type attemptResult struct {
	retryCaptcha  bool
	switchAccount bool
	final         runResult
}

// RunMessages 执行完整循环。body 是入口 NormalizeBody 后的请求体；
// incomingHeaders 为客户端透传头；deliver 在上游 200 时被调用一次。
func (e *Engine) RunMessages(ctx context.Context, body map[string]any, incomingHeaders map[string]string, deliver DeliverFunc) runResult {
	modelName, _ := body["model"].(string)
	stream := bodyBool(body, "stream")
	reqID := util.RandomHex(3)
	web.Req(reqID, orDash(modelName), stream)

	tried := map[string]bool{}
	for range MaxAccountAttempts {
		acc := e.Store.Select(model.ProviderZai, tried, modelName)
		if acc == nil {
			break
		}
		tried[acc.ID] = true
		res := e.tryAccount(ctx, reqID, acc, body, modelName, stream, incomingHeaders, deliver)
		if res.retryCaptcha {
			continue
		}
		if res.switchAccount {
			continue
		}
		return res.final
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

	res := e.tryAccount(ctx, reqID, acc, body, modelName, false, nil, deliver)
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

// tryAccount 单个账号的尝试：内层为验证码重试（对齐 MAX_CAPTCHA_RETRIES）。
func (e *Engine) tryAccount(
	ctx context.Context,
	reqID string,
	acc *model.Account,
	body map[string]any,
	modelName string,
	stream bool,
	incomingHeaders map[string]string,
	deliver DeliverFunc,
) attemptResult {
	needsCaptcha := acc.Mode == "jwt"

	for captchaAttempt := range MaxCaptchaRetries {
		var verifyParam, verifyRegion string
		if needsCaptcha {
			token, err := e.Captcha.GetVerifyParam(ctx)
			if err != nil {
				e.Captcha.Invalidate()
				if captchaAttempt+1 < MaxCaptchaRetries {
					web.Warn(reqID, fmt.Sprintf("验证码自动求解失败，刷新令牌重试（第 %d 次）", captchaAttempt+1))
					continue
				}
				return attemptResult{final: e.captchaRequired(reqID, err.Error())}
			}
			if token != nil {
				verifyParam, verifyRegion = token.VerifyParam, token.Region
			}
		}

		// 每个账号在副本上做 NormalizeBody（system 注入不幂等，见 body.go）
		actualBody := shallowCopyBody(body)
		NormalizeBody(actualBody, needsCaptcha)
		payload, err := util.MarshalJSON(actualBody)
		if err != nil {
			web.Err(reqID, fmt.Sprintf("请求体序列化失败: %v", err))
			return attemptResult{final: errResult(http.StatusBadRequest, "invalid_request", "请求体无法序列化")}
		}

		req, err := upstream.BuildRequest(acc, verifyParam, verifyRegion, incomingHeaders)
		if err != nil {
			e.mark(acc, model.StatusInvalid, err.Error())
			web.Warn(reqID, fmt.Sprintf("账号 %s 凭证无效，切换下一个", acc.Name))
			return attemptResult{switchAccount: true}
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.URL, bytes.NewReader(payload))
		if err != nil {
			// 失败源于 req.URL（即 ZAI_UPSTREAM_URL 配置），与账号凭证无关。
			// 标 invalid 会把整池账号逐个标失效并落库，且 last_error 指向错误方向。
			// 配置错误换号也修不好，直接终止并如实报告。
			web.Err(reqID, fmt.Sprintf("上游地址无效（检查 ZAI_UPSTREAM_URL）: %v", err))
			return attemptResult{final: errResult(http.StatusBadGateway, "invalid_upstream_url",
				"上游地址配置无效，请检查 ZAI_UPSTREAM_URL")}
		}
		for k, v := range req.Headers {
			httpReq.Header.Set(k, v)
		}

		resp, err := e.clientFor(acc).Do(httpReq)
		if err != nil {
			// 客户端主动断开（Ctrl-C、调用方超时、反代截断）会让 ctx 取消，
			// Do 随即返回 context.Canceled。这不是账号的问题：若照「连接失败」
			// 处理，后续每轮 Select→Do 都会立刻失败，最多把 5 个账号各标一次
			// 冷却并落库，小账号池几次中断就全池不可用。此时直接终止，
			// 不写任何账号状态。
			if isCanceled(ctx, err) {
				web.Warn(reqID, "客户端已断开，终止重试")
				return canceledResult()
			}
			e.mark(acc, model.StatusCooling, "连接失败: "+err.Error())
			web.Warn(reqID, fmt.Sprintf("账号 %s 连接失败，切换下一个", acc.Name))
			return attemptResult{switchAccount: true}
		}

		if resp.StatusCode >= 400 {
			res := e.handleUpstreamError(ctx, reqID, acc, modelName, needsCaptcha, resp, captchaAttempt)
			if res.retryCaptcha {
				continue
			}
			return res
		}

		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/json"
		}

		// HTTP 200 且为 JSON：先缓冲，处理 HTTP 200 包装的业务错误
		if strings.Contains(contentType, "application/json") {
			buffered, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				e.bumpFail(acc)
				web.ReqErr(reqID, fmt.Sprintf("上游错误体读取失败（账号 %s）", acc.Name))
				return attemptResult{final: errResult(http.StatusBadGateway, "upstream_error", textPreview(err.Error()))}
			}
			res := e.handleUpstreamJSON(reqID, acc, modelName, needsCaptcha, stream, contentType, buffered, captchaAttempt, deliver)
			if res.retryCaptcha {
				continue
			}
			return res
		}

		// SSE 成功：tee 读取流，交付完整后计入 usage
		return e.deliverStream(reqID, acc, contentType, resp, deliver)
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
	modelName string,
	needsCaptcha bool,
	resp *http.Response,
	captchaAttempt int,
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
		e.Captcha.Invalidate()
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

	// 2) 鉴权失败是强信号（JWT 上游为裸 401 空 body；api.z.ai 为 type=1000/1001/1003）
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
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

	// 4) 429 且 code=3010：模型并发准入限制，账号仍可用，按原版客户端延迟重试
	if IsModelConcurrencyLimit(resp.StatusCode, text) {
		if captchaAttempt < len(e.BusyRetryDelays) {
			delay := e.BusyRetryDelays[captchaAttempt]
			web.Warn(reqID, fmt.Sprintf("模型并发准入受限，%g s 后重试（账号仍可用）", delay.Seconds()))
			select {
			case <-ctx.Done():
				return canceledResult()
			case <-time.After(delay):
			}
			return attemptResult{retryCaptcha: true}
		}
		web.Warn(reqID, fmt.Sprintf("模型并发准入持续受限（账号 %s），保留账号状态", acc.Name))
		return attemptResult{final: runResult{
			Status: resp.StatusCode,
			Body:   passthroughBodyWithType(text, "upstream_rate_limit"),
		}}
	}

	// 5) 429：官方用量上限码族 → 该模型耗尽；其余瞬时限流 → 冷却
	if resp.StatusCode == http.StatusTooManyRequests {
		if quotaExhaustedCodes[UpstreamBusinessCode(text)] {
			e.markModelExhausted(acc, modelName, fmt.Sprintf("%s 額度/用量上限已達", OrCurrent(modelName)))
			web.Warn(reqID, fmt.Sprintf("账号 %s 的 %s 觸發用量上限，切換下一個", acc.Name, OrCurrent(modelName)))
			e.fireRefresh(acc)
		} else {
			e.mark(acc, model.StatusCooling, "上游限流 HTTP 429")
			web.Warn(reqID, fmt.Sprintf("账号 %s 被限流 429，切换下一个", acc.Name))
		}
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
	modelName string,
	needsCaptcha bool,
	stream bool,
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
		e.Captcha.Invalidate()
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

	case stream:
		e.bumpFail(acc)
		web.ReqErr(reqID, fmt.Sprintf("上游串流請求返回非 SSE JSON（帳號 %s）", acc.Name))
		return attemptResult{final: errResult(http.StatusBadGateway, "invalid_upstream_response", "上游未返回有效的 SSE 串流")}
	}

	// 成功（业务码缺失 / 0）：交付缓冲体
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

// deliverStream 交付 SSE 流式响应；usage 随读取同步收集，客户端完整接收后计入。
func (e *Engine) deliverStream(reqID string, acc *model.Account, contentType string, resp *http.Response, deliver DeliverFunc) attemptResult {
	e.success(acc)
	usage := NewUsageCollector(strings.Contains(contentType, "text/event-stream"))
	err := deliver(Delivery{
		StatusCode:  resp.StatusCode,
		ContentType: contentType,
		Header:      resp.Header,
		Body:        &teeReader{r: resp.Body, c: usage},
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

// MarkAccount 设置账号状态；status 为 cooling 时按配置写入冷却截止时间。
// 账号不存在时静默返回（可能已被后台删除）。
func MarkAccount(st *store.Store, provider, id, status, errMsg string, now time.Time) {
	st.Update(provider, id, func(acc *model.Account) {
		acc.Status = status
		acc.LastError = &errMsg
		if status == model.StatusCooling {
			until := float64(now.Add(time.Duration(config.CoolingSeconds)*time.Second).UnixNano()) / 1e9
			acc.CoolingUntil = &until
		}
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
	MarkAccount(e.Store, acc.Provider, acc.ID, status, errMsg, e.now())
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
	MarkSuccess(e.Store, acc.Provider, acc.ID, e.now())
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


