// 代理线路端点与出口探测。对应 Python 版 admin_api.py 的代理段落：
// CRUD、指派，以及 ip.sb / ipwho.is / ipapi.is 三级备援的出口查询。
package adminapi

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zcode2api/internal/store"
)

const probeUserAgent = "zcode2api-plus/2.0"

func (h *Handler) handleListProxies(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"profiles": h.Store.ListProxyProfiles()})
}

func (h *Handler) handleAddProxy(w http.ResponseWriter, r *http.Request) {
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	profile, err := h.Store.AddProxyProfile(
		strOf(firstTruthy(payload["name"])),
		strOf(firstTruthy(payload["url"])),
		payloadEnabled(payload),
	)
	if err != nil {
		writeAPIError(w, errBadRequest(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

func (h *Handler) handleUpdateProxy(w http.ResponseWriter, r *http.Request) {
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	profile, err := h.Store.UpdateProxyProfile(
		r.PathValue("profile_id"),
		strOf(firstTruthy(payload["name"])),
		strOf(firstTruthy(payload["url"])),
		payloadEnabled(payload),
	)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, errNotFound("代理配置不存在"))
			return
		}
		writeAPIError(w, errBadRequest(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

func (h *Handler) handleDeleteProxy(w http.ResponseWriter, r *http.Request) {
	ok, err := h.Store.DeleteProxyProfile(r.PathValue("profile_id"))
	if err != nil {
		writeError500(w, err)
		return
	}
	if !ok {
		writeAPIError(w, errNotFound("代理配置不存在"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleAssignProxy(w http.ResponseWriter, r *http.Request) {
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	accountID := strings.TrimSpace(strOf(firstTruthy(payload["account_id"])))
	if accountID == "" {
		writeAPIError(w, errBadRequest("缺少帳號 ID"))
		return
	}
	profileID := strOf(firstTruthy(payload["proxy_id"]))
	ok, err := h.Store.AssignProxyProfile(accountID, profileID)
	if err != nil {
		if errors.Is(err, store.ErrProxyNotFound) {
			writeAPIError(w, errBadRequest(err.Error()))
			return
		}
		writeError500(w, err)
		return
	}
	if !ok {
		writeAPIError(w, errNotFound("帳號不存在"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleTestCurrentProxy(w http.ResponseWriter, r *http.Request) {
	h.writeProbe(w, "")
}

func (h *Handler) handleTestProxy(w http.ResponseWriter, r *http.Request) {
	profileID := r.PathValue("profile_id")
	var target string
	for _, p := range h.Store.ListProxyProfiles() {
		if p.ID == profileID {
			target = p.URL
			break
		}
	}
	if target == "" {
		writeAPIError(w, errNotFound("代理配置不存在"))
		return
	}
	h.writeProbe(w, target)
}

func payloadEnabled(payload map[string]any) bool {
	if v, ok := payload["enabled"]; ok {
		return truthy(v)
	}
	return true
}

// writeProbe 执行出口查询并写响应；全部查询服务失败时返回 502（对齐 Python 版）。
func (h *Handler) writeProbe(w http.ResponseWriter, proxyURL string) {
	result, err := h.probe(proxyURL)
	if err != nil {
		writeAPIError(w, &apiError{
			status: http.StatusBadGateway,
			message: fmt.Sprintf(
				"出口查詢失敗（所有 IP 查詢服務均無回應，%s）", errorTypeName(err)),
		})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ipProbeProviders 依次尝试的 IP 查询服务（顺序对齐 Python 版）。
var ipProbeProviders = []struct{ name, endpoint string }{
	{"ip.sb", "https://api.ip.sb/geoip"},
	{"ipwho.is", "https://ipwho.is/"},
	{"ipapi.is", "https://api.ipapi.is/"},
}

// probe 经指定出口查询公网信息；主服务失败时自动切换备援。
func (h *Handler) probe(proxyURL string) (map[string]any, error) {
	client, err := newProbeClient(proxyURL)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	var lastErr error
	for _, p := range ipProbeProviders {
		result, err := fetchIPInfo(client, p.name, p.endpoint)
		if err == nil {
			result["latency_ms"] = time.Since(started).Milliseconds()
			return result, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no probe provider attempted")
	}
	return nil, lastErr
}

// newProbeClient 构造带 12s 超时、跟随重定向的探测客户端，默认透传环境代理。
// Go 标准库支持 http/https/socks5 代理；socks5 拨号即远程解析主机名，
// 与 socks5h 语义一致；socks4 无标准库支持，直接报错（呈 502 形态）。
func newProbeClient(proxyURL string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if strings.TrimSpace(proxyURL) != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, err
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
			transport.Proxy = http.ProxyURL(u)
		case "socks5", "socks5h":
			socks := *u
			socks.Scheme = "socks5"
			transport.Proxy = http.ProxyURL(&socks)
		default:
			return nil, fmt.Errorf("不支持的代理协议: %s", u.Scheme)
		}
	}
	return &http.Client{Timeout: 12 * time.Second, Transport: transport}, nil
}

// probeGet 以探测专用 UA 发起 GET。
func probeGet(client *http.Client, endpoint string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", probeUserAgent)
	return client.Do(req)
}

// fetchIPInfo 查询单个 IP 服务并整理结果；ASN 缺失时向 hackertarget 补查
// （补查失败仍保留 IP 结果，对齐 Python 版）。
func fetchIPInfo(client *http.Client, source, endpoint string) (map[string]any, error) {
	resp, err := probeGet(client, endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 { // raise_for_status
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	result, err := parseIPLookup(payload)
	if err != nil {
		return nil, err
	}
	if ip, _ := result["ip"].(string); result["asn"] == "" && ip != "" {
		if asn, operator, err := lookupASN(client, ip); err == nil {
			result["asn"] = asn
			if result["operator"] == "" {
				result["operator"] = operator
			}
		}
	}
	result["ok"] = true
	result["source"] = source
	return result, nil
}

// parseIPLookup 将不同 IP 查询服务的字段整理成稳定的后台 API 格式
// （对齐 Python _parse_ip_lookup）。
func parseIPLookup(payload map[string]any) (map[string]any, error) {
	connection, _ := payload["connection"].(map[string]any)
	ip := strings.TrimSpace(strOf(firstTruthy(payload["ip"])))
	if ip == "" {
		return nil, errors.New("查詢服務未回傳 IP")
	}
	asn := strings.ToUpper(strings.TrimSpace(strOf(firstTruthy(
		payload["asn"], payload["asn_num"], connection["asn"]))))
	if asn != "" && !strings.HasPrefix(asn, "AS") {
		asn = "AS" + asn
	}
	operator := strings.TrimSpace(strOf(firstTruthy(
		payload["asn_organization"], payload["asn_org"], payload["company_name"],
		payload["organization"], payload["isp"], connection["org"], connection["isp"],
	)))
	return map[string]any{
		"ip":       ip,
		"asn":      asn,
		"operator": operator,
		"country":  strings.TrimSpace(strOf(firstTruthy(payload["country"]))),
		"country_code": strings.ToUpper(strings.TrimSpace(strOf(firstTruthy(
			payload["country_code"], payload["cc"])))),
	}, nil
}

// lookupASN 查询并解析 hackertarget 备援 ASN 服务（单行 CSV 回应）。
func lookupASN(client *http.Client, ip string) (string, string, error) {
	resp, err := probeGet(client, "https://api.hackertarget.com/aslookup/?q="+url.QueryEscape(ip))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	return parseASLookup(string(body))
}

// parseASLookup 对齐 Python _parse_as_lookup：第一行 CSV，row[1]=asn、row[3]=运营方。
func parseASLookup(raw string) (string, string, error) {
	rec, err := csv.NewReader(strings.NewReader(strings.TrimSpace(raw))).Read()
	if err != nil || len(rec) < 2 {
		return "", "", errors.New("ASN 查詢服務回應格式無效")
	}
	asn := strings.ToUpper(strings.TrimSpace(rec[1]))
	if asn == "" {
		return "", "", errors.New("ASN 查詢服務未回傳 ASN")
	}
	if !strings.HasPrefix(asn, "AS") {
		asn = "AS" + asn
	}
	operator := ""
	if len(rec) > 3 {
		operator = strings.TrimSpace(rec[3])
	}
	return asn, operator, nil
}

// errorTypeName 近似 Python 的 type(last_error).__name__：取错误链最深一层的类型名。
func errorTypeName(err error) string {
	for unwrapped := errors.Unwrap(err); unwrapped != nil; {
		err = unwrapped
		unwrapped = errors.Unwrap(err)
	}
	name := fmt.Sprintf("%T", err)
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimPrefix(name, "*")
}
