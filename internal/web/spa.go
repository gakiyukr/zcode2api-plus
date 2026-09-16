// 管理后台静态资源托管与 SPA 回落路由。
// 对应 Python 版 routes/pages.py；嵌入产物来自仓库根包（go:embed 目录约束）。
package web

import (
	"encoding/json"
	"io/fs"
	"net/http"

	"zcode2api/internal/config"
)

// SPA 托管前端构建产物。
type SPA struct {
	dist fs.FS // frontend/dist 子树；nil 时页面路由返回 404 提示
}

// NewSPA 构建托管器；dist 为 frontend/dist 对应的文件树。
func NewSPA(dist fs.FS) *SPA {
	return &SPA{dist: dist}
}

// Register 在 mux 上注册页面路由：
// / → 307 /admin，/admin → 307 /admin/dashboard，
// /assets 静态直出，/admin/{path...} 与 /guest 回落 index.html，
// /meta 提供版本信息。/admin/api/* 由 adminapi 更精确的 pattern 接管。
func (s *SPA) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/dashboard", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("GET /admin/{path...}", s.handleIndex)
	// 访客提交页：与后台同一份 SPA 产物，但走独立路由，访客无需后台密钥。
	mux.HandleFunc("GET /guest", s.handleIndex)
	if s.dist != nil {
		if assets, err := fs.Sub(s.dist, "assets"); err == nil {
			mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(assets)))
		}
	}
	mux.HandleFunc("GET /meta", handleMeta)
}

// handleIndex 返回 SPA 入口页；no-store 确保发版后立即取到新 index.html。
func (s *SPA) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(s.dist, "index.html")
	if err != nil {
		http.Error(w, "管理後台尚未建置：缺少 frontend/dist，請先執行 npm run build", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// handleMeta 服务版本信息（后台「关于」展示用）。
func handleMeta(w http.ResponseWriter, r *http.Request) {
	data, err := json.Marshal(map[string]string{"version": config.AppVersion})
	if err != nil {
		http.Error(w, "{}", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}
