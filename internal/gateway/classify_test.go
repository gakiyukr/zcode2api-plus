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
	for _, code := range []string{"3008", "3009", "3010"} {
		if !IsModelConcurrencyLimit(429, `{"code":`+code+`,"msg":"concurrency limit exceeded"}`) {
			t.Fatalf("429+%s 應識別為併發准入限制", code)
		}
	}
	if !IsModelConcurrencyLimit(429, "model admission concurrency limit exceeded") {
		t.Fatal("文本形态也应识别")
	}
	if !IsModelConcurrencyLimit(429, "user concurrency limit exceeded") {
		t.Fatal("3008 文本形態也應識別")
	}
	if IsModelConcurrencyLimit(429, `{"code":3007,"msg":"captcha"}`) {
		t.Fatal("3007 不应误判为并发限制")
	}
	if IsModelConcurrencyLimit(500, "model admission concurrency limit exceeded") {
		t.Fatal("非 429 不应识别")
	}
}

func TestFlagshipAliasAndModelMap(t *testing.T) {
	if ModelNameMap["glm-5.4"] != "GLM-5.4" {
		t.Fatalf("ModelNameMap 應包含 glm-5.4: %v", ModelNameMap)
	}
	for _, old := range []string{"GLM-5.3", "glm-5.3", "glm-5.2", "glm-5-turbo", "glm-4.7"} {
		if got := FlagshipAlias(old); got != "GLM-5.4" {
			t.Fatalf("%s 應映射到 GLM-5.4，實際 %q", old, got)
		}
	}
	if got := FlagshipAlias("glm-5.3-flash"); got != "" {
		t.Fatalf("Flash 不應被改寫: %q", got)
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

func TestIsRefusedEnvelope(t *testing.T) {
	positives := []string{
		`{"code":0,"logid":"20261128abc","msg":""}`,
		`{"data":null,"success":true,"code":0}`,
		`{"code":0,"data":{},"msg":""}`,
		`{"data":[],"success":true}`,
		`{"success":false,"data":null,"msg":""}`,
	}
	for _, raw := range positives {
		if !IsRefusedEnvelope([]byte(raw)) {
			t.Fatalf("應識別為靜默拒絕空信封: %s", raw)
		}
	}

	negatives := []string{
		`{"id":"msg_1","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"code":1005,"msg":"exceed quota"}`,
		`{"code":0,"data":{"id":"msg_1"},"success":true}`,
		`{"code":0,"answer":"42"}`,
		`{"ok":true}`,
		`{"error":{"message":"bad request"}}`,
		`not json`,
		``,
	}
	for _, raw := range negatives {
		if IsRefusedEnvelope([]byte(raw)) {
			t.Fatalf("不應誤判為空信封: %s", raw)
		}
	}
}

func TestIsBare429(t *testing.T) {
	if !IsBare429(http.StatusTooManyRequests, "", http.Header{}) {
		t.Fatal("空字串 429 應識別為裸 429")
	}
	if !IsBare429(http.StatusTooManyRequests, "Too Many Requests", http.Header{}) {
		t.Fatal("純文字 429 應識別為裸 429")
	}
	if !IsBare429(http.StatusTooManyRequests, `{"message":"rate limit"}`, http.Header{}) {
		t.Fatal("無業務碼 JSON 429 應識別為裸 429")
	}
	if IsBare429(http.StatusTooManyRequests, `{"code":1302,"msg":"rate limited"}`, http.Header{}) {
		t.Fatal("帶業務碼 1302 的 429 不應判為裸 429")
	}
	if IsBare429(http.StatusTooManyRequests, `{"code":1310,"msg":"weekly cap"}`, http.Header{}) {
		t.Fatal("帶業務碼 1310 的 429 不應判為裸 429")
	}
	if IsBare429(http.StatusTooManyRequests, "", http.Header{"Retry-After": []string{"10"}}) {
		t.Fatal("帶 Retry-After 標頭的 429 不應判為裸 429")
	}
	if IsBare429(http.StatusServiceUnavailable, "", http.Header{}) {
		t.Fatal("非 429 狀態碼不應判為裸 429")
	}
}
