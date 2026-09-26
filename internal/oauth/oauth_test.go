// oauth 包单测：回调解析、邮箱提取、state 校验（网络链路不在单测范围）。
package oauth

import (
	"testing"

	"zcode2api/internal/model"
)

func TestParseCallbackURL(t *testing.T) {
	validZcode := "zcode://oauth/callback?code=abc123&state=st1"
	validHTTPS := "https://zcode.z.ai/app/oauth/login?redirect=zcode%3A%2F%2Foauth%2Fcallback&code=xyz&state=st2"

	cases := []struct {
		name    string
		url     string
		code    string
		state   string
		oauthEr string
		wantErr string
	}{
		{"zcode scheme", validZcode, "abc123", "st1", "", ""},
		{"https 完成页", validHTTPS, "xyz", "st2", "", ""},
		{"authCode 别名", "zcode://oauth/callback?authCode=a1&state=s1", "a1", "s1", "", ""},
		{"拒绝授权", "zcode://oauth/callback?error=access_denied", "", "", "access_denied", ""},
		{"空地址", "  ", "", "", "", "回调地址为空或过长"},
		{"非官方 https", "https://evil.com/cb?code=a&state=b", "", "", "", "只接受 ZCode 官方"},
		{"redirect 校验", "https://zcode.z.ai/app/oauth/login?redirect=https%3A%2F%2Fx&code=a&state=b", "", "", "", "ZCode 官方回调目标无效"},
		{"非法 scheme", "http://zcode/oauth/callback?code=a&state=b", "", "", "", "只接受 ZCode 官方"},
		{"缺 state", "zcode://oauth/callback?code=a", "", "", "", "缺少 code/authCode 或 state"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, state, oauthErr, err := ParseCallbackURL(c.url)
			if c.wantErr != "" {
				if err == nil || !contains(err.Error(), c.wantErr) {
					t.Fatalf("期望错误含 %q，得到 %v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if code != c.code || state != c.state || oauthErr != c.oauthEr {
				t.Fatalf("解析不符: code=%q state=%q err=%q", code, state, oauthErr)
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestMatchesState(t *testing.T) {
	f := NewFlow(model.ProviderZai)
	f.State = "abc"
	if !f.MatchesState("abc") {
		t.Fatal("相同 state 应匹配")
	}
	if f.MatchesState("abd") || f.MatchesState("") || f.MatchesState("abc0") {
		t.Fatal("不同/空 state 不应匹配")
	}
}

func TestExtractUserEmail(t *testing.T) {
	email := "user@example.com"
	cases := []struct {
		name  string
		input any
		want  *string
	}{
		{"顶层 email", map[string]any{"email": email}, &email},
		{"嵌套 user_info", map[string]any{"user_info": map[string]any{"email_address": email}}, &email},
		{"列表内", map[string]any{"items": []any{map[string]any{"mail": email}}}, &email},
		{"非邮箱值忽略", map[string]any{"email": "not-an-email"}, nil},
		{"深度超限", map[string]any{"a": map[string]any{"b": map[string]any{"c": map[string]any{"d": map[string]any{"email": email}}}}}, nil},
		{"无邮箱", map[string]any{"name": "abc"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractUserEmail(c.input)
			if (got == nil) != (c.want == nil) {
				t.Fatalf("结果不符: %v", got)
			}
			if got != nil && *got != *c.want {
				t.Fatalf("邮箱不符: %s", *got)
			}
		})
	}
}

func TestExtractJWTEmail(t *testing.T) {
	// {"email":"jwt@example.com"} 的 base64url（无填充）
	payload := "eyJlbWFpbCI6Imp3dEBleGFtcGxlLmNvbSJ9"
	token := "header." + payload + ".sig"
	got := ExtractJWTEmail(token)
	if got == nil || *got != "jwt@example.com" {
		t.Fatalf("JWT 邮箱提取不符: %v", got)
	}
	if ExtractJWTEmail("not-a-jwt") != nil {
		t.Fatal("非 JWT 应返回 nil")
	}
}
