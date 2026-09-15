package server

import (
	_ "embed"
	"net/http"
)

// uiHTML 是免校验模式下根路径返回的图形化解析页。
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

// handleUI 返回内嵌的解析页。
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	// 不缓存：页面随二进制更新，缓存住会让人对着旧界面排查新问题。
	h.Set("Cache-Control", "no-store")
	// 页面是自包含的，明确禁止浏览器去猜类型。
	h.Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(uiHTML)
}
