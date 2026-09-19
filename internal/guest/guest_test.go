// 访客提交的落库判定测试：核心契约是「实测通过才入池」。
//
// 完整授权链依赖真实 Z.AI OAuth 端点（internal/oauth 的端点是硬编码常量，
// 无法注入假服务器），因此这里直接驱动 finishSubmission——它承载全部
// 判定逻辑：额度校验、真实对话实测、以及通过后才写 Store。
package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/oauth"
	"zcode2api/internal/quota"
	"zcode2api/internal/store"
)

// fakeBilling 计费端点假客户端（额度校验走这里）。
type fakeBilling struct {
	mu     sync.Mutex
	status int
	body   string
}

func (f *fakeBilling) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &http.Response{
		StatusCode: f.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(f.body)),
	}, nil
}

// newTestHandler 构造访客处理器：存储隔离、上游指向假服务器、计费端点用假客户端。
func newTestHandler(t *testing.T, billingStatus int, billingBody string) (*Handler, *store.Store, *httptest.Server) {
	t.Helper()
	oldDB, oldData, oldZai, oldFB, oldBrowser := config.DBPath, config.DataDir,
		config.UpstreamZai, config.UpstreamZaiFallback, config.CaptchaBrowserEnabled
	config.DBPath = filepath.Join(t.TempDir(), "accounts.db")
	config.DataDir = t.TempDir()
	config.CaptchaBrowserEnabled = true // 实测必经验证码链路
	t.Cleanup(func() {
		config.DBPath, config.DataDir, config.UpstreamZai, config.UpstreamZaiFallback = oldDB, oldData, oldZai, oldFB
		config.CaptchaBrowserEnabled = oldBrowser
	})

	st, err := store.New()
	if err != nil {
		t.Fatalf("打开存储失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// 假上游：由各用例通过 up.Config 覆盖应答
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","content":[{"type":"text","text":"ok"}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(upstream.Close)
	config.UpstreamZai = upstream.URL
	config.UpstreamZaiFallback = upstream.URL

	cm := captcha.NewManager()
	// 注入假求解器：JWT 账号的实测必经验证码链路，没有求解器会直接
	// ErrUnavailable，测试就测不到实测本身。同时固定配置源，避免
	// FetchConfig 打真实上游。
	cm.SetSolver(fakeSolver{})
	cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{Enabled: true, Prefix: "no8xfe", Region: "sgp", SceneID: "11xygtvd"}, nil
	})
	qs := quota.NewService(st)
	qs.Client = &fakeBilling{status: billingStatus, body: billingBody}
	eng := gateway.NewEngine(st, cm, nil)
	eng.BusyRetryDelays = nil

	h := New(st, auth.New(st), cm, qs, eng)
	return h, st, upstream
}

// fakeSolver 固定返回一个令牌的假求解器。
type fakeSolver struct{}

func (fakeSolver) Solve(context.Context, captcha.Config) (string, error) {
	return "tok-test", nil
}
func (fakeSolver) Close() error { return nil }

// doFinish 驱动 finishSubmission 并返回状态码与响应体。
func doFinish(t *testing.T, h *Handler, token string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.finishSubmission(context.Background(), rec, &oauth.ExchangeResult{Token: token})
	return rec.Code, rec.Body.String()
}

// 对话实测失败时不得入池。
//
// 这是「先测后存」的核心闸门：OAuth 授权只证明提交者持有该账号，不证明
// 它当下可用（可能已封禁、额度耗尽或地区受限）。实测不过就直接丢弃，
// 不写进账号池拖累轮询。
func TestFinishSubmissionRejectsOnProbeFailure(t *testing.T) {
	h, st, upstream := newTestHandler(t, http.StatusOK, `{"code":0,"data":{"plans":[],"balances":[]}}`)
	// 让对话端点返回 401：额度端点仍正常，模拟「计费可查但对话不可用」
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	code, body := doFinish(t, h, "header.payload.signature")
	if code != http.StatusBadRequest {
		t.Fatalf("实测失败应 400: %d %s", code, body)
	}
	if !strings.Contains(body, "实测未通过") {
		t.Fatalf("错误应说明实测失败: %s", body)
	}
	if n := len(st.ListAccounts(model.ProviderZai)); n != 0 {
		t.Fatalf("实测失败不得入池，实际有 %d 个账号", n)
	}
}

// 两道校验都通过时才入池，且响应不回显任何账号信息。
func TestFinishSubmissionAcceptsOnSuccess(t *testing.T) {
	h, st, _ := newTestHandler(t, http.StatusOK, `{"code":0,"data":{"plans":[],"balances":[]}}`)

	code, body := doFinish(t, h, "header.payload.signature")
	if code != http.StatusOK {
		t.Fatalf("应通过: %d %s", code, body)
	}

	accounts := st.ListAccounts(model.ProviderZai)
	if len(accounts) != 1 {
		t.Fatalf("应恰好入池一个账号: %d", len(accounts))
	}
	if accounts[0].Mode != "jwt" {
		t.Fatalf("OAuth 账号应为 jwt 模式: %s", accounts[0].Mode)
	}

	// 响应不得泄露账号信息（ID/邮箱/额度一律不给）
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("响应应为合法 JSON: %v", err)
	}
	for _, forbidden := range []string{"id", "account", "email", "quota", "accounts"} {
		if _, leaked := resp[forbidden]; leaked {
			t.Fatalf("响应不应包含 %q: %s", forbidden, body)
		}
	}
	if resp["status"] != "accepted" {
		t.Fatalf("状态应为 accepted: %v", resp)
	}
}

// 非 JWT 形态的凭证应被拒（OAuth 兑换出来的必然是 JWT）。
func TestFinishSubmissionRejectsNonJWTCredential(t *testing.T) {
	h, st, _ := newTestHandler(t, http.StatusOK, `{"code":0,"data":{"plans":[],"balances":[]}}`)

	// 不含两个点的串会被 model.Create 判为 apiKey 模式
	code, body := doFinish(t, h, "plain-api-key")
	if code != http.StatusBadGateway {
		t.Fatalf("非 JWT 凭证应 502: %d %s", code, body)
	}
	if n := len(st.ListAccounts(model.ProviderZai)); n != 0 {
		t.Fatalf("异常凭证不得入池: %d", n)
	}
}

// 访客入口默认关闭（邀请码为空），且 info 端点不泄露邀请码本身。
func TestGuestInfoHidesInviteCode(t *testing.T) {
	h, _, _ := newTestHandler(t, http.StatusOK, `{}`)

	rec := httptest.NewRecorder()
	h.handleInfo(rec, httptest.NewRequest(http.MethodGet, "/guest/api/info", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("info 应 200: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "INVITE") {
		t.Fatalf("info 不得回显邀请码: %s", rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["enabled"] != false {
		t.Fatalf("未配邀请码时应为关闭: %v", resp)
	}

	// 配置后应显示开启，但仍不回显邀请码
	if err := h.Auth.SetInviteCode("SECRET-CODE"); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.handleInfo(rec, httptest.NewRequest(http.MethodGet, "/guest/api/info", nil))
	if strings.Contains(rec.Body.String(), "SECRET-CODE") {
		t.Fatalf("info 不得回显邀请码: %s", rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["enabled"] != true {
		t.Fatalf("配置后应显示开启: %v", resp)
	}
}

// ── 人机验证（Cap）─────────────────────────────────────────────────────────
//
// 校验本身由 internal/capverify 单测覆盖；这里验证的是接线：何时跳过、
// 何时拦截、以及访客能看到的响应形态。

// 未配置 Cap 时必须放行：自建实例地址因部署而异，未配置是合法的关闭状态，
// 不能把访客提交整个堵死。
func TestCaptchaSkippedWhenUnconfigured(t *testing.T) {
	h, _, _ := newTestHandler(t, http.StatusOK, `{}`)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/guest/api/start", nil)
	if !h.verifyCaptcha(rec, req) {
		t.Fatalf("未配置时应放行，却返回了 %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("放行时不应写响应体: %s", rec.Body.String())
	}
}

// 配置了 Cap 但请求没带 token → 拒绝。
func TestCaptchaRejectsMissingToken(t *testing.T) {
	h, _, _ := newTestHandler(t, http.StatusOK, `{}`)
	if err := h.Auth.SetCapConfig("https://cap.example.com", "site", "secret"); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/guest/api/start", nil)
	if h.verifyCaptcha(rec, req) {
		t.Fatal("缺少 token 时应拒绝")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
}

// Cap 服务不可达时返回 502 而非 400：这是管理员要查的配置/网络问题，
// 报成「验证未通过」会把排查方向带偏。
func TestCaptchaServiceFailureIsNotBlamedOnVisitor(t *testing.T) {
	h, _, _ := newTestHandler(t, http.StatusOK, `{}`)
	// 指向必然连不上的地址
	if err := h.Auth.SetCapConfig("http://127.0.0.1:1", "k", "secret"); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/guest/api/start", nil)
	req.Header.Set("x-cap-token", "some-token")
	if h.verifyCaptcha(rec, req) {
		t.Fatal("服务不可用时不应放行")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("期望 502，得到 %d: %s", rec.Code, rec.Body.String())
	}
}

// 只填地址不填密钥视为未启用：否则校验会拿空密钥去问 Cap，
// 全部失败而管理员看不出原因。
func TestCaptchaIncompleteConfigStaysDisabled(t *testing.T) {
	h, _, _ := newTestHandler(t, http.StatusOK, `{}`)
	if err := h.Auth.SetCapConfig("https://cap.example.com", "site", ""); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/guest/api/start", nil)
	if !h.verifyCaptcha(rec, req) {
		t.Fatalf("配置不完整应视为未启用而放行，却返回 %d", rec.Code)
	}
}

// info 回显 Cap 地址（widget 需要它取题）但绝不回显 secret——
// secret 泄露等于任何人都能自造 token，绕过整个验证。
func TestGuestInfoExposesEndpointButNotSecret(t *testing.T) {
	h, _, _ := newTestHandler(t, http.StatusOK, `{}`)
	if err := h.Auth.SetCapConfig("https://cap.example.com", "site", "SUPER-SECRET"); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.handleInfo(rec, httptest.NewRequest(http.MethodGet, "/guest/api/info", nil))
	body := rec.Body.String()
	if strings.Contains(body, "SUPER-SECRET") {
		t.Fatalf("info 泄露了 Cap 密钥: %s", body)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["captcha"] != true {
		t.Fatalf("配置完整时应报告需要人机验证: %v", resp)
	}
	if resp["cap_endpoint"] != "https://cap.example.com/site/" {
		t.Fatalf("应回显 endpoint 供 widget 使用: %v", resp)
	}
}

// 未配置时不应出现 cap_endpoint 字段，前端据此判断不渲染 widget。
func TestGuestInfoOmitsEndpointWhenUnconfigured(t *testing.T) {
	h, _, _ := newTestHandler(t, http.StatusOK, `{}`)

	rec := httptest.NewRecorder()
	h.handleInfo(rec, httptest.NewRequest(http.MethodGet, "/guest/api/info", nil))
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if _, ok := resp["cap_endpoint"]; ok {
		t.Fatalf("未配置时不应有 cap_endpoint: %v", resp)
	}
	if resp["captcha"] != false {
		t.Fatalf("未配置时应报告无需人机验证: %v", resp)
	}
}

// ── 重新生成授权链接 ────────────────────────────────────────────────────────
//
// 访客可能想换一个链接（旧链接发错地方、或想换个设备重来）。重新生成不该
// 重复扣当日配额，也不该要求再解一次人机验证——它没有任何上游开销。

// 同一 IP 重复 start：只保留一个会话，旧链接失效。
func TestStartReplacesExistingFlowForSameHost(t *testing.T) {
	h, _, _ := newTestHandler(t, http.StatusOK, `{}`)
	if err := h.Auth.SetInviteCode("CODE"); err != nil {
		t.Fatal(err)
	}

	host := "203.0.113.7:1234"
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/guest/api/start", nil)
	req1.Header.Set("x-invite-code", "CODE")
	req1.RemoteAddr = host
	h.handleStart(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("首次生成应 200: %d %s", rec1.Code, rec1.Body.String())
	}
	var first map[string]string
	_ = json.Unmarshal(rec1.Body.Bytes(), &first)

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/guest/api/start", nil)
	req2.Header.Set("x-invite-code", "CODE")
	req2.RemoteAddr = host
	h.handleStart(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("重新生成应 200: %d %s", rec2.Code, rec2.Body.String())
	}
	var second map[string]string
	_ = json.Unmarshal(rec2.Body.Bytes(), &second)

	if first["flow_id"] == second["flow_id"] {
		t.Fatal("重新生成应产生新的 flow_id")
	}
	// 旧会话必须被移除：留着会让内存随生成次数增长
	h.mu.Lock()
	n := len(h.flows)
	_, oldAlive := h.flows[first["flow_id"]]
	h.mu.Unlock()
	if oldAlive {
		t.Fatal("旧会话应被替换掉")
	}
	if n != 1 {
		t.Fatalf("同一 IP 应只保留一个会话，实际 %d", n)
	}
}

// 重新生成不扣配额：否则访客换几次链接就没额度可提交了。
func TestStartRegenerationDoesNotConsumeQuota(t *testing.T) {
	h, _, _ := newTestHandler(t, http.StatusOK, `{}`)
	if err := h.Auth.SetInviteCode("CODE"); err != nil {
		t.Fatal(err)
	}

	host := "203.0.113.9:1234"
	// 首次生成扣 1 次配额，其余为重新生成。次数取 auth 包的每日上限之上，
	// 若重新生成也扣配额，这里必然撞 429。
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/guest/api/start", nil)
		req.Header.Set("x-invite-code", "CODE")
		req.RemoteAddr = host
		h.handleStart(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次生成应放行（重新生成不扣配额），得到 %d: %s",
				i+1, rec.Code, rec.Body.String())
		}
	}
}

// 不同 IP 各自独立，互不影响。
func TestStartIsolatesFlowsPerHost(t *testing.T) {
	h, _, _ := newTestHandler(t, http.StatusOK, `{}`)
	if err := h.Auth.SetInviteCode("CODE"); err != nil {
		t.Fatal(err)
	}

	for _, host := range []string{"203.0.113.1:1", "203.0.113.2:2"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/guest/api/start", nil)
		req.Header.Set("x-invite-code", "CODE")
		req.RemoteAddr = host
		h.handleStart(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 应放行: %d", host, rec.Code)
		}
	}
	h.mu.Lock()
	n := len(h.flows)
	h.mu.Unlock()
	if n != 2 {
		t.Fatalf("不同来源应各保留一个会话，实际 %d", n)
	}
}

// 放宽重新生成后，配额仍须对「首次生成」生效。
//
// 这是当日额度的实际闸门：complete 会消耗掉会话，下次生成又算首次，
// 所以一个 IP 最多提交 3 次。若这里被绕过，昂贵的实测（真实上游调用 +
// 验证码求解，上限 90s）就能被无限重试。
func TestStartBlockedWhenQuotaExhausted(t *testing.T) {
	h, _, _ := newTestHandler(t, http.StatusOK, `{}`)
	if err := h.Auth.SetInviteCode("CODE"); err != nil {
		t.Fatal(err)
	}

	const host = "198.51.100.50:1234"
	// 直接把该来源的当日额度用尽（等价于已提交 3 次）
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/guest/api/start", nil)
		req.RemoteAddr = host
		if !h.Auth.AllowGuestSubmission(req) {
			t.Fatalf("第 %d 次预扣应放行", i+1)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/guest/api/start", nil)
	req.Header.Set("x-invite-code", "CODE")
	req.RemoteAddr = host
	h.handleStart(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("额度用尽后应 429，得到 %d: %s", rec.Code, rec.Body.String())
	}
}

// 已有会话时不该再看配额：换链接是免费的，额度用尽也不该挡。
func TestStartRegenerationIgnoresExhaustedQuota(t *testing.T) {
	h, _, _ := newTestHandler(t, http.StatusOK, `{}`)
	if err := h.Auth.SetInviteCode("CODE"); err != nil {
		t.Fatal(err)
	}

	const host = "198.51.100.60:1234"
	// 先建一个会话
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/guest/api/start", nil)
	req.Header.Set("x-invite-code", "CODE")
	req.RemoteAddr = host
	h.handleStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("首次生成应放行: %d", rec.Code)
	}

	// 用尽剩余额度
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest(http.MethodPost, "/guest/api/start", nil)
		r.RemoteAddr = host
		h.Auth.AllowGuestSubmission(r)
	}

	// 换链接仍应放行
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/guest/api/start", nil)
	req.Header.Set("x-invite-code", "CODE")
	req.RemoteAddr = host
	h.handleStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("额度用尽后换链接应仍放行，得到 %d: %s", rec.Code, rec.Body.String())
	}
}

// 兑换失败也必须消耗掉登录会话。
//
// ExchangeCode 无论成败都向上游发了一次真实请求。若失败时保留会话，持有邀请码
// 者可在 TTL 内反复提交同一个 flow_id，每次触发一次上游往返——而配额只在 start
// 扣一次（「换链接」按设计不扣），于是本机成了对上游 token 端点的请求放大器。
// 会话在锁内「取出即移除」，并发的两个 complete 也只有一个能拿到。
func TestCompleteConsumesFlowEvenOnExchangeFailure(t *testing.T) {
	h, _, _ := newTestHandler(t, http.StatusOK, `{}`)
	if err := h.Auth.SetInviteCode("CODE"); err != nil {
		t.Fatal(err)
	}

	const host = "203.0.113.99:1234"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/guest/api/start", nil)
	req.Header.Set("x-invite-code", "CODE")
	req.RemoteAddr = host
	h.handleStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start 应成功: %d %s", rec.Code, rec.Body.String())
	}
	var started map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	flowID := started["flow_id"]

	// 用一个格式合法但无法兑换的 code/state 触发失败路径。
	// state 必须与会话匹配，否则会在更早的校验处被拒、测不到兑换本身。
	h.mu.Lock()
	gf := h.flows[flowID]
	h.mu.Unlock()
	if gf == nil {
		t.Fatal("前置条件：会话应存在")
	}
	// 必须构造格式合法的回調地址（含 redirect 參數），否則會在
	// ParseCallbackURL 就被拒、走不到 ExchangeCode 那一步——而只有
	// ExchangeCode 才真正打上游，也只有它需要消耗會話。
	callback := "https://zcode.z.ai/app/oauth/login?redirect=zcode%3A%2F%2Foauth%2Fcallback" +
		"&code=bogus-code&state=" + gf.flow.State

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/guest/api/complete", nil)
	req.Header.Set("x-invite-code", "CODE")
	req.RemoteAddr = host
	body, _ := json.Marshal(map[string]string{"flow_id": flowID, "callback_url": callback})
	req.Body = io.NopCloser(bytes.NewReader(body))
	h.handleComplete(rec, req)

	t.Logf("complete 响应: %d %s", rec.Code, rec.Body.String())

	// 无论兑换成败，会话都不该还在——这是防重放的唯一手段
	h.mu.Lock()
	_, stillThere := h.flows[flowID]
	h.mu.Unlock()
	if stillThere {
		t.Fatalf("兑换失败后会话必须被消耗，否则可无限重放（响应: %d %s）",
			rec.Code, rec.Body.String())
	}
}

// 并发的 complete 只能有一个拿到会话。
//
// 会话若「先取出、检查、后删除」，两个并发请求都能通过检查并各自向上游兑换
// 一次——一次授权换来两次（或更多次）上游往返。改成锁内「取出即移除」后，
// 只有一个请求拿得到会话，其余得到 404。
func TestConcurrentCompleteOnlyOneWins(t *testing.T) {
	h, _, _ := newTestHandler(t, http.StatusOK, `{}`)
	if err := h.Auth.SetInviteCode("CODE"); err != nil {
		t.Fatal(err)
	}

	const host = "203.0.113.77:1234"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/guest/api/start", nil)
	req.Header.Set("x-invite-code", "CODE")
	req.RemoteAddr = host
	h.handleStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start 应成功: %d", rec.Code)
	}
	var started map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	flowID := started["flow_id"]

	h.mu.Lock()
	gf := h.flows[flowID]
	h.mu.Unlock()
	if gf == nil {
		t.Fatal("前置条件：会话应存在")
	}
	callback := "https://zcode.z.ai/app/oauth/login?redirect=zcode%3A%2F%2Foauth%2Fcallback" +
		"&code=bogus&state=" + gf.flow.State

	// 并发发起多个 complete。关键是检测「有多少个请求真正走到了上游」——
	// 只看状态码不够：有竞态时两个请求都能通过检查并各自兑换，只是上游对
	// 重复 code 可能都返回错误，状态码看起来一样。
	//
	// 用一个计数上游调用的假 HTTP 客户端来判定。
	var upstreamCalls int32
	// oauth.ExchangeCode 走 http.DefaultClient，用本地假服务器承接
	oauthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		time.Sleep(50 * time.Millisecond) // 拉长窗口，让并发充分交错
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":2007,"msg":"http error"}`))
	}))
	defer oauthSrv.Close()
	restore := oauth.SetTokenURLForTest(oauthSrv.URL)
	defer restore()

	// 让所有 goroutine 先跑到屏障再同时出发，并用 runtime.Gosched 逼出真正的
	// 并行；否则它们会被逐个调度，测不出竞态。
	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	ready := make(chan struct{}, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/guest/api/complete", nil)
			req.Header.Set("x-invite-code", "CODE")
			req.RemoteAddr = host
			body, _ := json.Marshal(map[string]string{"flow_id": flowID, "callback_url": callback})
			req.Body = io.NopCloser(bytes.NewReader(body))
			ready <- struct{}{} // 报到
			<-start             // 等发令
			h.handleComplete(rec, req)
			codes[idx] = rec.Code
		}(i)
	}
	for i := 0; i < n; i++ {
		<-ready
	}
	close(start)
	wg.Wait()

	// 核心断言：无论并发多少，上游只能被调用一次
	if got := atomic.LoadInt32(&upstreamCalls); got != 1 {
		t.Fatalf("上游应只被调用 1 次，实际 %d 次（状态码 %v）", got, codes)
	}

	// 只有第一个能进入兑换（其状态码由上游决定，非 404）；其余必须是 404
	notFound := 0
	others := 0
	for _, c := range codes {
		if c == http.StatusNotFound {
			notFound++
		} else {
			others++
		}
	}
	if others != 1 {
		t.Fatalf("应恰好一个请求拿到会话，实际 %d 个（状态码 %v）", others, codes)
	}
	if notFound != n-1 {
		t.Fatalf("其余应全部 404，实际 %d/%d（状态码 %v）", notFound, n-1, codes)
	}
}
