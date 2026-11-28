// Token 调度统计：从上游 Anthropic Messages 回应中提取 usage。
// 对应 Python 版 app/usage.py。
//
// 上游回应有两种形态：
//   - SSE（text/event-stream）：message_start 携带输入侧（input / cache），
//     message_delta 的 usage.output_tokens 为累计值（取最大，不可累加）。
//   - JSON：顶层 usage 一次带齐全部字段。
//
// 收集器以「块」为单位餵入，内部缓冲不完整行，对上游分块边界不敏感；
// 仅在回應完整结束（Finish）后计入账号统计，串流中断时 usage 不完整、不计入。
package gateway

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"zcode2api/internal/model"
)

// UsageCollector 收集单次成功回應的 token 用量。
type UsageCollector struct {
	isSSE         bool
	buf           []byte
	input         int
	output        int
	cacheCreation int
	cacheRead     int
}

// NewUsageCollector 创建收集器；isSSE 决定逐行解析还是缓冲到 Finish 一次解析。
func NewUsageCollector(isSSE bool) *UsageCollector {
	return &UsageCollector{isSSE: isSSE}
}

// Feed 餵入一段回應位元組。
func (u *UsageCollector) Feed(chunk []byte) {
	u.buf = append(u.buf, chunk...)
	if !u.isSSE {
		return
	}
	for {
		idx := bytes.IndexByte(u.buf, '\n')
		if idx < 0 {
			break
		}
		line := u.buf[:idx]
		u.buf = u.buf[idx+1:]
		u.FeedLine(string(line))
	}
}

// FeedLine 解析一行 SSE data；供串流分块解析与 async 池逐行解析共用。
func (u *UsageCollector) FeedLine(line string) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payloadText := strings.TrimSpace(line[len("data:"):])
	if payloadText == "" || payloadText == "[DONE]" {
		return
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(payloadText), &payload); err != nil {
		return
	}
	switch payload["type"] {
	case "message_start":
		msg, _ := payload["message"].(map[string]any)
		usage, _ := msg["usage"].(map[string]any)
		u.input = max(u.input, toInt(usage["input_tokens"]))
		u.output = max(u.output, toInt(usage["output_tokens"]))
		u.cacheCreation = max(u.cacheCreation, toInt(usage["cache_creation_input_tokens"]))
		u.cacheRead = max(u.cacheRead, toInt(usage["cache_read_input_tokens"]))
	case "message_delta":
		// output_tokens 为累计值，取最大避免重复累加
		usage, _ := payload["usage"].(map[string]any)
		u.output = max(u.output, toInt(usage["output_tokens"]))
	}
}

// Finish 回應結束後收尾：SSE 模式先刷出未含尾部換行的殘留行，非 SSE 模式解析完整 JSON。
func (u *UsageCollector) Finish() {
	if u.isSSE {
		if len(u.buf) > 0 {
			u.FeedLine(string(u.buf))
		}
		u.buf = nil
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(u.buf, &payload); err == nil {
		usage, _ := payload["usage"].(map[string]any)
		u.input = max(u.input, toInt(usage["input_tokens"]))
		u.output = max(u.output, toInt(usage["output_tokens"]))
		u.cacheCreation = max(u.cacheCreation, toInt(usage["cache_creation_input_tokens"]))
		u.cacheRead = max(u.cacheRead, toInt(usage["cache_read_input_tokens"]))
	}
	u.buf = nil
}

// AsDict 返回累计结果（键与 Account 累计字段约定一致）。
func (u *UsageCollector) AsDict() model.Usage {
	return model.Usage{
		Input:         u.input,
		Output:        u.output,
		CacheCreation: u.cacheCreation,
		CacheRead:     u.cacheRead,
	}
}

// toInt 对应 Python 版 _to_int：int(value or 0) 再由调用方 max(0, ...) 截负。
func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case float32:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		n, err := strconv.ParseInt(x.String(), 10, 64)
		if err != nil {
			return 0
		}
		return int(n)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}
