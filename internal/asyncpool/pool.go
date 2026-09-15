// Package asyncpool Async 空闲池路由（POST /async/v1/messages）：
// ticket + SSE keepalive + 等待 ready + 转发响应，仅支持 OAuth (JWT) 账号。
// 对应 Python 版 app/routes/async_pool.py；泄漏防护三件套（SSE 退出释放 +
// 中止后台任务、孤儿 ticket 建票时清扫）与流中断终止语义逐一对齐。
package asyncpool

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/store"
	"zcode2api/internal/upstream"
	"zcode2api/internal/web"
)

// ticketSweepGrace 孤儿 ticket 清扫的宽限期（秒）。
const ticketSweepGrace = 60 * time.Second

// keepaliveInterval SSE 静默期心跳间隔。
const keepaliveInterval = 10 * time.Second

// ticketEvent 后台任务投递给 SSE 流的事件（对齐 Python queue 里的 dict）。
type ticketEvent struct {
	Type string // ready | chunk | done | error
	Data any    // chunk / error 的负载
}

// ticket 单个异步请求的票务状态。
type ticket struct {
	status    string
	body      map[string]any
	queue     chan ticketEvent
	createdAt time.Time
	cancel    context.CancelFunc // 中止后台任务（释放票务时调用）
}

// Pool Async 空闲池：票务存储 + 入口路由 + 后台处理。
type Pool struct {
	Store   *store.Store
	Auth    *auth.Service
	Captcha *captcha.Manager
	Client  *http.Client // nil 时使用与网关一致的上游客户端

	mu      sync.Mutex
	tickets map[string]*ticket
}

// NewPool 创建空闲池。
func NewPool(st *store.Store, au *auth.Service, cm *captcha.Manager) *Pool {
	return &Pool{Store: st, Auth: au, Captcha: cm, tickets: map[string]*ticket{}}
}

// Register 在 mux 上注册异步路由（调用方按 config.AsyncEnabled 决定是否挂载，
// 对齐 Python 的条件 include_router；端点内的 503 检查作为运行期兜底保留）。
func (p *Pool) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /async/v1/messages", p.handleAsyncMessages)
}

// handleAsyncMessages 创建 async ticket 并 SSE 等待结果。
func (p *Pool) handleAsyncMessages(w http.ResponseWriter, r *http.Request) {
	if e := p.Auth.VerifyGatewayKey(r); e != nil {
		writeJSONStatus(w, e.Status, map[string]any{"detail": e.Message})
		return
	}

	if !config.AsyncEnabled {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "Async 路由未启用", "type": "feature_disabled"},
		})
		return
	}

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "请求体不是合法 JSON", "type": "invalid_request"},
		})
		return
	}

	// 模型白名單與 /v1/messages 一致：僅開放清單內模型，其餘在建票前一律拒絕
	if !gateway.ModelAllowed(body["model"]) {
		modelName := anyToString(body["model"])
		web.Warn("async", fmt.Sprintf("模型 %s 不在開放清單內，拒絕建票", modelName))
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"message": fmt.Sprintf("模型 %s 不在可用清單內，僅支持 %s", modelName, strings.Join(gateway.AvailableModels, ", ")),
				"type":    "model_not_allowed",
			},
		})
		return
	}

	ticketID := p.newTicket(body)

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	p.streamTicket(r.Context(), func(s string) error {
		if _, err := io.WriteString(w, s); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}, ticketID)
}

// streamTicket SSE keepalive + 等待结果；退出时（含客户端断开）释放 ticket
// 并中止后台任务。write 负责写出并 flush；返回错误即视为连接已断开。
func (p *Pool) streamTicket(ctx context.Context, write func(string) error, ticketID string) {
	tk := p.getTicket(ticketID)
	if tk == nil {
		_ = write("event: error\ndata: " + sseJSON(map[string]any{"error": "ticket not found"}) + "\n\n")
		return
	}
	defer p.releaseTicket(ticketID)

	send := func(event, data string) error {
		if event == "chunk" {
			return write("data: " + data + "\n\n")
		}
		return write("event: " + event + "\ndata: " + data + "\n\n")
	}

	deadline := tk.createdAt.Add(time.Duration(config.AsyncTicketTimeout) * time.Second)
	if err := send("ticket", sseJSON(map[string]any{"id": ticketID, "status": "pending"})); err != nil {
		return
	}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			// 客户端断开：finally 是唯一必经的清理点（defer releaseTicket）
			return
		case ev, ok := <-tk.queue:
			if !ok {
				return
			}
			switch ev.Type {
			case "ready":
				if err := send("ready", sseJSON(map[string]any{"status": "processing"})); err != nil {
					return
				}
			case "chunk":
				if err := send("chunk", sseJSON(ev.Data)); err != nil {
					return
				}
			case "done":
				_ = send("done", "{}")
				return
			case "error":
				_ = send("error", sseJSON(ev.Data))
				return
			}
		case <-time.After(keepaliveInterval):
			if err := write(": keepalive\n\n"); err != nil {
				return
			}
		}
	}
	// 逾时：必须显式投递终止事件，否则客户端只看到连接关闭而无法区分
	// 「已完成」与「被超时截断」（defer releaseTicket 会中止后台任务）。
	_ = send("error", sseJSON(map[string]any{"error": map[string]any{
		"message": fmt.Sprintf("请求超时（%d 秒未完成）", config.AsyncTicketTimeout),
		"type":    "ticket_timeout",
	}}))
}

// ── 票务生命周期 ────────────────────────────────────────────────────────────

// newTicket 创建 ticket 并启动后台任务，返回 ticket_id。
func (p *Pool) newTicket(body map[string]any) string {
	p.sweepExpiredTickets()
	ticketID := newUUID()
	ctx, cancel := context.WithCancel(context.Background())
	tk := &ticket{
		status:    "pending",
		body:      body,
		queue:     make(chan ticketEvent, 256),
		createdAt: time.Now(),
		cancel:    cancel,
	}
	p.mu.Lock()
	p.tickets[ticketID] = tk
	p.mu.Unlock()
	go p.processTicket(ctx, ticketID)
	return ticketID
}

// getTicket 读取票务（并发安全）。
func (p *Pool) getTicket(ticketID string) *ticket {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tickets[ticketID]
}

// releaseTicket 移除 ticket 并中止仍在运行的后台任务。
// 客户端断开或放弃后继续请求上游只会白耗账号额度，后台任务一并取消；
// 任务已自然结束时 cancel 是无操作（对齐 Python cancel 语义）。
func (p *Pool) releaseTicket(ticketID string) {
	p.mu.Lock()
	tk := p.tickets[ticketID]
	delete(p.tickets, ticketID)
	p.mu.Unlock()
	if tk != nil && tk.cancel != nil {
		tk.cancel()
	}
}

// sweepExpiredTickets 清扫超过生命周期的孤儿 ticket（如客户端在建票后、
// SSE 启动前消失）。正常退出由 streamTicket 的 defer 负责清理；此处按
// 建票时间加宽限兜底。
func (p *Pool) sweepExpiredTickets() {
	cutoff := time.Now().Add(-time.Duration(config.AsyncTicketTimeout)*time.Second - ticketSweepGrace)
	p.mu.Lock()
	var stale []string
	for id, tk := range p.tickets {
		if tk.createdAt.Before(cutoff) {
			stale = append(stale, id)
		}
	}
	p.mu.Unlock()
	for _, id := range stale {
		p.releaseTicket(id)
	}
}

// emit 投递事件；票务已释放或上下文取消时返回 false（调用方应停止工作）。
func (p *Pool) emit(ctx context.Context, ticketID string, ev ticketEvent) bool {
	tk := p.getTicket(ticketID)
	if tk == nil {
		return false
	}
	select {
	case tk.queue <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// ── 后台任务 ────────────────────────────────────────────────────────────────

// processTicket 后台执行 ticket 请求。JWT 走主网关同一套验证码续期；
// 429/503 与网络异常换号重试（指数退避 2^n），已发 chunk 的流中断终止票务。
func (p *Pool) processTicket(ctx context.Context, ticketID string) {
	tk := p.getTicket(ticketID)
	if tk == nil {
		return
	}
	body := tk.body
	retries := 0
	announcedReady := false
	tried := map[string]bool{}

	for {
		modelName, _ := body["model"].(string)
		acc := p.Store.Select(model.ProviderZai, tried, modelName)
		if acc == nil || acc.Mode != "jwt" {
			p.emitError(ctx, ticketID, "无可用 OAuth 账号", "no_account")
			return
		}
		tried[acc.ID] = true

		// 每个账号在副本上注入 zcode_system（NormalizeBody 的 system 注入不幂等）
		actualBody := shallowCopyBody(body)
		gateway.NormalizeBody(actualBody, true)
		payload, err := marshalJSON(actualBody)
		if err != nil {
			p.emitError(ctx, ticketID, "请求体序列化失败", "build_error")
			return
		}

		networkRetry := false
		lastNetworkError := ""
		for attempt := range gateway.MaxCaptchaRetries {
			var verifyParam, verifyRegion string
			token, err := p.Captcha.GetVerifyParam(ctx)
			if err != nil {
				p.Captcha.Invalidate()
				if attempt+1 < gateway.MaxCaptchaRetries {
					web.Warn(ticketID, fmt.Sprintf("验证码自动求解失败，刷新令牌重试（第 %d 次）", attempt+1))
					continue
				}
				p.emitError(ctx, ticketID, fmt.Sprintf("自动验证码暂时失败: %s", err.Error()), "captcha_required")
				return
			}
			if token != nil {
				verifyParam, verifyRegion = token.VerifyParam, token.Region
			}

			req, err := upstream.BuildRequest(acc, verifyParam, verifyRegion, nil)
			if err != nil {
				p.emitError(ctx, ticketID, err.Error(), "build_error")
				return
			}

			if !announcedReady {
				if !p.emit(ctx, ticketID, ticketEvent{Type: "ready"}) {
					return
				}
				announcedReady = true
			}

			midStream, streamErr := p.attemptUpstream(ctx, ticketID, acc, modelName, req, payload)
			if midStream {
				// 已向客户端发出内容块，不能换号重发（会收到重复事件），终止本票
				web.Warn(ticketID, fmt.Sprintf("流转发中断: %s", streamErr.Error()))
				p.emitError(ctx, ticketID, fmt.Sprintf("上游流式响应中断: %s", streamErr.Error()), "upstream_stream_interrupted")
				return
			}
			if streamErr == nil {
				return // 200 流已完整转发并投递 done
			}
			// 上游拒绝验证码：同一账号换令牌重试，次数与求解失败共享内层循环
			//（对齐 Python _is_captcha_error 分支的 continue）
			if errors.Is(streamErr, errCaptchaRejected) {
				if attempt+1 < gateway.MaxCaptchaRetries {
					web.Warn(ticketID, fmt.Sprintf("账号 %s 验证码失效，刷新重试（第 %d 次）", acc.Name, attempt+1))
					continue
				}
				p.emitError(ctx, ticketID, "上游连续拒绝验证码", "captcha_required")
				return
			}
			// 其余错误已作为 error 事件投递给客户端，后台任务直接结束
			if errors.Is(streamErr, errDelivered) {
				return
			}
			if ctx.Err() != nil {
				return // 票务已被释放，无需继续
			}
			web.Warn(ticketID, "请求失败: "+streamErr.Error())
			networkRetry = true
			lastNetworkError = streamErr.Error()
			break
		}

		if !networkRetry {
			// 验证码重试循环耗尽（防御分支，对齐 Python for-else）
			p.emitError(ctx, ticketID, "上游连续拒绝验证码", "captcha_required")
			return
		}

		retries++
		if retries > config.AsyncMaxRetries {
			msg := lastNetworkError
			if msg == "" {
				msg = "重试次数耗尽"
			}
			p.emitError(ctx, ticketID, msg, "max_retries")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(1<<retries) * time.Second):
		}
	}
}

// emitError 投递 error 事件（票务已释放时静默丢弃）。
func (p *Pool) emitError(ctx context.Context, ticketID, message, errType string) {
	p.emit(ctx, ticketID, ticketEvent{
		Type: "error",
		Data: map[string]any{"error": map[string]any{"message": message, "type": errType}},
	})
}

// attemptUpstream 发起一次上游请求并处理响应。
// 返回 (midStream, err)：
//   - 200 流完整转发（done 已投递）→ (false, nil)；
//   - 已发出过 chunk 后流中断 → (true, err)（调用方终止票务，不得重试）；
//   - 一个 chunk 都没发过时按普通网络错误 → (false, err)（调用方换号重试）。
//
// 非 200 的错误分类（验证码 / 429+503 冷却 / 其余原样透传）在此内联处理，
// 与 Python 版 _process_ticket 的分支顺序一致。
func (p *Pool) attemptUpstream(
	ctx context.Context,
	ticketID string,
	acc *model.Account,
	modelName string,
	req upstream.Request,
	payload []byte,
) (bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.URL, strings.NewReader(string(payload)))
	if err != nil {
		return false, err
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := p.clientFor(acc).Do(httpReq)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		text, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return false, readErr
		}
		bodyText := string(text)

		// 分类顺序与 engine.handleUpstreamError 一致（PLAN §5.2），
		// 同一账号在两条路径下必须标出相同状态。
		if gateway.IsCaptchaError(bodyText, resp.StatusCode, resp.Header) {
			// 验证码被拒：令牌作废，由调用方在内层循环内换令牌重试
			p.Captcha.Invalidate()
			return false, errCaptchaRejected
		}

		// 401/403 → 账号失效，换号
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			gateway.MarkAccount(p.Store, acc.Provider, acc.ID, model.StatusInvalid,
				fmt.Sprintf("鉴权失败 HTTP %d", resp.StatusCode), time.Now())
			web.Warn(ticketID, fmt.Sprintf("账号 %s 鉴权失败 %d，切换下一个", acc.Name, resp.StatusCode))
			return false, errNetwork{bodyText}
		}

		// 402 → 该模型额度用完
		if resp.StatusCode == http.StatusPaymentRequired {
			gateway.MarkModelExhausted(p.Store, acc.Provider, acc.ID, modelName,
				fmt.Sprintf("%s 額度已用完", orCurrent(modelName)))
			web.Warn(ticketID, fmt.Sprintf("账号 %s 的 %s 額度用完，切換下一個", acc.Name, orCurrent(modelName)))
			return false, errNetwork{bodyText}
		}

		// 429 且 code=3010：模型并发准入限制，账号仍可用，不标状态
		if gateway.IsModelConcurrencyLimit(resp.StatusCode, bodyText) {
			web.Warn(ticketID, fmt.Sprintf("账号 %s 模型并发准入受限，保留账号状态", acc.Name))
			return false, errNetwork{bodyText}
		}

		// 429：额度上限码族 → 该模型耗尽；其余瞬时限流 → 冷却
		if resp.StatusCode == http.StatusTooManyRequests {
			if gateway.IsQuotaExhaustedCode(bodyText) {
				gateway.MarkModelExhausted(p.Store, acc.Provider, acc.ID, modelName,
					fmt.Sprintf("%s 額度/用量上限已達", orCurrent(modelName)))
				web.Warn(ticketID, fmt.Sprintf("账号 %s 的 %s 觸發用量上限，切換下一個", acc.Name, orCurrent(modelName)))
			} else {
				gateway.MarkAccount(p.Store, acc.Provider, acc.ID, model.StatusCooling, "上游限流 HTTP 429", time.Now())
				web.Warn(ticketID, fmt.Sprintf("账号 %s 被限流 429，切换下一个", acc.Name))
			}
			return false, errNetwork{bodyText}
		}

		// 503 → 冷却换号
		if resp.StatusCode == http.StatusServiceUnavailable {
			p.bumpFail(acc)
			gateway.MarkAccount(p.Store, acc.Provider, acc.ID, model.StatusCooling,
				"上游服務不可用 HTTP 503", time.Now())
			web.Warn(ticketID, fmt.Sprintf("账号 %s 上游返回 503，進入冷卻並切換下一個", acc.Name))
			return false, errNetwork{bodyText}
		}

		// 其余错误：原样回传上游错误体，终止本票
		p.bumpFail(acc)
		p.emit(ctx, ticketID, ticketEvent{
			Type: "error",
			Data: map[string]any{"error": map[string]any{"message": bodyText, "type": "upstream_error"}},
		})
		return false, errDelivered
	}

	// 200 且为 JSON：ZCode 的业务错误有时仍用 HTTP 200，不能当成成功串流。
	// 分类与 engine.handleUpstreamJSON 对齐——同一响应在两条路径下必须
	// 标出相同的账号状态，否则 async 请求会把额度耗尽的账号一直留在池里。
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		buffered, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return false, readErr
		}
		return p.handleUpstreamJSON(ctx, ticketID, acc, modelName, buffered)
	}

	// 200：转发 SSE；转发开始后中断按 chunk 计数区分两种出路
	return p.forwardSSE(ctx, ticketID, resp, acc)
}

// handleUpstreamJSON 处理 HTTP 200 且 content-type 为 JSON 的响应。
// 返回值语义与 attemptUpstream 一致（midStream / err）。
func (p *Pool) handleUpstreamJSON(
	ctx context.Context,
	ticketID string,
	acc *model.Account,
	modelName string,
	buffered []byte,
) (bool, error) {
	text := string(buffered)
	code := gateway.UpstreamBusinessCode(text)

	switch {
	case code == "1005":
		// 每日额度用完：该模型从池中摘除，换号重试
		gateway.MarkModelExhausted(p.Store, acc.Provider, acc.ID, modelName,
			fmt.Sprintf("%s 每日額度已用完", orCurrent(modelName)))
		web.Warn(ticketID, fmt.Sprintf("账号 %s 的 %s 每日額度用完，切換下一個", acc.Name, orCurrent(modelName)))
		return false, errNetwork{text}

	case code == "3007":
		// 验证码被拒：令牌作废，由调用方在内层循环内换令牌重试
		p.Captcha.Invalidate()
		return false, errCaptchaRejected

	case code != "" && code != "0":
		p.bumpFail(acc)
		web.Warn(ticketID, fmt.Sprintf("上游業務錯誤 code=%s（账号 %s）", code, acc.Name))
		p.emit(ctx, ticketID, ticketEvent{
			Type: "error",
			Data: map[string]any{"error": map[string]any{
				"message": gateway.MessageFromJSON(text, buffered),
				"type":    "upstream_error",
				"code":    code,
			}},
		})
		return false, errDelivered
	}

	// 业务码缺失 / 0：JSON 成功响应（async 期望 SSE，但上游给了普通 JSON）
	p.bumpFail(acc)
	web.Warn(ticketID, fmt.Sprintf("上游未返回 SSE 串流（账号 %s）", acc.Name))
	p.emit(ctx, ticketID, ticketEvent{
		Type: "error",
		Data: map[string]any{"error": map[string]any{
			"message": "上游未返回有效的 SSE 串流",
			"type":    "invalid_upstream_response",
		}},
	})
	return false, errDelivered
}

// errCaptchaRejected 上游拒绝验证码：调用方在内层循环内换令牌重试（不换号）。
var errCaptchaRejected = errors.New("上游拒绝验证码")

// orCurrent 模型名为空时的占位文案（与 gateway 同语义）。
func orCurrent(modelName string) string {
	if modelName == "" {
		return "當前模型"
	}
	return modelName
}

// errDelivered 非 200 错误体已作为 error 事件投递给客户端，任务直接结束。
var errDelivered = errors.New("已投递错误事件")

// errNetwork 网络 / 限流类错误：携带上游错误体文本，供 max_retries 消息使用。
type errNetwork struct{ body string }

func (e errNetwork) Error() string { return e.body }

// forwardSSE 把上游 SSE 行转成 ticket chunk 事件，并累计账号 token 用量。
// 完整结束时投递 done 并返回 (false, nil)；转发开始后中断时返回
// (true, err)，零 chunk 时返回 (false, err) 由调用方换号重试。
func (p *Pool) forwardSSE(ctx context.Context, ticketID string, resp *http.Response, acc *model.Account) (bool, error) {
	usage := gateway.NewUsageCollector(true)
	chunksSent := 0
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		usage.FeedLine(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		chunkData := line[len("data: "):]
		if strings.TrimSpace(chunkData) == "[DONE]" {
			continue
		}
		var payload any
		if err := json.Unmarshal([]byte(chunkData), &payload); err != nil {
			continue
		}
		if !p.emit(ctx, ticketID, ticketEvent{Type: "chunk", Data: payload}) {
			return false, ctx.Err()
		}
		chunksSent++
	}
	if err := scanner.Err(); err != nil {
		if chunksSent > 0 {
			return true, err
		}
		return false, err
	}

	// 统计落库失败不应触发换号重发。
	// 除 token 外还要记调用次数/最后使用时间并复位状态——与 engine.success
	// 一致（gateway.MarkSuccess），否则后台用量页漏算 async 流量，
	// 且冷却到期的账号即使这里已成功也仍停在 cooling。
	usage.Finish()
	got := usage.AsDict()
	p.Store.Update(acc.Provider, acc.ID, func(a *model.Account) {
		a.AccumulateTokens(got)
	})
	gateway.MarkSuccess(p.Store, acc.Provider, acc.ID, time.Now())
	p.emit(ctx, ticketID, ticketEvent{Type: "done"})
	return false, nil
}

// bumpFail 累加账号失败计数（锁内修改，供 503 与未知错误分支复用）。
func (p *Pool) bumpFail(acc *model.Account) {
	p.Store.Update(acc.Provider, acc.ID, func(a *model.Account) {
		a.FailCount++
	})
}

// ── 小工具 ──────────────────────────────────────────────────────────────────

// asyncResponseHeaderTimeout 对齐 Python make_async_client(account, timeout=httpx.Timeout(180))：
// 各阶段上限 180s；响应体流式读取（SSE）不能设总超时。
const asyncResponseHeaderTimeout = 180 * time.Second

// clientFor 返回账号的出站客户端。
//
// 与网关一致：账号配置了 proxy_url 时走对应代理。README 与 PLAN §5.9 都承诺
// 「该账号的网关请求、额度查询与套餐领取均走对应代理」——async 曾漏掉这一条，
// 配置代理的账号在这条路径上以服务器真实 IP 直连上游（泄露部署 IP、触发风控）。
// 代理无效时回退直连并记日志，与 engine.clientFor / quota.clientFor 同语义。
func (p *Pool) clientFor(acc *model.Account) *http.Client {
	if p.Client != nil {
		return p.Client
	}
	raw := ""
	if acc != nil && acc.ProxyURL != nil {
		raw = *acc.ProxyURL
	}
	t, err := proxy.TransportForTimeout(raw, asyncResponseHeaderTimeout)
	if err != nil {
		// 仅当账号配了非法代理才会失败；回退直连（TransportForTimeout 对空 URL
		// 永不报错，故此处 t 一定非 nil）。
		web.Warn("async", fmt.Sprintf("账号 %s 代理无效，回退直连: %v", acc.Name, err))
		t, _ = proxy.TransportForTimeout("", asyncResponseHeaderTimeout)
	}
	return &http.Client{Transport: t}
}

// marshalJSON 与网关一致（Python json.dumps(ensure_ascii=False) 形态）：
// 紧凑序列化、不转义 HTML 字符、无尾部换行。
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// sseJSON 对齐 Python json.dumps 默认的 ensure_ascii=True：非 ASCII 字符
// 转义为 \uXXXX（含 UTF-16 代理对），HTML 字符不转义。分隔符沿用 Go 紧凑
// 形态（Python 默认逗号冒号后带空格，SSE 消费方按 JSON 解析无感知）。
func sseJSON(v any) string {
	data, err := marshalJSON(v)
	if err != nil {
		return "{}"
	}
	var b strings.Builder
	for _, r := range string(data) {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case r > 0xFFFF:
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	return b.String()
}

// shallowCopyBody 顶层浅拷贝（对齐 body.copy()：嵌套结构共享引用）。
func shallowCopyBody(body map[string]any) map[string]any {
	out := make(map[string]any, len(body)+1)
	for k, v := range body {
		out[k] = v
	}
	return out
}

// anyToString 对应 Python str(v or "")：nil → 空串。
func anyToString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// newUUID 生成 UUIDv4（不引入第三方依赖）。
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// writeJSONStatus 错误 JSON 响应（与网关 writeJSON 同形态）。
func writeJSONStatus(w http.ResponseWriter, status int, body any) {
	data, err := marshalJSON(body)
	if err != nil {
		http.Error(w, `{"error":{"message":"响应序列化失败","type":"internal_error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
