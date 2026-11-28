// Package asyncpool Async 空闲池路由（POST /async/v1/messages）：
// ticket + SSE keepalive + 等待 ready + 转发响应，仅支持 OAuth (JWT) 账号。
// 对应 Python 版 app/routes/async_pool.py；泄漏防护三件套（SSE 退出释放 +
// 中止后台任务、孤儿 ticket 建票时清扫）与流中断终止语义逐一对齐。
package asyncpool

import (
	"bufio"
	"context"
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
	"zcode2api/internal/fingerprint"
	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/store"
	"zcode2api/internal/upstream"
	"zcode2api/internal/util"
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
	headers   map[string]string
	queue     chan ticketEvent
	createdAt time.Time
	cancel    context.CancelFunc // 中止后台任务（释放票务时调用）
}

// maxErrorBodyBytes 异常响应的读取上限。
//
// 正常路径是流式转发（边读边发），只有错误分支与「200 但为 JSON」的异常
// 分支要整段读进内存做分类，而上游或代理异常时可能回一个任意大的 body。
const maxErrorBodyBytes = 64 << 10

// Pool Async 空闲池：票务存储 + 入口路由 + 后台处理。
type Pool struct {
	Store   *store.Store
	Auth    *auth.Service
	Captcha *captcha.Manager
	Client  *http.Client // nil 时使用与网关一致的上游客户端
	// Gate Start Plan 雙層併發閘門（生產環境由 main 與 gateway.Engine 共用同一實例）。
	Gate *gateway.StartPlanGate
	// Affinity 快取感知會話親和路由表（生產環境由 main 與 gateway.Engine 共用同一實例）。
	Affinity *gateway.SessionAffinity
	// BusyRetryDelay 遇 3008/3009/3010 同號重試的延遲（零值預設 1s，測試可縮短）。
	BusyRetryDelay time.Duration
	// ReplayDelay 未寫完／複用斷連透明重放延遲（零值預設 gateway.DefaultReplayDelay）。
	ReplayDelay time.Duration
	// UploadStallTimeout 上傳請求體流控停滯看門狗逾時（零值預設 gateway.DefaultUploadStallTimeout）。
	UploadStallTimeout time.Duration

	mu      sync.Mutex
	tickets map[string]*ticket
}

// NewPool 创建空闲池。
func NewPool(st *store.Store, au *auth.Service, cm *captcha.Manager) *Pool {
	return &Pool{
		Store:    st,
		Auth:     au,
		Captcha:  cm,
		Gate:     gateway.NewStartPlanGate(),
		Affinity: gateway.NewSessionAffinity(gateway.DefaultAffinityMaxEntries, gateway.DefaultAffinityTTL),
		tickets:  map[string]*ticket{},
	}
}

// Register 在 mux 上注册异步路由（调用方按 config.AsyncEnabled 决定是否挂载，
// 对齐 Python 的条件 include_router；端点内的 503 检查作为运行期兜底保留）。
func (p *Pool) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /async/v1/messages", p.handleAsyncMessages)
}

// handleAsyncMessages 创建 async ticket 并 SSE 等待结果。
func (p *Pool) handleAsyncMessages(w http.ResponseWriter, r *http.Request) {
	if e := p.Auth.VerifyGatewayKey(r); e != nil {
		gateway.WriteAuthError(w, e)
		return
	}

	if !config.AsyncEnabled {
		gateway.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "Async 路由未启用", "type": "feature_disabled"},
		})
		return
	}

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			gateway.WriteJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"error": map[string]any{"message": "請求體過大", "type": "request_too_large"},
			})
			return
		}
		gateway.WriteJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "請求體不是合法 JSON", "type": "invalid_request_error"},
		})
		return
	}
	if body == nil {
		gateway.WriteJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "請求體不能為空", "type": "invalid_request_error"},
		})
		return
	}

	// 入口整形一次，與 /v1/messages 一致（去掉 provider/ 前綴、套用名稱映射）。
	gateway.NormalizeBody(body, false)

	// 透傳客戶端請求頭（upstream.BuildRequest 內部會依 dropHeaders 剔除鑑權與反代頭）
	ticketID := p.newTicketWithHeaders(body, gateway.IncomingHeaders(r))

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

// newTicket 建立 ticket 並啟動背景任務（無透傳標頭的相容包裝）。
func (p *Pool) newTicket(body map[string]any) string {
	return p.newTicketWithHeaders(body, nil)
}

// newTicketWithHeaders 建立攜帶透傳標頭的 ticket 並啟動背景任務，返回 ticket_id。
func (p *Pool) newTicketWithHeaders(body map[string]any, headers map[string]string) string {
	p.sweepExpiredTickets()
	ticketID := util.NewUUID()
	ctx, cancel := context.WithCancel(context.Background())
	tk := &ticket{
		status:    "pending",
		body:      body,
		headers:   headers,
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
	if modelName, _ := body["model"].(string); modelName != "" {
		if resolved := gateway.ResolveModel(p.Store, modelName, time.Now().Round(0)); resolved != modelName {
			web.Warn(ticketID, fmt.Sprintf("旗艦模型自動相容改寫: %s → %s", modelName, resolved))
			body = shallowCopyBody(body)
			body["model"] = resolved
		}
	}

	lookupKey, bindKey := gateway.DeriveAffinityKeys(body, tk.headers)
	preferredID := ""
	if p.Affinity != nil && lookupKey != "" {
		preferredID = p.Affinity.Lookup(lookupKey, time.Now().Round(0))
	}

	retries := 0
	announcedReady := false
	tried := map[string]bool{}
	edgeBlocked := false
	bare429Count := 0
	var firstBare429Acc *model.Account

	for {
		modelName, _ := body["model"].(string)
		now := time.Now().Round(0)
		var inFlightFn func(string) int
		if p.Gate != nil {
			inFlightFn = p.Gate.AccountInFlight
		}

		// async 僅支援 JWT 帳號，結合會話親和性、6h 臨期優惠與最小在飛選號，並跳過熔斷出口
		acc := p.Store.SelectSmart(model.ProviderZai, tried, modelName, store.SelectOptions{
			Now:         now,
			PreferredID: preferredID,
			InFlight:    inFlightFn,
			Filter:      func(a *model.Account) bool { return a.Mode == "jwt" },
			CanStick: func(cand *model.Account) bool {
				ak := gateway.AddressKeyFor(cand)
				if gateway.EdgeHeldFor(ak, now) {
					return false
				}
				if p.Gate != nil {
					if held, _ := p.Gate.BusyHeld(ak); held {
						return false
					}
					if p.Gate.AccountInFlight(cand.ID) >= p.Gate.AccountCap(cand.ID) {
						return false
					}
				}
				return true
			},
		})
		if acc == nil {
			if edgeBlocked {
				p.emitError(ctx, ticketID, "上游邊緣防火牆已封鎖目前出口，稍後自動恢復", "upstream_edge_blocked")
			} else {
				p.emitError(ctx, ticketID, "无可用 OAuth 账号", "no_account")
			}
			return
		}
		tried[acc.ID] = true
		addrKey := gateway.AddressKeyFor(acc)
		if gateway.EdgeHeldFor(addrKey, now) {
			edgeBlocked = true
			if acc.ID == preferredID && p.Affinity != nil {
				p.Affinity.EvictIfAccount(lookupKey, acc.ID)
			}
			web.Warn(ticketID, fmt.Sprintf("帳號 %s 所屬出口處於邊緣熔斷期，跳過並嘗試其他出口", acc.Name))
			continue
		}

		networkRetry := false
		immediateSwitch := false
		lastNetworkError := ""

		// 單帳號外層處理 Start Plan 閘門與自適應收窄隊首重排（對齊 engine.tryAccount），
		// 避免單帳號池收窄時直接換號誤報 no_account。
		for requeueCount := 0; requeueCount <= gateway.MaxGateRequeue; requeueCount++ {
			var lease *gateway.GateLease
			if p.Gate != nil {
				var err error
				lease, err = p.Gate.Acquire(ctx, acc.ID, addrKey, requeueCount > 0)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					var holdErr *gateway.BusyHoldError
					if errors.As(err, &holdErr) {
						p.emitError(ctx, ticketID, holdErr.Error(), "start_plan_busy_hold")
						return
					}
					p.emitError(ctx, ticketID, err.Error(), "rate_limit_error")
					return
				}
			}

			requeueGate := false
			captchaAttempt := 0
			busyAttempt := 0
			forceCaptcha := false
			for captchaAttempt < gateway.MaxCaptchaRetries {
				var verifyParam, verifyRegion string
				token, err := p.Captcha.GetVerifyParamForModel(ctx, addrKey, forceCaptcha)
				if err != nil {
					// 客戶端斷線或票務釋放時不得清空全域驗證碼快取
					if ctx.Err() != nil {
						if lease != nil {
							lease.Release()
						}
						return
					}
					p.Captcha.InvalidateForAddr(addrKey)
					captchaAttempt++
					if captchaAttempt < gateway.MaxCaptchaRetries {
						web.Warn(ticketID, fmt.Sprintf("验证码自动求解失败，刷新令牌重试（第 %d 次）", captchaAttempt))
						continue
					}
					if lease != nil {
						lease.Release()
					}
					p.emitError(ctx, ticketID, fmt.Sprintf("自动验证码暂时失败: %s", err.Error()), "captcha_required")
					return
				}
				if token != nil {
					verifyParam, verifyRegion = token.VerifyParam, token.Region
				}
				skippedCaptcha := verifyParam == ""

				req, err := upstream.BuildRequest(acc, verifyParam, verifyRegion, tk.headers)
				if err != nil {
					if lease != nil {
						lease.Release()
					}
					p.emitError(ctx, ticketID, err.Error(), "build_error")
					return
				}

				// 每個帳號在副本上進行 ToolSearch 清洗、QueryID 計算與 system 注入；
				// async 路由固定以 SSE 串流轉發，必須強制設 stream: true
				actualBody := shallowCopyBody(body)
				actualBody["stream"] = true
				toolSearchRenamed := gateway.RewriteToolSearchRequest(actualBody)
				upstream.SetQueryID(&req, actualBody)
				gateway.NormalizeBodyForSKU(actualBody, true, upstream.AccountDeviceMid(acc), req.Headers["x-session-id"], fingerprint.For(acc))
				payload, err := util.MarshalJSON(actualBody)
				if err != nil {
					if lease != nil {
						lease.Release()
					}
					p.emitError(ctx, ticketID, "请求体序列化失败", "build_error")
					return
				}

				if !announcedReady {
					if !p.emit(ctx, ticketID, ticketEvent{Type: "ready"}) {
						if lease != nil {
							lease.Release()
						}
						return
					}
					announcedReady = true
				}

				midStream, streamErr := p.attemptUpstream(ctx, ticketID, acc, addrKey, modelName, busyAttempt, requeueCount, toolSearchRenamed, skippedCaptcha, req, payload)
				if midStream {
					if lease != nil {
						lease.Release()
					}
					// 已向客户端发出内容块，不能换号重发（会收到重复事件），终止本票
					web.Warn(ticketID, fmt.Sprintf("流转发中断: %s", streamErr.Error()))
					p.emitError(ctx, ticketID, fmt.Sprintf("上游流式响应中断: %s", streamErr.Error()), "upstream_stream_interrupted")
					return
				}
				if streamErr == nil {
					if lease != nil {
						lease.Release()
					}
					if p.Affinity != nil && bindKey != "" {
						p.Affinity.Bind(bindKey, acc.ID, time.Now().Round(0))
					}
					return // 200 流已完整转发并投递 done
				}
				// 免驗動態回退：上游拒絕無驗證碼請求時強制補驗，不消耗驗證碼重試次數
				if errors.Is(streamErr, errSkippedCaptchaRetry) {
					forceCaptcha = true
					continue
				}
				// 觸發自適應收窄：歸還槽位並排回同帳號隊首重試，不消耗換號次數
				if errors.Is(streamErr, errGateRequeue) {
					requeueGate = true
					break
				}
				// 上游拒絕併發准入（3008/3009/3010）：睡眠前主動歸還閘門槽位，醒來隊首重取，避免佔槽
				if errors.Is(streamErr, errConcurrencyRetry) {
					delay := p.BusyRetryDelay
					if delay <= 0 {
						delay = 1 * time.Second
					}
					web.Warn(ticketID, fmt.Sprintf("帳號 %s 模型併發准入受限，%g s 後同號重試", acc.Name, delay.Seconds()))
					if lease != nil {
						lease.Release()
						lease = nil
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(delay):
					}
					if p.Gate != nil {
						if held, rem := p.Gate.BusyHeld(addrKey); held {
							holdErr := &gateway.BusyHoldError{Remaining: rem}
							p.emitError(ctx, ticketID, holdErr.Error(), "start_plan_busy_hold")
							return
						}
						var acqErr error
						lease, acqErr = p.Gate.Acquire(ctx, acc.ID, addrKey, true)
						if acqErr != nil {
							if ctx.Err() != nil {
								return
							}
							var holdErr *gateway.BusyHoldError
							if errors.As(acqErr, &holdErr) {
								p.emitError(ctx, ticketID, holdErr.Error(), "start_plan_busy_hold")
								return
							}
							p.emitError(ctx, ticketID, acqErr.Error(), "rate_limit_error")
							return
						}
					}
					busyAttempt++
					continue
				}
				// 上游拒绝验证码：同一账号换令牌重试，次数与求解失败共享内层循环
				//（对齐 Python _is_captcha_error 分支的 continue）
				if errors.Is(streamErr, errCaptchaRejected) {
					captchaAttempt++
					if captchaAttempt < gateway.MaxCaptchaRetries {
						web.Warn(ticketID, fmt.Sprintf("账号 %s 验证码失效，刷新重试（第 %d 次）", acc.Name, captchaAttempt))
						continue
					}
					if lease != nil {
						lease.Release()
					}
					p.emitError(ctx, ticketID, "上游连续拒绝验证码", "captcha_required")
					return
				}
				// 邊緣 405：僅熔斷當前出口，立即切換其他代理出口帳號（不耗換號重試與退避）
				if errors.Is(streamErr, errEdgeBlocked) {
					edgeBlocked = true
					bare429Count = 0
					firstBare429Acc = nil
					if acc.ID == preferredID && p.Affinity != nil {
						p.Affinity.EvictIfAccount(lookupKey, acc.ID)
					}
					immediateSwitch = true
					break
				}
				// 請求級裸 429 防雪崩：第 1 號暫標探測冷卻立即換號探測；連續第 2 號仍裸 429 則僅平反探測冷卻並立即止損
				var bareErr errBare429
				if errors.As(streamErr, &bareErr) {
					bare429Count++
					if acc.ID == preferredID && p.Affinity != nil {
						p.Affinity.EvictIfAccount(lookupKey, acc.ID)
					}
					if bare429Count == 1 {
						firstBare429Acc = acc
						gateway.MarkAccount(p.Store, acc.Provider, acc.ID, model.StatusCooling, gateway.Bare429ProbeReason, time.Now().Round(0))
						web.Warn(ticketID, fmt.Sprintf("帳號 %s 收到裸 429，暫標冷卻並換號探測", acc.Name))
						immediateSwitch = true
						break
					}
					if firstBare429Acc != nil {
						p.Store.Update(firstBare429Acc.Provider, firstBare429Acc.ID, func(a *model.Account) {
							if a.Status == model.StatusCooling && a.LastError != nil && *a.LastError == gateway.Bare429ProbeReason {
								a.Status = model.StatusActive
								a.CoolingUntil = nil
								a.LastError = nil
							}
						})
					}
					if lease != nil {
						lease.Release()
					}
					web.Warn(ticketID, "連續 2 個帳號觸發無業務碼裸 429（疑似請求級限流），已恢復探路帳號並立即止損")
					msg := bareErr.body
					if strings.TrimSpace(msg) == "" {
						msg = gateway.Bare429FallbackMessage
					}
					p.emitError(ctx, ticketID, msg, "upstream_rate_limit")
					return
				}
				// 其余错误已作为 error 事件投递给客户端，后台任务直接结束
				if errors.Is(streamErr, errDelivered) {
					if lease != nil {
						lease.Release()
					}
					return
				}
				if ctx.Err() != nil {
					if lease != nil {
						lease.Release()
					}
					return // 票务已被释放，无需继续
				}
				bare429Count = 0
				firstBare429Acc = nil
				if acc.ID == preferredID && p.Affinity != nil {
					p.Affinity.EvictIfAccount(lookupKey, acc.ID)
				}
				web.Warn(ticketID, "请求失败: "+streamErr.Error())
				networkRetry = true
				lastNetworkError = streamErr.Error()
				break
			}
			if lease != nil {
				lease.Release()
			}
			if requeueGate && requeueCount < gateway.MaxGateRequeue {
				web.Warn(ticketID, fmt.Sprintf("帳號 %s 併發上限已自適應收窄，歸還槽位並排回隊首重試（第 %d 次）", acc.Name, requeueCount+1))
				continue
			}
			break
		}

		if immediateSwitch {
			continue
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
	addrKey string,
	modelName string,
	attempt int,
	requeueCount int,
	restoreToolSearch bool,
	skippedCaptcha bool,
	req upstream.Request,
	payload []byte,
) (bool, error) {
	var (
		resp       *http.Response
		reqCleanup func()
	)
	for netAttempt := 0; netAttempt < 2; netAttempt++ {
		tracedCtx, traceState := gateway.AttachReplayTrace(ctx)
		reqCtx, reqBody, cleanup, stalled := gateway.WrapBodyWithStallGuard(tracedCtx, payload, p.uploadStallTimeout(), traceState)

		httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, req.URL, reqBody)
		if err != nil {
			cleanup()
			return false, err
		}
		httpReq.ContentLength = int64(len(payload))
		for k, v := range req.Headers {
			httpReq.Header.Set(k, v)
		}

		r, doErr := p.clientFor(acc).Do(httpReq)
		if doErr == nil {
			resp = r
			reqCleanup = cleanup
			break
		}
		wasStalled := stalled()
		cleanup()
		if wasStalled {
			p.closeIdleForAccount(acc)
			web.Warn(ticketID, fmt.Sprintf("帳號 %s 上游請求體上傳流控停滯逾時，驅逐閒置連線並換號", acc.Name))
			return false, gateway.ErrUploadStalled
		}
		if ctx.Err() != nil {
			return false, doErr
		}
		if netAttempt == 0 && gateway.ShouldTransparentReplay(ctx, doErr, traceState, false) {
			p.closeIdleForAccount(acc)
			web.Warn(ticketID, fmt.Sprintf("帳號 %s 出站連線未送達即斷開，%dms 後透明重放 1 次", acc.Name, p.replayDelay().Milliseconds()))
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(p.replayDelay()):
				continue
			}
		}
		return false, doErr
	}
	defer reqCleanup()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 限长：错误体要整段读进内存做分类，上游或代理异常时可能回一个
		// 任意大的 body。正常路径是流式转发，不受此限。
		text, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		if readErr != nil {
			return false, readErr
		}
		bodyText := string(text)

		// 分类顺序与 engine.handleUpstreamError 一致（PLAN §5.2），
		// 同一账号在两条路径下必须标出相同状态。
		if gateway.IsCaptchaError(bodyText, resp.StatusCode, resp.Header) {
			if skippedCaptcha {
				// 動態免驗撞見驗證碼挑戰：立即失效免驗開關並回退補驗，不消耗驗證碼重試次數
				p.Captcha.InvalidateModelSkip()
				web.Warn(ticketID, fmt.Sprintf("帳號 %s 動態免驗被上游要求驗證，立即補辦驗證碼重送", acc.Name))
				return false, errSkippedCaptchaRetry
			}
			// 验证码被拒：按出口作廢令牌，由调用方在内层循环内换令牌重试
			p.Captcha.InvalidateForAddr(addrKey)
			return false, errCaptchaRejected
		}

		// 405：先區分邊緣攔截頁（按出口熔斷並換其他出口）與帳號級風控（3012，長冷卻）
		if gateway.IsZcodeEdgeBlocked(resp.StatusCode, bodyText) {
			gateway.HoldEdgeFor(addrKey, time.Duration(config.EdgeHoldSeconds)*time.Second, time.Now().Round(0))
			p.closeIdleForAccount(acc)
			p.bumpFail(acc)
			web.Warn(ticketID, fmt.Sprintf("上游邊緣防火牆攔截出口（%s），熔斷 %d s 並嘗試其他出口", addrKey, config.EdgeHoldSeconds))
			return false, errEdgeBlocked
		}
		if gateway.IsZcodeRestricted(resp.StatusCode, bodyText) {
			p.closeIdleForAccount(acc)
			p.bumpFail(acc)
			gateway.MarkAccountCooling(p.Store, acc.Provider, acc.ID,
				"上游风控 3012 拒绝（HTTP 405）",
				time.Duration(config.RestrictedCooldownSeconds)*time.Second, time.Now().Round(0))
			web.Warn(ticketID, fmt.Sprintf("账号 %s 被上游风控限制（3012），冷却 %d s 后重试", acc.Name, config.RestrictedCooldownSeconds))
			return false, errNetwork{bodyText}
		}

		// 401/403 → 帳號失效並驅逐該出口髒閒置連線，換號
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			p.closeIdleForAccount(acc)
			gateway.MarkAccount(p.Store, acc.Provider, acc.ID, model.StatusInvalid,
				fmt.Sprintf("鉴权失败 HTTP %d", resp.StatusCode), time.Now().Round(0))
			web.Warn(ticketID, fmt.Sprintf("账号 %s 鉴权失败 %d，切换下一个", acc.Name, resp.StatusCode))
			return false, errNetwork{bodyText}
		}

		// 402 → 该模型额度用完
		if resp.StatusCode == http.StatusPaymentRequired {
			gateway.MarkModelExhausted(p.Store, acc.Provider, acc.ID, modelName,
				fmt.Sprintf("%s 額度已用完", gateway.OrCurrent(modelName)))
			web.Warn(ticketID, fmt.Sprintf("账号 %s 的 %s 額度用完，切換下一個", acc.Name, gateway.OrCurrent(modelName)))
			return false, errNetwork{bodyText}
		}

		// 429 且 code∈{3008,3009,3010}：併發准入限制，帳號仍可用；首次同號延遲重試，
		// 二次仍被拒時以 retryAttempt=1 回報閘門觸發收窄隊首重排或忙線熔斷。
		if gateway.IsModelConcurrencyLimit(resp.StatusCode, bodyText) {
			if attempt == 0 {
				return false, errConcurrencyRetry
			}
			if p.Gate != nil {
				requeue, holdTriggered := p.Gate.RecordConcurrencyRefusal(acc.ID, addrKey, 1)
				if holdTriggered {
					_, rem := p.Gate.BusyHeld(addrKey)
					holdErr := &gateway.BusyHoldError{Remaining: rem}
					p.emitError(ctx, ticketID, holdErr.Error(), "start_plan_busy_hold")
					return false, errDelivered
				}
				if requeue && requeueCount < gateway.MaxGateRequeue {
					return false, errGateRequeue
				}
			}
			web.Warn(ticketID, fmt.Sprintf("帳號 %s 模型併發准入持續受限，保留帳號狀態並換號", acc.Name))
			return false, errNetwork{bodyText}
		}

		// 429：額度上限碼族 → 該模型耗盡；裸 429 → 交外層防雪崩止損；其餘瞬時限流 → 冷卻
		if resp.StatusCode == http.StatusTooManyRequests {
			if gateway.IsQuotaExhaustedCode(bodyText) {
				gateway.MarkModelExhausted(p.Store, acc.Provider, acc.ID, modelName,
					fmt.Sprintf("%s 額度/用量上限已達", gateway.OrCurrent(modelName)))
				web.Warn(ticketID, fmt.Sprintf("账号 %s 的 %s 觸發用量上限，切換下一個", acc.Name, gateway.OrCurrent(modelName)))
			} else if gateway.IsBare429(resp.StatusCode, bodyText, resp.Header) {
				return false, errBare429{body: bodyText}
			} else {
				gateway.MarkAccount(p.Store, acc.Provider, acc.ID, model.StatusCooling, "上游限流 HTTP 429", time.Now().Round(0))
				web.Warn(ticketID, fmt.Sprintf("账号 %s 被限流 429，切换下一个", acc.Name))
			}
			return false, errNetwork{bodyText}
		}

		// 503 → 冷却换号
		if resp.StatusCode == http.StatusServiceUnavailable {
			p.bumpFail(acc)
			gateway.MarkAccount(p.Store, acc.Provider, acc.ID, model.StatusCooling,
				"上游服務不可用 HTTP 503", time.Now().Round(0))
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
		// 同样限长：这是「200 但为 JSON」的异常分支，不是正常流式响应。
		buffered, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		if readErr != nil {
			return false, readErr
		}
		return p.handleUpstreamJSON(ctx, ticketID, acc, addrKey, modelName, skippedCaptcha, buffered)
	}

	// 200：转发 SSE；转发开始后中断按 chunk 计数区分两种出路
	return p.forwardSSE(ctx, ticketID, resp, acc, restoreToolSearch)
}

// handleUpstreamJSON 处理 HTTP 200 且 content-type 为 JSON 的响应。
// 返回值语义与 attemptUpstream 一致（midStream / err）。
func (p *Pool) handleUpstreamJSON(
	ctx context.Context,
	ticketID string,
	acc *model.Account,
	addrKey string,
	modelName string,
	skippedCaptcha bool,
	buffered []byte,
) (bool, error) {
	text := string(buffered)
	code := gateway.UpstreamBusinessCode(text)

	switch {
	case code == "1005":
		// 每日额度用完：该模型从池中摘除，换号重试
		gateway.MarkModelExhausted(p.Store, acc.Provider, acc.ID, modelName,
			fmt.Sprintf("%s 每日額度已用完", gateway.OrCurrent(modelName)))
		web.Warn(ticketID, fmt.Sprintf("账号 %s 的 %s 每日額度用完，切換下一個", acc.Name, gateway.OrCurrent(modelName)))
		return false, errNetwork{text}

	case code == "3007":
		if skippedCaptcha {
			p.Captcha.InvalidateModelSkip()
			web.Warn(ticketID, fmt.Sprintf("帳號 %s 動態免驗收到 3007，立即補辦驗證碼重送", acc.Name))
			return false, errSkippedCaptchaRetry
		}
		// 验证码被拒：按出口作廢令牌，由调用方在内层循环内换令牌重试
		p.Captcha.InvalidateForAddr(addrKey)
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

	// 靜默版本淘汰空信封攔截：不記帳號失敗、不換號
	if gateway.IsRefusedEnvelope(buffered) {
		web.Warn(ticketID, fmt.Sprintf("上游返回空信封疑似客戶端版本過舊（帳號 %s），不計帳號失敗並終止請求", acc.Name))
		p.emitError(ctx, ticketID,
			"上游靜默拒絕請求（返回空信封，疑似客戶端版本號過舊，請檢查 ZCODE_CLIENT_VERSION）",
			"upstream_version_refused")
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

// errSkippedCaptchaRetry 動態免驗遇驗證碼挑戰：呼叫端強制補驗重送，不計驗證碼重試次數。
var errSkippedCaptchaRetry = errors.New("免驗被拒需補驗證碼重送")

// errEdgeBlocked 上游邊緣 405 封鎖目前出口：呼叫端立即切換其他代理出口帳號。
var errEdgeBlocked = errors.New("上游邊緣封鎖目前出口")

// errBare429 上游回傳無業務碼裸 429：由外層防雪崩機制決定探測換號或止損。
type errBare429 struct{ body string }

func (e errBare429) Error() string { return e.body }

// errConcurrencyRetry 上游併發准入受限（3008/3009/3010）：首次同號延遲重試。
var errConcurrencyRetry = errors.New("上游併發准入受限重試")

// errGateRequeue 閘門觸發自適應收窄，歸還槽位並排回同帳號隊首重試。
var errGateRequeue = errors.New("閘門隊首重排重試")

// errDelivered 非 200 错误体已作为 error 事件投递给客户端，任务直接结束。
var errDelivered = errors.New("已投递错误事件")

// errNetwork 网络 / 限流类错误：携带上游错误体文本，供 max_retries 消息使用。
type errNetwork struct{ body string }

func (e errNetwork) Error() string { return e.body }

// forwardSSE 把上游 SSE 行转成 ticket chunk 事件，并累计账号 token 用量。
// 完整结束时投递 done 并返回 (false, nil)；转发开始后中断时返回
// (true, err)，零 chunk 时返回 (false, err) 由调用方换号重试。
func (p *Pool) forwardSSE(ctx context.Context, ticketID string, resp *http.Response, acc *model.Account, restoreToolSearch bool) (bool, error) {
	usage := gateway.NewUsageCollector(true)
	chunksSent := 0
	var reader io.Reader = resp.Body
	if restoreToolSearch {
		reader = gateway.RestoreToolSearchStream(reader)
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := scanner.Text()
		usage.FeedLine(line)
		var chunkData string
		if strings.HasPrefix(line, "data: ") {
			chunkData = line[len("data: "):]
		} else if strings.HasPrefix(line, "data:") {
			chunkData = strings.TrimSpace(line[len("data:"):])
		} else {
			continue
		}
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
	_ = p.Store.UpdateStats(acc.Provider, acc.ID, func(a *model.Account) bool {
		a.AccumulateTokens(got)
		return false
	})
	gateway.MarkSuccess(p.Store, acc.Provider, acc.ID, time.Now().Round(0))
	p.emit(ctx, ticketID, ticketEvent{Type: "done"})
	return false, nil
}

// replayDelay 取得未送達／複用斷連透明重放延遲。
func (p *Pool) replayDelay() time.Duration {
	if p.ReplayDelay > 0 {
		return p.ReplayDelay
	}
	return gateway.DefaultReplayDelay
}

// uploadStallTimeout 取得上傳流控停滯看門狗逾時。
func (p *Pool) uploadStallTimeout() time.Duration {
	if p.UploadStallTimeout > 0 {
		return p.UploadStallTimeout
	}
	return gateway.DefaultUploadStallTimeout
}

// closeIdleForAccount 驅逐指定帳號對應出口的髒閒置連線。
func (p *Pool) closeIdleForAccount(acc *model.Account) {
	if p.Client != nil {
		p.Client.CloseIdleConnections()
	}
	raw := ""
	if acc != nil && acc.ProxyURL != nil {
		raw = strings.TrimSpace(*acc.ProxyURL)
	}
	proxy.CloseIdleFor(raw)
}

// bumpFail 累加账号失败计数（锁内修改，供 503 与未知错误分支复用）。
func (p *Pool) bumpFail(acc *model.Account) {
	_ = p.Store.UpdateStats(acc.Provider, acc.ID, func(a *model.Account) bool {
		a.FailCount++
		return false
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

// sseJSON 对齐 Python json.dumps 默认的 ensure_ascii=True：非 ASCII 字符
// 转义为 \uXXXX（含 UTF-16 代理对），HTML 字符不转义。分隔符沿用 Go 紧凑
// 形态（Python 默认逗号冒号后带空格，SSE 消费方按 JSON 解析无感知）。
func sseJSON(v any) string {
	data, err := util.MarshalJSON(v)
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

// newUUID 生成 UUIDv4（不引入第三方依赖）。

// writeJSONStatus 错误 JSON 响应（与网关 writeJSON 同形态）。
