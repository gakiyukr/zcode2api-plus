// HTTP 层：/v1/messages 与 /v1/models。同时导出供 OpenAI 兼容层复用的
// JSON 写出、鉴权错误、请求头汇合工具（M4）。
// 对应 Python 版 routes/gateway.py 的路由与流式透传部分。
package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"zcode2api/internal/auth"
	"zcode2api/internal/util"
)

// Handler 网关 HTTP 层。
type Handler struct {
	Engine *Engine
	Auth   *auth.Service
}

// Register 在 mux 上注册网关路由（鉴权内建于每个端点）。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/messages", h.handleMessages)
	mux.HandleFunc("GET /v1/models", h.handleModels)
}

func (h *Handler) handleMessages(w http.ResponseWriter, r *http.Request) {
	if e := h.Auth.VerifyGatewayKey(r); e != nil {
		WriteAuthError(w, e)
		return
	}

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "请求体不是合法 JSON", "type": "invalid_request"},
		})
		return
	}

	// 入口整形一次（不含 system 注入；每账号副本内注入，见 engine.tryAccount）
	NormalizeBody(body, false)

	if !ModelAllowed(body["model"]) {
		modelName := AnyToString(body["model"])
		WriteJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"message": fmt.Sprintf("模型 %s 不在可用清單內，僅支持 %s", modelName, strings.Join(AvailableModels, ", ")),
				"type":    "model_not_allowed",
			},
		})
		return
	}

	result := h.Engine.RunMessages(r.Context(), body, IncomingHeaders(r), func(d Delivery) error {
		return passthroughDeliver(w, d)
	})
	if result.Delivered {
		return
	}
	WriteJSON(w, result.Status, result.Body)
}

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if e := h.Auth.VerifyGatewayKey(r); e != nil {
		WriteAuthError(w, e)
		return
	}
	// 双兼容超集（M4）：每项同时带 Anthropic 侧 id/display_name/type 与
	// OpenAI 侧 object/created/owned_by，顶层 object:list
	data := make([]map[string]any, 0, len(AvailableModels))
	for _, id := range AvailableModels {
		data = append(data, map[string]any{
			"id":           id,
			"type":         "model",
			"display_name": id,
			"created_at":   "2025-01-01T00:00:00Z",
			"object":       "model",
			"created":      float64(1735689600), // 2025-01-01T00:00:00Z
			"owned_by":     "zcode2api",
		})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// passthroughDeliver 字节级透传上游响应；SSE 每 chunk flush。
func passthroughDeliver(w http.ResponseWriter, d Delivery) error {
	w.Header().Set("Content-Type", d.ContentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(d.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := d.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// IncomingHeaders 汇合客户端请求头（多值逗号连接，对齐 Python request.headers）。
func IncomingHeaders(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, vs := range r.Header {
		out[k] = strings.Join(vs, ", ")
	}
	return out
}

func WriteJSON(w http.ResponseWriter, status int, body any) {
	// 与 Python JSONResponse 对齐：紧凑序列化、不转义 HTML、无尾部换行
	data, err := util.MarshalJSON(body)
	if err != nil {
		http.Error(w, `{"error":{"message":"响应序列化失败","type":"internal_error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// WriteAuthError 鉴权错误返回 FastAPI 的 {"detail": ...} 形态（与 Python 版一致）。
func WriteAuthError(w http.ResponseWriter, e *auth.AuthError) {
	WriteJSON(w, e.Status, map[string]any{"detail": e.Message})
}

// AnyToString 对应 Python str(v or "")：nil → 空串。
func AnyToString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
