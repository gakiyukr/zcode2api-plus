// 访客提交的落库判定测试：核心契约是「实测通过才入池」。
//
// 完整授权链依赖真实 Z.AI OAuth 端点（internal/oauth 的端点是硬编码常量，
// 无法注入假服务器），因此这里直接驱动 finishSubmission——它承载全部
// 判定逻辑：额度校验、真实对话实测、以及通过后才写 Store。
package guest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
