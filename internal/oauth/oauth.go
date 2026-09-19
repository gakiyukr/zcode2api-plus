// Package oauth 移植 Python 版 app/oauth.py 的 Z.AI 浏览器 OAuth 登录链。
// 流程：后台生成授权链接 → 用户在官方页授权后粘贴回调地址 → 解析 code/state
// → 兑换 Coding Plan JWT →（可选）用 access_token 兑换 API Key 回退通道。
package oauth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"zcode2api/internal/util"
)

const (
	authorizeURL          = "https://chat.z.ai/api/oauth/authorize"
	defaultTokenURL       = "https://zcode.z.ai/api/v1/oauth/token"
	clientID              = "client_P8X5CMWmlaRO9gyO-KSqtg"
	registeredRedirectURI = "https://zcode.z.ai/app/oauth/login?redirect=zcode%3A%2F%2Foauth%2Fcallback"
)

// tokenURLOverride 仅供测试替换兑换端点。
//
// 生产路径永远是 defaultTokenURL；测试需要一个可观测的上游（断言兑换被调用
// 几次、返回什么），而真实端点不可注入。用包级变量而非参数：调用方遍布
// gateway/adminapi/guest，改签名会牵动一大片。
var tokenURLOverride string

func tokenURL() string {
	if tokenURLOverride != "" {
		return tokenURLOverride
	}
	return defaultTokenURL
}

// SetTokenURLForTest 替换兑换端点并返回还原函数；仅供测试使用。
func SetTokenURLForTest(url string) func() {
	prev := tokenURLOverride
	tokenURLOverride = url
	return func() { tokenURLOverride = prev }
}

// extractTimeout HTTP 请求超时（对齐 Python httpx timeout=30）。
const exchangeTimeout = 30 * time.Second

// Flow 一次登录会话（对应 Python ZaiAuthFlow）。
type Flow struct {
	RedirectURI string
	FlowID      string
	Nonce       string
	State       string
	CreatedAt   time.Time
}

// NewFlow 创建会话；token_urlsafe(24) 等价 32 字节 base64url 无填充。
func NewFlow() *Flow {
	return &Flow{
		RedirectURI: registeredRedirectURI,
		FlowID:      util.RandomTokenURLSafe(24),
		Nonce:       util.RandomTokenURLSafe(24),
		CreatedAt:   time.Now(),
	}
}

// tokenURLSafe 生成 n 字节随机数据的 base64url 字符串（无填充）。


// Init 构造 state 并返回 (flow_id, authorize_url)。
func (f *Flow) Init() (string, string, error) {
	stateData := map[string]string{
		"nonce":         f.Nonce,
		"app_return_to": f.RedirectURI,
		"redirect_uri":  f.RedirectURI,
	}
	raw, err := json.Marshal(stateData)
	if err != nil {
		return "", "", err
	}
	f.State = base64.RawURLEncoding.EncodeToString(raw)
	query := url.Values{}
	query.Set("redirect_uri", f.RedirectURI)
	query.Set("response_type", "code")
	query.Set("client_id", clientID)
	query.Set("state", f.State)
	return f.FlowID, authorizeURL + "?" + query.Encode(), nil
}

// MatchesState 常数时间比较 state（对齐 secrets.compare_digest）。
func (f *Flow) MatchesState(state string) bool {
	if f.State == "" || state == "" {
		return false
	}
	return subtleEqual(f.State, state)
}

// subtleEqual 常数时间字符串比较。
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// ParseCallbackURL 校验用户粘贴的登录完成页地址，返回 (code, state, error)。
// 语义对齐 Python parse_callback_url：只接受官方 HTTPS 页面或 zcode:// 回调。
func ParseCallbackURL(callbackURL string) (string, string, string, error) {
	callbackURL = strings.TrimSpace(callbackURL)
	if callbackURL == "" || len(callbackURL) > 8192 {
		return "", "", "", errors.New("回调地址为空或过长")
	}
	parsed, err := url.Parse(callbackURL)
	if err != nil {
		return "", "", "", errors.New("回调地址无效")
	}
	query := parsed.Query()
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "https":
		if parsed.Host != "zcode.z.ai" || strings.TrimRight(parsed.Path, "/") != "/app/oauth/login" {
			return "", "", "", errors.New("只接受 ZCode 官方登录完成页地址")
		}
		redirect := firstQuery(query, "redirect")
		if strings.TrimRight(redirect, "/") != "zcode://oauth/callback" {
			return "", "", "", errors.New("ZCode 官方回调目标无效")
		}
	case "zcode":
		if parsed.Host != "oauth" || strings.TrimRight(parsed.Path, "/") != "/callback" {
			return "", "", "", errors.New("ZCode 回调地址无效")
		}
	default:
		return "", "", "", errors.New("只接受 ZCode 官方 HTTPS 或 zcode:// 回调地址")
	}
	code := strings.TrimSpace(firstQuery(query, "code", "authCode"))
	state := strings.TrimSpace(firstQuery(query, "state"))
	oauthErr := strings.TrimSpace(firstQuery(query, "error"))
	if oauthErr == "" && (code == "" || state == "") {
		return "", "", "", errors.New("回调地址缺少 code/authCode 或 state")
	}
	return code, state, oauthErr, nil
}

// firstQuery 取 query 参数首个值（兼容 parse_qs keep_blank_values 语义）。
func firstQuery(v url.Values, keys ...string) string {
	for _, k := range keys {
		if items := v[k]; len(items) > 0 {
			return items[0]
		}
	}
	return ""
}

// exchangeResult 对应 Python exchange_code 的返回形态。
type ExchangeResult struct {
	Token       string         `json:"token"`
	Zai         map[string]any `json:"zai"`
	User        map[string]any `json:"user"`
	Email       *string        `json:"email"`
	AccessToken string         `json:"-"`
}

// ExchangeCode 用回调 code 兑换 Coding Plan JWT 等凭证。
func (f *Flow) ExchangeCode(code, state string) (*ExchangeResult, error) {
	if code == "" {
		return nil, errors.New("OAuth 回调缺少授权码")
	}
	if !f.MatchesState(state) {
		return nil, errors.New("OAuth state 校验失败")
	}
	payload := map[string]string{
		"code":         code,
		"redirect_uri": f.RedirectURI,
		"state":        state,
	}
	body, err := postJSON(tokenURL(), payload)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Code any    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Token string         `json:"token"`
			Zai   map[string]any `json:"zai"`
			User  map[string]any `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, errors.New("Z.AI 凭证交换返回了无效 JSON")
	}
	if !codeIsZero(parsed.Code) {
		msg := strings.TrimSpace(parsed.Msg)
		if msg == "" {
			msg = "Z.AI 凭证交换失败"
		}
		return nil, errors.New(msg)
	}
	jwt := strings.TrimSpace(parsed.Data.Token)
	if jwt == "" {
		return nil, errors.New("Z.AI 凭证响应中不含 Coding Plan Token")
	}
	access := ""
	if parsed.Data.Zai != nil {
		if v, ok := parsed.Data.Zai["access_token"].(string); ok {
			access = strings.TrimSpace(v)
		}
	}
	email := ExtractUserEmail(parsed.Data.User)
	if email == nil {
		email = ExtractJWTEmail(jwt)
	}
	zai := map[string]any{}
	if access != "" {
		zai["access_token"] = access
	}
	user := parsed.Data.User
	if user == nil {
		user = map[string]any{}
	}
	return &ExchangeResult{
		Token:       jwt,
		Zai:         zai,
		User:        user,
		Email:       email,
		AccessToken: access,
	}, nil
}

// codeIsZero 业务码判零（兼容缺失/字符串/数字形态，对齐 Python `!= 0`）。
func codeIsZero(code any) bool {
	switch v := code.(type) {
	case nil:
		return true
	case float64:
		return v == 0
	case string:
		return v == "0"
	default:
		return false
	}
}

// ExchangeAPIKey OAuth access_token → 业务 token → 机构/项目 → API Key
// （对齐 Python exchange_api_key 的完整链路）。
func ExchangeAPIKey(accessToken string) (string, error) {
	client := &http.Client{Timeout: exchangeTimeout}
	bizToken, err := func() (string, error) {
		body, err := postJSON("https://api.z.ai/api/auth/z/login", map[string]string{"token": accessToken})
		if err != nil {
			return "", err
		}
		var parsed struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return "", errors.New("Z.AI 业务凭证响应无效 JSON")
		}
		biz := parsed.Data
		for _, key := range []string{"access_token", "accessToken"} {
			if v, ok := biz[key].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v), nil
			}
		}
		return "", errors.New("返回数据中不含业务凭证")
	}()
	if err != nil {
		return "", err
	}

	info, err := getJSON(client, "https://api.z.ai/api/biz/customer/getCustomerInfo", bizToken)
	if err != nil {
		return "", err
	}
	data, _ := info["data"].(map[string]any)
	orgs, _ := data["organizations"].([]any)
	org := pickNamed(orgs, "organizationName", "默认机构", "organizationId")
	if org == nil {
		return "", errors.New("找不到可用的机构")
	}
	projects, _ := org["projects"].([]any)
	proj := pickNamed(projects, "projectName", "默认项目", "projectId")
	if proj == nil {
		return "", errors.New("找不到可用的项目")
	}
	orgID, _ := org["organizationId"].(string)
	projID, _ := proj["projectId"].(string)
	keyURL := fmt.Sprintf("https://api.z.ai/api/biz/v1/organization/%s/projects/%s/api_keys", orgID, projID)

	keys, err := getJSON(client, keyURL, bizToken)
	if err != nil {
		return "", err
	}
	keyList, _ := keys["data"].([]any)
	keyObj := findNamed(keyList, "zcode-api-key")
	if keyObj == nil {
		created, err := postJSONAuth(client, keyURL, bizToken, map[string]string{"name": "zcode-api-key"})
		if err != nil {
			return "", err
		}
		keyObj, _ = created["data"].(map[string]any)
	}
	apiKey, _ := keyObj["apiKey"].(string)
	if strings.TrimSpace(apiKey) == "" {
		return "", errors.New("获取 API Key 失败")
	}
	copied, err := getJSON(client, keyURL+"/copy/"+apiKey, bizToken)
	if err != nil {
		return "", err
	}
	copyData, _ := copied["data"].(map[string]any)
	secretKey, _ := copyData["secretKey"].(string)
	if strings.TrimSpace(secretKey) == "" {
		return "", errors.New("未能解密 Secret Key")
	}
	return apiKey + "." + secretKey, nil
}

// pickNamed 从列表中取名称含指定关键字的项，否则取首项（对齐 Python next(...) 逻辑）。
func pickNamed(items []any, nameKey, keyword, idKey string) map[string]any {
	var first map[string]any
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := obj[nameKey].(string)
		if strings.Contains(name, keyword) {
			return obj
		}
		if first == nil {
			first = obj
		}
	}
	_ = idKey
	return first
}

// findNamed 按 name 精确匹配查找。
func findNamed(items []any, name string) map[string]any {
	for _, item := range items {
		if obj, ok := item.(map[string]any); ok {
			if n, _ := obj["name"].(string); n == name {
				return obj
			}
		}
	}
	return nil
}

// postJSON POST JSON 并返回响应体（非 2xx 或业务码非 0 视为失败）。
func postJSON(url string, payload any) ([]byte, error) {
	client := &http.Client{Timeout: exchangeTimeout}
	data, _ := json.Marshal(payload)
	res, err := client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("上游请求失败: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %v", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("上游请求失败 (%d): %s", res.StatusCode, util.Truncate(string(body), 200))
	}
	return body, nil
}

// postJSONAuth 带鉴权头的 POST（创建 API Key 用）。
func postJSONAuth(client *http.Client, url, token string, payload any) (map[string]any, error) {
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("上游请求失败: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %v", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("上游请求失败 (%d): %s", res.StatusCode, util.Truncate(string(body), 200))
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, errors.New("上游响应无效 JSON")
	}
	return parsed, nil
}

// getJSON 带鉴权头的 GET。
func getJSON(client *http.Client, url, token string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("上游请求失败: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %v", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("上游请求失败 (%d): %s", res.StatusCode, util.Truncate(string(body), 200))
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, errors.New("上游响应无效 JSON")
	}
	return parsed, nil
}

// truncate 截断错误详情。


// ExtractUserEmail 从 OAuth 用户资料递归取邮箱（对齐 Python extract_user_email）。
func ExtractUserEmail(user any) *string {
	keys := map[string]bool{"email": true, "emailaddress": true, "mail": true, "useremail": true}
	var walk func(value any, depth int) string
	walk = func(value any, depth int) string {
		if depth > 3 {
			return ""
		}
		switch v := value.(type) {
		case map[string]any:
			for key, item := range v {
				normalized := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(key))
				if keys[normalized] {
					if s, ok := item.(string); ok && strings.Contains(s, "@") && strings.TrimSpace(s) != "" {
						return strings.TrimSpace(s)
					}
				}
			}
			for _, item := range v {
				if found := walk(item, depth+1); found != "" {
					return found
				}
			}
		case []any:
			for _, item := range v {
				if found := walk(item, depth+1); found != "" {
					return found
				}
			}
		}
		return ""
	}
	if found := walk(user, 0); found != "" {
		return &found
	}
	return nil
}

// ExtractJWTEmail 用户字段缺失时从 JWT 非验证解析邮箱声明（备援）。
func ExtractJWTEmail(token string) *string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 兼容带填充的 payload
		if padded, perr := base64.URLEncoding.DecodeString(parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4)); perr == nil {
			claimsBytes, err = padded, nil
		}
		if err != nil {
			return nil
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return nil
	}
	return ExtractUserEmail(claims)
}
