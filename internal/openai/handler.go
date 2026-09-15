// HTTP 层：POST /v1/chat/completions。同步走网关引擎（选号/验证码/错误分类
// 全复用），不经 async ticket 池；鉴权与 /v1/messages 相同（fail-closed）。
package openai

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"zcode2api/internal/auth"
	"zcode2api/internal/gateway"
)

// Handler OpenAI 兼容端点的 HTTP 层。
type Handler struct {
	Engine *gateway.Engine
	Auth   *auth.Service
}

// New 创建 OpenAI 兼容层处理器。
func New(engine *gateway.Engine, au *auth.Service) *Handler {
	return &Handler{Engine: engine, Auth: au}
}

// Register 在 mux 上注册 /v1/chat/completions 与 /v1/responses（/v1/models 由
// gateway.Handler 提供双兼容超集）。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", h.handleChatCompletions)
	mux.HandleFunc("POST /v1/responses", h.handleResponses)
}

func (h *Handler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if e := h.Auth.VerifyGatewayKey(r); e != nil {
		gateway.WriteAuthError(w, e)
		return
	}

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON", "invalid_request_error")
		return
	}

	anthropicReq, err := ConvertRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	// 模型白名单校验（与 /v1/messages 一致；模型名映射由引擎 NormalizeBody 完成）
	if !gateway.ModelAllowed(anthropicReq["model"]) {
		writeError(w, http.StatusBadRequest,
			"模型 "+gateway.AnyToString(anthropicReq["model"])+" 不在可用清單內，僅支持 "+
				strings.Join(gateway.AvailableModels, ", "), "invalid_request_error")
		return
	}

	// 入口整形一次（模型映射 + content 桥接；system 注入在每账号副本内）
	gateway.NormalizeBody(anthropicReq, false)

	stream, _ := body["stream"].(bool)
	// include_usage 取自 OpenAI 原始请求（Messages API 无此字段，不转发上游）
	includeUsage := false
	if opts, ok := body["stream_options"].(map[string]any); ok {
		includeUsage, _ = opts["include_usage"].(bool)
	}

	result := h.Engine.RunMessages(r.Context(), anthropicReq, gateway.IncomingHeaders(r),
		func(d gateway.Delivery) error {
			if stream {
				return deliverStream(w, d, includeUsage)
			}
			return deliverJSON(w, d)
		})
	if result.Delivered {
		return
	}
	// 引擎错误体已是 {"error":{message,type,code}} 形态，OpenAI 客户端兼容
	gateway.WriteJSON(w, result.Status, result.Body)
}

// deliverJSON 非流式交付：读上游 Anthropic JSON → 转换为 OpenAI 形态回写。
func deliverJSON(w http.ResponseWriter, d gateway.Delivery) error {
	buffered, err := io.ReadAll(d.Body)
	if err != nil {
		return err
	}
	var payload map[string]any
	if err := json.Unmarshal(buffered, &payload); err != nil {
		writeError(w, http.StatusBadGateway, "上游响应不是合法 JSON", "invalid_upstream_response")
		return nil
	}
	converted := ConvertResponse(payload)
	if converted == nil {
		writeError(w, http.StatusBadGateway, "上游响应缺少消息内容", "invalid_upstream_response")
		return nil
	}
	gateway.WriteJSON(w, http.StatusOK, converted)
	return nil
}

// deliverStream 流式交付：SSE 头 + 逐事件重编码。
func deliverStream(w http.ResponseWriter, d gateway.Delivery, includeUsage bool) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	err := reencodeSSE(d.Body, includeUsage, func(event string) error {
		if _, werr := io.WriteString(w, event); werr != nil {
			return werr
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	// 完整收尾：触发引擎的 usage 统计（对齐 finishDelivery 的完整交付语义）
	_, err = io.Copy(io.Discard, d.Body)
	return err
}

// writeError OpenAI 错误形态（error.message / error.type）。
func writeError(w http.ResponseWriter, status int, msg, errType string) {
	gateway.WriteJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": errType},
	})
}
