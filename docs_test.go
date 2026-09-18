package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
		"/v1/checkin", "/v1/ledger",
		"/v1/admin/accounts", "/v1/admin/stats", "/v1/admin/quota", "/v1/admin/ledger",
		"/v1/proxy", "/tip.png",
	} {
		if !strings.Contains(body, path) {
			t.Errorf("docs/API.md 没有提到端点 %s", path)
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
