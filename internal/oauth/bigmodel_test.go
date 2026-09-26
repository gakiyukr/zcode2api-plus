// BigModel OAuth 链路测试：授权链接拼装与凭证兑换（httptest 上游）。
package oauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"zcode2api/internal/model"
)

func TestBigModelAuthorizeURL(t *testing.T) {
	f := NewFlow(model.ProviderBigModel)
	if _, raw, err := f.Init(); err != nil {
		t.Fatalf("Init 不应报错: %v", err)
	} else {
		if !strings.HasPrefix(raw, "https://bigmodel.cn/login?") {
			t.Fatalf("bigmodel 授权页不符: %s", raw)
		}
		// 用 url.ParseQuery 逐参数断言（顺序无关）
		body := raw[strings.Index(raw, "?")+1:]
		q := parseQuery(t, body)
		if q["appId"] != "zcode" {
			t.Fatalf("appId 不符: %v", q["appId"])
		}
		if q["state"] != f.State {
			t.Fatalf("state 应与 Flow 一致")
		}
		// redirect 在授权链接里被再编码一次（对齐桌面端 URLSearchParams 语义），
		// 单次解码后应还原为 Flow 的 redirect_uri。
		if q["redirect"] != f.RedirectURI {
			t.Fatalf("redirect 解码后应与 Flow.RedirectURI 一致: %v", q["redirect"])
		}
		if !strings.Contains(q["redirect"], "app_version=") {
			t.Fatalf("redirect 应携带 app_version: %v", q["redirect"])
		}
		if _, ok := q["client_id"]; ok {
			t.Fatal("bigmodel 不应带 client_id 参数")
		}
	}
}
func TestZaiAuthorizeURLUnchanged(t *testing.T) {
	f := NewFlow(model.ProviderZai)
	_, raw, err := f.Init()
	if err != nil {
		t.Fatalf("Init 不应报错: %v", err)
	}
	if !strings.HasPrefix(raw, "https://chat.z.ai/api/oauth/authorize?") {
		t.Fatalf("zai 授权页不符: %s", raw)
	}
	q := parseQuery(t, raw[strings.Index(raw, "?")+1:])
	if q["client_id"] != clientID || q["response_type"] != "code" {
		t.Fatalf("zai 参数不符: %v", q)
	}
}

func TestBigModelExchangeCode(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"token":"h.payload.s","` +
			`bigmodel":{"access_token":"ak123","refresh_token":"rt456"}}}`))
	}))
	defer srv.Close()
	restore := SetTokenURLForTest(srv.URL)
	defer restore()

	f := NewFlow(model.ProviderBigModel)
	_, _, err := f.Init()
	if err != nil {
		t.Fatalf("Init 不应报错: %v", err)
	}
	res, err := f.ExchangeCode("code-1", f.State)
	if err != nil {
		t.Fatalf("兑换失败: %v", err)
	}
	if gotBody["provider"] != model.ProviderBigModel {
		t.Fatalf("兑换请求体应携带 provider=bigmodel: %v", gotBody)
	}
	if gotBody["code"] != "code-1" || gotBody["state"] != f.State {
		t.Fatalf("兑换请求体 code/state 不符: %v", gotBody)
	}
	if res.Token != "h.payload.s" || res.AccessToken != "ak123" {
		t.Fatalf("兑换结果不符: %+v", res)
	}
	if res.BigModel["access_token"] != "ak123" || res.BigModel["refresh_token"] != "rt456" {
		t.Fatalf("bigmodel 凭证不符: %v", res.BigModel)
	}
}

func TestZaiExchangeCodeNoProviderField(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"token":"h.p.s","zai":{"access_token":"z1"}}}`))
	}))
	defer srv.Close()
	restore := SetTokenURLForTest(srv.URL)
	defer restore()

	f := NewFlow(model.ProviderZai)
	_, _, _ = f.Init()
	res, err := f.ExchangeCode("code-1", f.State)
	if err != nil {
		t.Fatalf("兑换失败: %v", err)
	}
	if _, ok := gotBody["provider"]; ok {
		t.Fatal("zai 兑换请求体不应携带 provider 字段（对齐既有上游契约）")
	}
	if res.Zai["access_token"] != "z1" {
		t.Fatalf("zai 凭证不符: %v", res.Zai)
	}
}

// parseQuery 测试内的小工具：字符串 query → map（取首值，单次解码）。
func parseQuery(t *testing.T, raw string) map[string]string {
	t.Helper()
	values, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("query 解析失败: %v", err)
	}
	out := map[string]string{}
	for k, v := range values {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}
