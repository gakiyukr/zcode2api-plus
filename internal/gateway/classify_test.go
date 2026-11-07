package gateway

import (
	"net/http"
	"testing"
)

func TestIsCaptchaError(t *testing.T) {
	if !IsCaptchaError("F001", 400, http.Header{}) {
		t.Fatal("F001 + 400 应识别为验证码挑战")
	}
	if !IsCaptchaError(`{"error":"F001"}`, 403, http.Header{}) {
		t.Fatal("F001 + 403 应识别为验证码挑战")
	}
	if IsCaptchaError("F001", 500, http.Header{}) {
		t.Fatal("5xx 的 F001 不应重分类")
	}
	if IsCaptchaError("invalid token", 403, http.Header{}) {
		t.Fatal("非验证码鉴权错误不应被重分类")
	}

	header := http.Header{"X-Aliyun-Captcha-Verify-Param": []string{"challenge"}}
	if !IsCaptchaError("ordinary response", 200, header) {
		t.Fatal("验证码响应头应识别为挑战")
	}
	if !IsCaptchaError(`{"code":3007,"msg":"captcha verify failed"}`, 400, http.Header{}) {
		t.Fatal("code 3007 应识别为挑战")
	}
	if !IsCaptchaError(`{"error":{"code":3007}}`, 400, http.Header{}) {
		t.Fatal("嵌套 error.code=3007 应识别为挑战")
	}
	if !IsCaptchaError("please verify token again", 403, http.Header{}) {
		t.Fatal("verify token 文本应识别为挑战")
	}
	if IsCaptchaError("please verify token again", 500, http.Header{}) {
		t.Fatal("5xx 的验证码文本不应重分类")
	}
}

func TestUpstreamBusinessCode(t *testing.T) {
	if got := UpstreamBusinessCode(`{"code":1310,"msg":"usage cap reached"}`); got != "1310" {
		t.Fatalf("zcode-plan 包装格式不符: %q", got)
	}
	if got := UpstreamBusinessCode(`{"error":{"message":"Authentication Failed","type":"1000"}}`); got != "1000" {
		t.Fatalf("Anthropic 风格不符: %q", got)
	}
	if got := UpstreamBusinessCode("not json"); got != "" {
		t.Fatalf("非 JSON 应返回空: %q", got)
	}
	if got := UpstreamBusinessCode(`{"error":"flat"}`); got != "" {
		t.Fatalf("扁平 error 无码应返回空: %q", got)
	}
	if got := UpstreamBusinessCode(`{}`); got != "" {
		t.Fatalf("空对象应返回空: %q", got)
	}
}

func TestIsModelConcurrencyLimit(t *testing.T) {
	if !IsModelConcurrencyLimit(429, `{"code":3010,"msg":"model admission concurrency limit exceeded"}`) {
		t.Fatal("429+3010 应识别为并发准入限制")
	}
	if !IsModelConcurrencyLimit(429, "model admission concurrency limit exceeded") {
		t.Fatal("文本形态也应识别")
	}
	if IsModelConcurrencyLimit(429, `{"code":3007,"msg":"captcha"}`) {
		t.Fatal("3007 不应误判为 3010")
	}
	if IsModelConcurrencyLimit(500, "model admission concurrency limit exceeded") {
		t.Fatal("非 429 不应识别")
	}
}

func TestModelAllowed(t *testing.T) {
	for _, m := range []any{"glm-5.3-flash", "GLM-5.3", " glm-5.3 "} {
		if !ModelAllowed(m) {
			t.Fatalf("%v 应在开放清单内", m)
		}
	}
	// GLM-5_3 正規化後為 glm-5-3，不还原点号，与 Python 版同语义：不在清单内
	for _, m := range []any{"glm-5.2", "glm-5-turbo", "GLM-5_3", "", "gpt-4o"} {
		if ModelAllowed(m) {
			t.Fatalf("%v 不应在开放清单内", m)
		}
	}
}

func TestZcodeRestricted405(t *testing.T) {
	// 3012 业务 JSON（直打上游的原样形态）
	body := `{"code":3012,"logid":"x","msg":"request has been blocked due to unusual activity."}`
	if !IsZcodeRestricted(http.StatusMethodNotAllowed, body) {
		t.Fatal("405+3012 应判为账号级风控")
	}
	if IsZcodeEdgeBlocked(http.StatusMethodNotAllowed, body) {
		t.Fatal("业务 JSON 不是边缘拦截页")
	}
	// 裸 405（无 body）也按账号级处理：边缘页必带 HTML 特征
	if !IsZcodeRestricted(http.StatusMethodNotAllowed, "") {
		t.Fatal("裸 405 应判为账号级风控")
	}
}

func TestEdgeBlocked405(t *testing.T) {
	page := `<html><body>errors.aliyun.com</body></html>`
	if !IsZcodeEdgeBlocked(http.StatusMethodNotAllowed, page) {
		t.Fatal("HTML 拦截页应判为边缘封锁")
	}
	if IsZcodeRestricted(http.StatusMethodNotAllowed, page) {
		t.Fatal("拦截页不是账号级风控")
	}
	// 非 405 的其他状态码不触发两分类
	rest := `{"code":3012}`
	if IsZcodeEdgeBlocked(http.StatusForbidden, page) || IsZcodeRestricted(http.StatusForbidden, rest) {
		t.Fatal("仅 405 参与风控分类")
	}
}
