package server

import (
	_ "embed"
	"net/http"
)

// adminUIPath 是管理面板的路径。
const adminUIPath = "/admin"

// uiHTML 是图形化解析页（根路径）。
//
// 为什么内嵌而不是放静态目录：整个服务是**单文件、零依赖**部署的
// （scratch 镜像里只有一个二进制），多一个需要随镜像分发的目录就多一处
// 部署出错的可能。页面本身也只有几十 KB，编进二进制不构成负担。
//
// 页面遵守三条约束：
//
//  1. **不引用任何外部资源**（没有 CDN、没有字体、没有图标库）——
//     内网/离线环境打开也必须完整可用，且不向第三方泄露访问者的行为；
//  2. **只调用本服务的公开接口**（/v1/health、/v1/platforms、四个解析端点），
//     与 curl 能做到的事情完全一致，页面不引入任何隐藏能力；
//  3. **不把接口返回值当 HTML 插入**（只用 textContent 赋值），
//     标题、作者名这些来自平台的内容不构成 XSS 注入点。
//
//go:embed ui.html
var uiHTML []byte

// adminHTML 是管理面板（/admin）。
//
// 与解析页的相同点：自包含、零外部资源、只用 textContent 写 DOM。
// 不同点在于它**必须**先有一个管理 Key 才有内容可显示：页面本身是公开的壳，
// 打开后要填管理 Key（VIDLINK_ADMIN_KEY 配置的那个固定 Key），
// 之后所有请求都由 api() 自动带上它。这样做的原因很直接——
// 不公开壳子，浏览器就永远拿不到填 Key 的输入框。
//
//go:embed admin.html
var adminHTML []byte

// handleUI 返回内嵌的解析页。
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	serveEmbeddedHTML(w, r, uiHTML)
}

// handleAdminUI 返回内嵌的管理面板。
func (s *Server) handleAdminUI(w http.ResponseWriter, r *http.Request) {
	serveEmbeddedHTML(w, r, adminHTML)
}

// serveEmbeddedHTML 是两页共用的响应写法。
//
// 不缓存是刻意的：页面随二进制更新，缓存住会让人对着旧界面排查新问题。
// 明确 nosniff：页面是自包含的，不该让浏览器去猜类型。
func serveEmbeddedHTML(w http.ResponseWriter, r *http.Request, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}
