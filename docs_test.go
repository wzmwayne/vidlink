package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"vidlink/internal/account"
)

// 文档一致性测试。
//
// 这一组用例锁的是"文档约定"这件容易腐烂的事：接口路径必须是相对路径、
// 示例统一走 $BASE、头部必须给出官方测试地址、不能留有已删除的旧接口。
// 没有它，文档会在几次改动之后悄悄与实现对不上——而文档对不上，
// 用户就会拿错误的路径来打接口。

// readDoc 读仓库里的一个文档文件。
func readDoc(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(".", rel))
	if err != nil {
		t.Fatalf("读不到 %s: %v", rel, err)
	}
	return string(b)
}

// apiDocs 是"讲我们自己的接口"的文档清单。
//
// docs/平台调研报告.md 不在里面：它记录的是**平台侧**接口（B 站/抖音的
// 上游 API），出现别家的域名与绝对地址是正常的。
var apiDocs = []string{
	"README.md",
	"docs/API.md",
	"docs/配额说明-用户版.md",
	"docs/配额倍率表.md",
	"docs/架构设计.md",
}

// TestDocsUseRelativeAPIPaths：接口路径一律相对——示例里不许出现硬编码 host。
//
// 文档里写死 127.0.0.1:8080 的坏处很实际：读者从公网实例复制命令时
// 会直接失败，而"把 $BASE 换成你的地址"这句话只写一次就够了。
func TestDocsUseRelativeAPIPaths(t *testing.T) {
	// RE2 不支持负向断言，所以先抓出所有 "主机 + /v1/"，再逐个判断主机是不是 $BASE
	re := regexp.MustCompile(`https?://([A-Za-z0-9_.:\[\]-]+)/v1/`)
	for _, rel := range apiDocs {
		body := readDoc(t, rel)
		for i, line := range strings.Split(body, "\n") {
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				if m[1] == "$BASE" {
					continue
				}
				t.Errorf("%s:%d 出现了硬编码的接口地址 %q（示例请统一用 $BASE/v1/...）",
					rel, i+1, m[0])
			}
		}
	}
}

// TestAPIDocHeaderHasOfficialTestBase：唯一的人读接口文档必须在**头部**
// 给出官方测试实例地址与 $BASE 用法。
func TestAPIDocHeaderHasOfficialTestBase(t *testing.T) {
	body := readDoc(t, "docs/API.md")
	head := body
	if i := strings.Index(body, "## 1."); i > 0 {
		head = body[:i]
	}
	for _, want := range []string{"https://vl.wzml.cc.cd", "$BASE", "vl_public"} {
		if !strings.Contains(head, want) {
			t.Errorf("docs/API.md 头部缺少 %q", want)
		}
	}
}

// TestExampleScriptsMatchGoldenVector：examples/ 里的示例脚本必须与 Go 侧
// 算出**同一条**签名。
//
// 示例代码是最容易腐烂的东西：它在文档里、没人编译它、也没有测试跑它，
// 一旦签名格式变了，用户照着示例写出来的东西会静默地 403。
// 所以这里真的把三个脚本跑起来，并与 account.SignAt 的产物逐字符比对。
func TestExampleScriptsMatchGoldenVector(t *testing.T) {
	const key = "vl_demo_key"
	const ts = int64(0x68a3e658) // 1755571800
	want := account.SignAt(account.HandlePrefix, key, ts)

	cases := []struct {
		name string
		bin  string
		args []string
	}{
		{"python3", "python3", []string{"examples/sign.py", key, "1755571800"}},
		{"node", "node", []string{"examples/sign.js", key, "1755571800"}},
		{"sh", "sh", []string{"examples/sign.sh", key, "1755571800"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin, err := exec.LookPath(tc.bin)
			if err != nil {
				t.Skipf("环境里没有 %s，跳过", tc.bin)
			}
			out, err := exec.Command(bin, tc.args...).CombinedOutput()
			if err != nil {
				t.Fatalf("运行 %s 失败：%v\n%s", tc.bin, err, out)
			}
			if got := strings.TrimSpace(string(out)); got != want {
				t.Errorf("%s 输出 = %s\n        想要 = %s", tc.name, got, want)
			}
		})
	}

	// URL 拼接形式也要对（示例里都提供了 --url 用法）
	if bin, err := exec.LookPath("python3"); err == nil {
		out, err := exec.Command(bin, "examples/sign.py", key, "1755571800",
			"--url", "/v1/usage?url=x").CombinedOutput()
		if err != nil {
			t.Fatalf("python3 示例 --url 失败：%v\n%s", err, out)
		}
		wantURL := "/v1/usage?url=x&key=" + want
		if got := strings.TrimSpace(string(out)); got != wantURL {
			t.Errorf("示例 URL = %s\n    想要 = %s", got, wantURL)
		}
	}
	// 管理签名用的是同一套数学，只是前缀不同
	if bin, err := exec.LookPath("python3"); err == nil {
		out, err := exec.Command(bin, "examples/sign.py", key, "1755571800",
			"--prefix", "adm_").CombinedOutput()
		if err != nil {
			t.Fatalf("python3 示例 --prefix 失败：%v\n%s", err, out)
		}
		wantAdm := account.SignAt(account.AdminPrefix, key, ts)
		if got := strings.TrimSpace(string(out)); got != wantAdm {
			t.Errorf("管理签名 = %s\n      想要 = %s", got, wantAdm)
		}
	}
}

// TestParseExamplesWork：examples/parse.{py,js,sh} 必须真的能跑通。
//
// 示例代码最大的风险不是"写得不好看"，而是**没人跑它**：签名格式一改、
// 头名一改、字段一改，文档还在、代码却已经 403，而用户会照抄。
// 所以这里起一个"会验签"的桩服务端，把三个示例各跑一遍：
//   - 桩服务端用 account.VerifyCredential 验，只有示例真的签对了才放行；
//   - 响应体是固定的假数据，示例解析出来后必须出现那条假直链。
func TestParseExamplesWork(t *testing.T) {
	const key = "vl_demo_key"
	const directURL = "https://cdn.test/1080.mp4"
	const title = "示例标题"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONStub := func(status int, body map[string]any) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
		}
		// 凭据可以放头（推荐）或 URL（<a>/<video> 形态），这里都收
		cred := r.Header.Get("X-API-Key")
		if cred == "" {
			cred = r.URL.Query().Get("key")
		}
		handle, ts, sig, ok := account.ParseCredential(cred)
		if !ok || handle != account.Handle(key) ||
			account.VerifyCredential(key, handle, sig, ts, time.Now(), account.DefaultTTL) != account.SigOK {
			writeJSONStub(http.StatusForbidden, map[string]any{
				"error": map[string]any{"kind": "signature_invalid", "message": "凭据不对"}})
			return
		}
		switch r.URL.Path {
		case "/v1/usage":
			writeJSONStub(http.StatusOK, map[string]any{
				"name": "示例账号", "quota": 42.5, "used": 7.5, "calls": 3, "multiplier": 1.0})
		case "/v1/info":
			w.Header().Set("X-Quota-Consumed", "0.5")
			w.Header().Set("X-Quota-Remaining", "42")
			writeJSONStub(http.StatusOK, map[string]any{
				"title": title, "author": map[string]any{"name": "UP主"},
				"stats":     map[string]any{"view": 1, "like": 2, "duration": 3},
				"qualities": []any{map[string]any{"label": "1080P", "height": 1080, "quality_id": 80}}})
		case "/v1/links":
			w.Header().Set("X-Quota-Consumed", "1")
			w.Header().Set("X-Quota-Remaining", "41.5")
			// 形状必须与真服务端一致：/v1/links 是**单个 Links 对象**
			// （url / backup_urls / headers / audio_url / needs_mux），
			// 不是 streams 数组——否则示例代码会"测试全绿、真机全空"。
			writeJSONStub(http.StatusOK, map[string]any{
				"url":         directURL,
				"backup_urls": []any{"https://cdn.test/backup.mp4"},
				"headers":     map[string]any{"Referer": "https://www.bilibili.com/"},
				"audio_url":   "https://cdn.test/audio.m4s",
				"needs_mux":   true})
		case "/v1/proxy":
			body := []byte("FAKEMP4")
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		default:
			writeJSONStub(http.StatusNotFound, map[string]any{
				"error": map[string]any{"kind": "not_found", "message": r.URL.Path}})
		}
	}))
	defer srv.Close()

	type probe struct {
		name string
		bin  string
		args func(out string) []string
		want string
	}
	cases := []probe{
		{"python3 links", "python3",
			func(out string) []string {
				return []string{"examples/parse.py", "--base", srv.URL, "--key", key, "--url", "https://example.test/v/1"}
			}, directURL},
		{"python3 info", "python3",
			func(string) []string {
				return []string{"examples/parse.py", "--base", srv.URL, "--key", key,
					"--url", "x", "--endpoint", "info"}
			}, title},
		{"node links", "node",
			func(string) []string {
				return []string{"examples/parse.js", "--base", srv.URL, "--key", key, "--url", "https://example.test/v/1"}
			}, directURL},
		{"node info", "node",
			func(string) []string {
				return []string{"examples/parse.js", "--base", srv.URL, "--key", key,
					"--url", "x", "--endpoint", "info"}
			}, title},
		{"sh links", "sh",
			func(string) []string {
				return []string{"examples/parse.sh", "--base", srv.URL, "--key", key, "--url", "https://example.test/v/1"}
			}, directURL},
		{"sh usage", "sh",
			func(string) []string {
				return []string{"examples/parse.sh", "--base", srv.URL, "--key", key, "--usage"}
			}, "示例账号"},
		{"python3 proxy 下载", "python3",
			func(out string) []string {
				return []string{"examples/parse.py", "--base", srv.URL, "--key", key,
					"--url", "x", "--download", out}
			}, "已保存"},
		{"node proxy 下载", "node",
			func(out string) []string {
				return []string{"examples/parse.js", "--base", srv.URL, "--key", key,
					"--url", "x", "--download", out}
			}, "已保存"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin, err := exec.LookPath(tc.bin)
			if err != nil {
				t.Skipf("环境里没有 %s，跳过", tc.bin)
			}
			out := filepath.Join(t.TempDir(), "dl.mp4")
			cmd := exec.Command(bin, tc.args(out)...)
			cmd.Dir = "." // 相对路径按仓库根算
			got, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("示例执行失败：%v\n%s", err, got)
			}
			if !strings.Contains(string(got), tc.want) {
				t.Errorf("输出里没有 %q：\n%s", tc.want, got)
			}
			// 下载类用例：文件真的落盘且非空
			if strings.Contains(tc.name, "下载") {
				st, err := os.Stat(out)
				if err != nil || st.Size() == 0 {
					t.Errorf("下载的文件不对：%v", err)
				}
			}
		})
	}
}

// TestDocsHaveNoStaleAPIPaths：不许留下已经删掉的旧接口路径。
func TestDocsHaveNoStaleAPIPaths(t *testing.T) {
	for _, rel := range append(apiDocs, "docs/平台调研报告.md") {
		body := readDoc(t, rel)
		if strings.Contains(body, "/api/v1/") {
			t.Errorf("%s 仍引用已删除的旧路径 /api/v1/（现在是 /v1/...）", rel)
		}
	}
}

// TestAPIDocEndpointHeadingsAreRelative：端点小节的标题必须是相对路径。
func TestAPIDocEndpointHeadingsAreRelative(t *testing.T) {
	body := readDoc(t, "docs/API.md")
	re := regexp.MustCompile(`(?m)^#{3,4} .*\b(GET|POST|PUT|DELETE|HEAD) (/\S+)`)
	found := 0
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		found++
		if !strings.HasPrefix(m[2], "/v1/") && !strings.HasPrefix(m[2], "/healthz") &&
			!strings.HasPrefix(m[2], "/readyz") && !strings.HasPrefix(m[2], "/tip.png") {
			t.Errorf("端点标题里的路径不像相对路径：%q", m[2])
		}
	}
	if found < 15 {
		t.Fatalf("只扫到 %d 个端点标题，正则可能失效", found)
	}
}

// TestDocsListEveryEndpoint：文档必须覆盖所有对外端点。
//
// 新增端点却忘了写文档，是这类项目最常见的腐烂方式。
func TestDocsListEveryEndpoint(t *testing.T) {
	body := readDoc(t, "docs/API.md")
	for _, path := range []string{
		"/v1/version", "/v1/health", "/v1/platforms", "/healthz", "/readyz",
		"/v1/usage", "/v1/info", "/v1/links", "/v1/detail", "/v1/batch/links",
		"/v1/checkin", "/v1/ledger", "/v1/sign",
		"/v1/admin/accounts", "/v1/admin/stats", "/v1/admin/quota", "/v1/admin/ledger",
		"/v1/admin/sign",
		"/v1/proxy", "/tip.png",
	} {
		if !strings.Contains(body, path) {
			t.Errorf("docs/API.md 没有提到端点 %s", path)
		}
	}
}

// TestDocsNeverPutPlainKeyInURL：文档里不许出现"把明文 Key 放进 URL"的例子。
//
// URL 里只允许签名凭据（acc_/adm_ + 时间戳 + 签名）。文档里留一个
// `?key=vl_xxx` 的示例，读者就会照着用——那条路现在会 403，
// 而更糟的是它教人把长期有效的凭据写进浏览器历史、隧道日志与截屏。
func TestDocsNeverPutPlainKeyInURL(t *testing.T) {
	forbidden := []string{
		"?key=vl_", "?key=$KEY", "?key=$ADMIN", "?key=$PUBLIC",
		"&key=vl_", "&key=$KEY", "key=vl_public", "Authorization: Bearer vl_public",
	}
	for _, rel := range apiDocs {
		body := readDoc(t, rel)
		for _, bad := range forbidden {
			if strings.Contains(body, bad) {
				t.Errorf("%s 里出现了把明文 Key 放进 URL 的写法 %q（URL 里只允许签名凭据）", rel, bad)
			}
		}
	}
	// 签名规则本身必须写清楚，否则调用方无从下手
	api := readDoc(t, "docs/API.md")
	for _, want := range []string{"GET /v1/sign", "HMAC-SHA256", "acc_", "signature_expired"} {
		if !strings.Contains(api, want) {
			t.Errorf("docs/API.md 缺少签名相关说明 %q", want)
		}
	}
}

// TestOpenAPIMatchesDocs：机器可读规格要与文档同源：
// 路径相对、servers 给出官方实例与相对地址、没有硬编码 host。
func TestOpenAPIMatchesDocs(t *testing.T) {
	body := readDoc(t, "docs/openapi.yaml")
	if strings.Contains(body, "127.0.0.1:8080/v1") {
		t.Error("openapi.yaml 里出现了硬编码的接口地址")
	}
	if !strings.Contains(body, "https://vl.wzml.cc.cd") {
		t.Error("openapi.yaml 的 servers 应给出官方测试实例")
	}
	if !regexp.MustCompile(`url:\s*"/"`).MatchString(body) {
		t.Error("openapi.yaml 的 servers 应包含相对地址 /")
	}
	// 所有 paths 键都是相对路径
	re := regexp.MustCompile(`(?m)^  (/\S*):\s*$`)
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "  http") {
			t.Errorf("openapi.yaml 里出现了绝对地址的 path：%s", line)
		}
	}
	if got := len(re.FindAllString(body, -1)); got < 15 {
		t.Errorf("openapi.yaml 只扫到 %d 个 path，正则可能失效", got)
	}
}

// TestReadmePointsAtTheSingleAPIDoc：README 不再重复接口文档，而是链过去。
func TestReadmePointsAtTheSingleAPIDoc(t *testing.T) {
	body := readDoc(t, "README.md")
	if !strings.Contains(body, "docs/API.md") {
		t.Error("README 应链接到 docs/API.md")
	}
	// 只统计"命令形式"的示例（行首的可选 $ 加 curl），散文里提到 curl 不算
	re := regexp.MustCompile(`(?m)^\s*\$?\s*curl\s`)
	if n := len(re.FindAllString(body, -1)); n > 6 {
		t.Errorf("README 里有 %d 条 curl 命令示例：接口文档已合并到 docs/API.md，"+
			"README 只该留最小上手示例（≤6）", n)
	}
}
