package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"vidlink/internal/config"
	"vidlink/internal/quota"
)

// readAPIDoc 读仓库里的接口文档（用于"后端→文档"方向的覆盖检查）。
func readAPIDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "API.md"))
	if err != nil {
		t.Fatalf("读 docs/API.md 失败：%v", err)
	}
	return string(b)
}

// 前后端匹配审计。
//
// 这一组测试回答两个方向的问题：
//
//	前端 → 后端：页面里出现的每个 /v1/... 路径，后端真的注册了吗？
//	后端 → 前端/文档：每条路由都至少被页面或文档覆盖了吗？
//
// 之所以要机器来查：这两个方向各自都出过真实事故——
// 「页面调用了一个不存在的端点」与「后端加了端点、页面与文档都没跟上」，
// 症状都是"看起来一切正常，点下去才发现不对"。

// uiPathRe 抓取页面源码里的 /v1/... 字面量（含拼接写法 "…/v1/x/" + id 的前缀）。
var uiPathRe = regexp.MustCompile(`["'` + "`" + `](/v1/[A-Za-z0-9_/{}.\-]*)`)

// uiPathsOf 返回某个页面里出现的路径字面量（去掉查询串与尾斜杠），
// 以及"裸前缀"列表（如 "/v1/"）。
func uiPathsOf(src string) (paths []string, bare []bool) {
	for _, m := range uiPathRe.FindAllStringSubmatch(src, -1) {
		p := strings.SplitN(m[1], "?", 2)[0]
		if p == "/v1" || p == "/v1/" {
			bare = append(bare, true)
			continue
		}
		paths = append(paths, strings.TrimSuffix(p, "/"))
	}
	return paths, bare
}

// auditRoutes 返回"最完整的一套路由"：媒体代理默认是关闭的，但页面确实会用到它，
// 所以审计时要显式打开，否则会把合法调用误判成"后端不存在"。
func auditRoutes(t *testing.T) []routeSpec {
	t.Helper()
	env := newTestEnv(t, func(c *config.Config) {
		c.ProxySrv = config.ProxyOptions{Enabled: true}
	})
	return env.srv.routes()
}

// TestUIPathsExistOnServer：页面引用的路径必须能在路由表里找到对应模板。
//
// 匹配规则允许"字面量是模板的前缀"：面板会把账号句柄拼在后面
// （"/v1/admin/accounts/" + id），字面量本身不构成完整路径。
func TestUIPathsExistOnServer(t *testing.T) {
	var templates []string
	for _, rt := range auditRoutes(t) {
		templates = append(templates, rt.path)
	}
	match := func(lit string) bool {
		for _, tp := range templates {
			if tp == lit || strings.HasPrefix(tp, lit+"/") {
				return true
			}
		}
		return false
	}
	for _, page := range []struct{ name, src string }{
		{"ui.html", string(uiHTML)},
		{"admin.html", string(adminHTML)},
	} {
		paths, _ := uiPathsOf(page.src)
		seen := map[string]bool{}
		for _, p := range paths {
			if seen[p] {
				continue
			}
			seen[p] = true
			if !match(p) {
				t.Errorf("%s 引用了后端不存在的路径：%s", page.name, p)
			}
		}
	}
}

// TestEveryRouteIsCoveredOrExplained：每条路由要么被页面用到、要么在 API 文档里、
// 要么在下面的白名单里写明"只给外部客户端"。没有任何一条可以"谁也不提"。
func TestEveryRouteIsCoveredOrExplained(t *testing.T) {
	ui := string(uiHTML) + "\n" + string(adminHTML)
	doc := readAPIDoc(t)
	uiPaths, _ := uiPathsOf(ui)

	coveredByUI := func(template string) bool {
		for _, p := range uiPaths {
			if template == p || strings.HasPrefix(template, p+"/") || strings.HasPrefix(p, template+"/") {
				return true
			}
		}
		return false
	}
	// 页面故意不用的端点：它们服务的是外部客户端（或运维）。
	backendOnly := map[string]string{
		"/v1/sign":       "页面在本地现算签名，无需这个端点；它是给外部客户端换 URL 签名用的",
		"/v1/version":    "给探针/客户端查版本；页面显示的是健康检查里的版本",
		"/healthz":       "容器存活探针",
		"/readyz":        "负载均衡就绪探针",
		"/v1/admin/sign": "管理面板同样本地现算 adm_ 签名",
	}
	for _, rt := range auditRoutes(t) {
		if _, ok := backendOnly[rt.path]; ok {
			continue
		}
		if coveredByUI(rt.path) {
			continue
		}
		if strings.Contains(doc, rt.path) {
			continue
		}
		t.Errorf("路由 %s 既没被页面使用，也没写进 docs/API.md，也没在“仅后端”白名单里说明", rt.path)
	}
}

// TestUIEndpointColumnsCoverQuotaEndpoints：两个页面里**硬编码**的端点列
// （费率表 / 管理面板倍率网格）必须覆盖全部配额端点。
//
// 这正是加 search 时踩过的坑：后端加了端点，页面少一列，
// 而页面看起来完全正常——只是那一格永远不显示。
func TestUIEndpointColumnsCoverQuotaEndpoints(t *testing.T) {
	for _, page := range []struct{ name, src string }{
		{"ui.html", string(uiHTML)},
		{"admin.html", string(adminHTML)},
	} {
		for _, ep := range quota.AllEndpoints {
			if !strings.Contains(page.src, `"`+string(ep)+`"`) {
				t.Errorf("%s 的端点列表缺少 %q（quota.AllEndpoints 里有）", page.name, ep)
			}
		}
	}
}

// TestAdminPlatformColumnsCoverAllPlatforms：管理面板的两份平台清单
// （列名与中文名）必须覆盖 quota.AllPlatforms + 通用档。
func TestAdminPlatformColumnsCoverAllPlatforms(t *testing.T) {
	src := string(adminHTML)
	pl := append(append([]string{}, platformNames()...), quota.PlatformDefault)
	for _, name := range pl {
		if !strings.Contains(src, `"`+name+`"`) {
			t.Errorf("admin.html 的平台清单缺少 %q", name)
		}
	}
}

// platformNames 把 quota.AllPlatforms 转成字符串。
func platformNames() []string {
	out := make([]string, 0, len(quota.AllPlatforms))
	for _, p := range quota.AllPlatforms {
		out = append(out, string(p))
	}
	return out
}

// TestAdminButtonsAreWired：管理面板里每个按钮的 data-act 都要在 runAction 里有分支。
//
// 加按钮忘写 case 是这类页面最常见的坏法：按钮点下去什么也不发生，控制台也没有报错。
func TestAdminButtonsAreWired(t *testing.T) {
	src := string(adminHTML)
	acts := map[string]bool{}
	// mk("标签", "act") / mk2("标签", "act", "cls")
	for _, m := range regexp.MustCompile(`mk2?\([^,]+,\s*"([a-z_]+)"`).FindAllStringSubmatch(src, -1) {
		acts[m[1]] = true
	}
	if len(acts) < 5 {
		t.Fatalf("只认出 %d 个按钮动作，正则可能失效：%v", len(acts), acts)
	}
	for act := range acts {
		if !strings.Contains(src, `case "`+act+`"`) {
			t.Errorf("管理面板的按钮 %q 没有对应的 case 分支（点了不会有任何反应）", act)
		}
	}
}

// TestUserPageActionsAreWired：解析页的 data-act 要么有显式分支，要么落到 run(act)
// 对应的真实端点（/v1/<act>）。
func TestUserPageActionsAreWired(t *testing.T) {
	src := string(uiHTML)
	var templates []string
	for _, rt := range auditRoutes(t) {
		templates = append(templates, rt.path)
	}
	acts := map[string]bool{}
	for _, m := range regexp.MustCompile(`data-act="([a-z_]+)"`).FindAllStringSubmatch(src, -1) {
		acts[m[1]] = true
	}
	if len(acts) < 3 {
		t.Fatalf("只认出 %d 个 data-act，正则可能失效：%v", len(acts), acts)
	}
	for act := range acts {
		if strings.Contains(src, `act === "`+act+`"`) {
			continue // 显式处理（clear / batch / batchfill）
		}
		if !strings.HasPrefix(act, "batch") && containsTemplate(templates, "/v1/"+act) {
			continue // 走通用的 run(act)
		}
		t.Errorf("解析页的 data-act=%q 既没有显式分支，也没有 /v1/%s 这个端点", act, act)
	}
}

func containsTemplate(templates []string, want string) bool {
	for _, tp := range templates {
		if tp == want {
			return true
		}
	}
	return false
}

// TestResetKeyUIIsWired：重置 Key 这条链路必须在管理面板里真的接上
// （按钮 → 分支 → 正确的端点与请求体 → 明文只显示一次 + 复制）。
func TestResetKeyUIIsWired(t *testing.T) {
	src := string(adminHTML)
	for _, want := range []string{
		`"重置 Key"`, `case "resetkey":`,
		`"/v1/admin/accounts/" + encodeURIComponent(id) + "/reset_key"`,
		`method: "POST"`,
		// 留空 = 随机；手填要先确认（毕竟会让旧 Key 立刻失效）
		`want ? { key: want } : {}`,
		`confirm("确认把 Key 重置为你填写的这个值？`,
		// 明文只出现这一次：给出复制按钮与旧/新句柄
		`复制新 Key`, `old_handle`, `handle`,
		// 公共账号说明 Key 来自配置，不给按钮
		`VIDLINK_PUBLIC_KEY`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("管理面板的重置 Key 接线缺少 %q", want)
		}
	}
}
