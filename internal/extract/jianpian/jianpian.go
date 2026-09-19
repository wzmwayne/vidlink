// Package jianpian 实现"荐片"（jianpian）影视内容的解析与搜索。
//
// 链路（2026-09 实测，见 docs/evidence/搜索与荐片取证.md）：
//
//	GET /api/v2/sys/init                      取 secret（可选签名用）
//	GET /api/v2/search/videoV2?key=&page=      搜索（10 条/页，带 total）
//	GET /api/video/detailv2?id=                详情：线路 × 选集 + ftp 直链
//	线路里的 url 就是 m3u8（绝对地址，无签名、无 query、不过期）
//
// 三个必须记住的实测事实：
//
//  1. **线路就是它的"清晰度"**。一部剧有十几条线路，彼此是不同来源，
//     质量与可用性差异极大：实测同一条目里既有 AES-128 加密的 HLS（VIP线路），
//     也有明文 HLS（424 段）、还有 master playlist（#EXT-X-STREAM-INF），
//     甚至有个别线路的域名已经解析不了。所以这里把线路名放进 Stream.Quality，
//     并让调用方用 quality=<线路名> 选择。
//  2. **"VIP线路"匿名也可用**：m3u8 与 16 字节 AES 密钥都不需要 token，
//     因此这里不跳过任何线路，只在 Line.VIP 上标注。
//  3. 域名会轮换（8 月 mv.cuitonghai.com → 现在的 mv.cqhnq.com，
//     图片 CDN 与切片 CDN 同样在变）。**绝不能写死**：线路地址来自 detail 响应，
//     图片域名来自 /api/v2/settings/resourceDomainConfig。
package jianpian

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"vidlink/internal/core"
	"vidlink/internal/deps"
	"vidlink/internal/jsonx"
	"vidlink/internal/netx"
)

// 搜索的分页常量：荐片固定 10 条/页，端点层允许一次取 1..50 条。
const (
	pageSize       = 10
	searchMinLimit = 1
	searchMaxLimit = 50
	searchMaxPage  = 50
)

// Endpoints 集中管理地址，便于换域名与离线测试（荐片的域名历史上换过多次）。
type Endpoints struct {
	Init           string
	Search         string
	Detail         string
	ResourceDomain string
}

// DefaultEndpoints 返回官方（H5）地址。
func DefaultEndpoints() Endpoints {
	const base = "https://h5.jianpianips1.com"
	return Endpoints{
		Init:           base + "/api/v2/sys/init",
		Search:         base + "/api/v2/search/videoV2",
		Detail:         base + "/api/video/detailv2",
		ResourceDomain: base + "/api/v2/settings/resourceDomainConfig",
	}
}

// Extractor 是荐片提取器。
type Extractor struct {
	d  *deps.Deps
	ep Endpoints

	mu        sync.Mutex
	secret    string
	imgDomain string
	cfgExp    time.Time
}

// New 构造荐片提取器。
func New(d *deps.Deps) *Extractor {
	return &Extractor{d: d, ep: DefaultEndpoints()}
}

// WithEndpoints 返回替换了接口地址的副本（测试与镜像用）。
func (e *Extractor) WithEndpoints(ep Endpoints) *Extractor {
	// 不整体拷贝：Extractor 里带 sync.Mutex（配置缓存）。
	// 顺带丢掉旧缓存，换地址后重新取 secret/图片域名也是对的。
	return &Extractor{d: e.d, ep: ep}
}

func (e *Extractor) Name() core.Platform { return core.PlatformJianpian }

// Hosts 返回空：**荐片不支持 URL 解析**。
//
// 它的 H5 是 hash 路由（`#/...`），没有稳定的作品页形态，硬猜路径只会带来
// "看起来能解析、实际随机失败"的假象；因此唯一入口是 `platform=jianpian&id=`。
// 返回空切片意味着注册表永远不会按域名把荐片链接路由进来。
func (e *Extractor) Hosts() []string { return nil }

// Match 恒为 false（理由见 Hosts）。
func (e *Extractor) Match(*core.URL) bool { return false }

// Parse 永远失败：荐片不支持 URL 解析（保留它只是为了满足 core.Extractor 契约，
// 并给出可操作的提示）。
func (e *Extractor) Parse(context.Context, *core.URL) (*core.Video, error) {
	return nil, core.BadInput(core.PlatformJianpian,
		"荐片不支持链接解析：请用 platform=jianpian&id=<影片ID>（先用 /v1/search 搜到影片 ID）")
}

// ParseID 按解析 ID 解析。荐片的解析 ID 有两种形态：
//
//	553300          影片 ID：默认取首选（VIP）线路的第 1 集
//	553300_33921    影片 ID_单集 ID：精确到某一集（单集 ID 见 /v1/info 的 series.episodes[].id）
//
// 之所以用下划线拼接而不是额外的 query 参数：ID 本身就能唯一定位"哪一集"，
// 缓存键、日志、分享链接都不会因为漏传参数而错位。
func (e *Extractor) ParseID(ctx context.Context, id string) (*core.Video, error) {
	movieID, episodeID, err := splitID(id)
	if err != nil {
		return nil, err
	}
	node, derr := e.fetchDetail(ctx, movieID)
	if derr != nil {
		return nil, derr
	}
	return e.build(ctx, node, movieID, episodeID)
}

// --- 上游请求 ---

// headers 组装请求头。签名是可选的（实测服务端不强制），但按研究建议带上。
func (e *Extractor) headers(ctx context.Context) netx.Headers {
	h := netx.Headers{
		"User-Agent": e.d.Params.Mobile(),
		"Accept":     "application/json, text/plain, */*",
		"Referer":    "https://h5.jianpianips1.com/",
	}
	secret, _ := e.config(ctx)
	if secret != "" {
		// signature = md5("610" + timestamp + secret)，见 docs/evidence。
		ts := strconv.FormatInt(e.now().Unix(), 10)
		h["version"] = "610"
		h["timestamp"] = ts
		h["signature"] = md5Hex("610" + ts + secret)
	}
	if ck := e.d.Cookies.Get(core.PlatformJianpian); ck != "" {
		h["Cookie"] = ck
	}
	return h
}

func (e *Extractor) now() time.Time {
	if e.d != nil && e.d.Now != nil {
		return e.d.Now()
	}
	return time.Now()
}

// config 返回 (secret, 图片域名)，带 1 小时缓存。
//
// 两者都只在"能拿到就用"的意义上存在：拿不到就不签名、封面保持相对路径，
// 解析主链路不受影响。
func (e *Extractor) config(ctx context.Context) (string, string) {
	e.mu.Lock()
	if e.cfgExp.After(e.now()) {
		s, img := e.secret, e.imgDomain
		e.mu.Unlock()
		return s, img
	}
	e.mu.Unlock()

	var secret, img string
	if body, _, err := e.d.Client.GetBytes(ctx, e.ep.Init, netx.Headers{
		"User-Agent": e.d.Params.Mobile(),
	}, 1<<20); err == nil {
		if node, jerr := jsonx.Unmarshal(body); jerr == nil {
			secret = jsonx.String(node, "data.secret")
		}
	}
	if body, _, err := e.d.Client.GetBytes(ctx, e.ep.ResourceDomain, netx.Headers{
		"User-Agent": e.d.Params.Mobile(),
	}, 1<<20); err == nil {
		if node, jerr := jsonx.Unmarshal(body); jerr == nil {
			// imgDomain 是逗号分隔的多个域名，取第一个（实测互为镜像）。
			if list := strings.Split(jsonx.String(node, "data.imgDomain"), ","); len(list) > 0 {
				img = strings.TrimSpace(list[0])
			}
		}
	}

	e.mu.Lock()
	e.secret, e.imgDomain = secret, img
	e.cfgExp = e.now().Add(time.Hour)
	e.mu.Unlock()
	return secret, img
}

func (e *Extractor) fetchDetail(ctx context.Context, id string) (jsonx.Node, error) {
	body, code, err := e.d.Client.GetBytes(ctx,
		e.ep.Detail+"?id="+id, e.headers(ctx), 4<<20)
	if err != nil {
		return nil, core.Upstream(core.PlatformJianpian, "detail", err)
	}
	if code < 200 || code >= 300 {
		return nil, core.Errf(core.KindUpstream, core.PlatformJianpian, "detail", "HTTP %d", code)
	}
	node, jerr := jsonx.Unmarshal(body)
	if jerr != nil {
		return nil, core.E(core.KindUpstream, core.PlatformJianpian, "detail",
			"响应不是合法 JSON", jerr)
	}
	if c := jsonx.Int(node, "code"); c != 1 {
		return nil, classifyAPIError(c, jsonx.String(node, "msg"))
	}
	if data := jsonx.Get(node, "data"); data == nil {
		return nil, core.NotFound(core.PlatformJianpian, "影片不存在：%s", id)
	}
	return node, nil
}

// classifyAPIError 把荐片的业务错误码映射成领域错误。
func classifyAPIError(code int64, msg string) error {
	switch code {
	case 0, 1:
		return nil
	case 404, 1002:
		return core.NotFound(core.PlatformJianpian, "影片不存在（code=%d %s）", code, msg)
	case 401, 403:
		return core.Forbidden(core.PlatformJianpian, "需要登录或权限不足（code=%d %s）", code, msg)
	case 429, 400002:
		return core.Errf(core.KindRateLimit, core.PlatformJianpian, "api",
			"触发荐片风控（code=%d %s）", code, msg)
	default:
		return core.Errf(core.KindUpstream, core.PlatformJianpian, "api",
			"荐片返回错误 code=%d msg=%s", code, msg)
	}
}

// --- 领域模型构造 ---

// rawEpisode 是上游的一条选集/片源。
type rawEpisode struct {
	id   string
	name string
	url  string
	vip  bool
}

// rawLine 是上游的一条线路。
type rawLine struct {
	name     string
	key      string
	vip      bool
	episodes []rawEpisode
}

// build 把 detail 响应投影成统一模型。
//
// 三个层次要分清：
//
//	Lines    全部线路（VIP 优先）
//	Episodes **全部线路 × 全部集**（每集带自己的单集 ID 与播放地址）
//	Videos   选中的那一集在**全部线路上**的播放地址（每条线路一条流）
//
// Episodes 全量保留是"列出选集"接口的前提（它要按线路筛），
// 内存里就是几百个小结构体；但投影到响应时会按档位裁剪：
// info 只给"线路 + 默认线路的集（不含 URL）"，detail 给"选中线路的集 + 直链"。
func (e *Extractor) build(ctx context.Context, node jsonx.Node, movieID, episodeID string) (*core.Video, error) {
	data := jsonx.Get(node, "data")
	lines := preferVIP(parseLines(data))
	if len(lines) == 0 {
		return nil, core.NotFound(core.PlatformJianpian, "该影片没有可用的播放线路：%s", movieID)
	}

	// 选中的那一集：给了单集 ID 就必须在该影片里找到（找不到是调用方用错了 ID）；
	// 没给就默认首选（VIP）线路的第 1 集。
	chosen, chosenIdx := lines[0].episodes[0], 1
	if episodeID != "" {
		var ok bool
		chosen, chosenIdx, ok = findEpisodeByID(lines, episodeID)
		if !ok {
			return nil, core.NotFound(core.PlatformJianpian,
				"该影片没有单集 ID %s：先用 GET /v1/info?platform=jianpian&id=%s 取 series.episodes[].id（或直接用 parse_id），"+
					"再用 id=<影片ID>_<单集ID> 解析", episodeID, movieID)
		}
	}

	ftp := ftpMap(data)

	v := &core.Video{
		Platform: core.PlatformJianpian,
		ID:       movieID,
		Title:    jsonx.String(data, "title"),
		Desc:     jsonx.String(data, "description"),
		Cover:    e.imageURL(ctx, firstNonEmpty(jsonx.String(data, "thumbnail"), jsonx.String(data, "tvimg"))),
		Author:   core.Author{Name: strings.Join(jsonx.StringList(data, "directors"), " / ")},
		Finished: jsonx.Int(data, "finished") == 1,
	}
	// mask 的语义按内容类型分两种：剧集是更新进度（"第10集"），
	// 电影是版本标签（"DVD"/"HD"）。只有前者才叫"更新到"。
	if len(lines[0].episodes) > 1 {
		v.Latest = jsonx.String(data, "mask")
	}
	for i, ln := range lines {
		v.Lines = append(v.Lines, core.Line{Name: ln.name, Key: ln.key, Count: len(ln.episodes), VIP: ln.vip})
		for j, ep := range ln.episodes {
			v.Episodes = append(v.Episodes, core.Episode{
				ID:      ep.id,
				Name:    ep.name,
				Line:    ln.name,
				LineIdx: i + 1,
				Index:   j + 1,
				URL:     ep.url,
				FTP:     ftp[ep.name],
				VIP:     ln.vip || ep.vip,
				ParseID: parseID(movieID, ep.id),
			})
		}
	}

	// 传了拼接 ID（`qwert_poiuy`）时，选集列表只保留这一集。
	// 注意可能不止一条：实测前三条线路**共用同一个单集 ID**，
	// 所以"这个单集 ID 属于哪条线路"的答案可能有多条，每条各列一条记录。
	if episodeID != "" {
		kept := make([]core.Episode, 0, len(v.Episodes))
		for _, ep := range v.Episodes {
			if ep.ID == episodeID {
				kept = append(kept, ep)
			}
		}
		v.Episodes = kept
		v.EpisodeOnly = true
	}

	// Videos：同一个"这一集"在所有线路上的播放地址，每条线路一条流。
	// 顺序保持 lines 的顺序（VIP 优先），因此 Videos[0] 就是默认线路。
	headers := netx.Headers{"User-Agent": e.d.Params.Mobile()}
	variants := map[string]bool{}
	for i, ln := range lines {
		ep, ok := findEpisode(ln.episodes, chosen, chosenIdx)
		if !ok || ep.url == "" {
			continue
		}
		if ep.name != "" {
			variants[ep.name] = true
		}
		v.Videos = append(v.Videos, core.Stream{
			URL:       ep.url,
			Quality:   ln.name,
			QualityID: i + 1,
			MimeType:  "application/vnd.apple.mpegurl",
			Headers:   headers,
		})
	}
	if len(v.Videos) == 0 {
		return nil, core.NotFound(core.PlatformJianpian, "该影片这一集没有可用的播放地址：%s", movieID)
	}
	// 电影的不同线路经常是**不同版本/语言**（实测同一部电影：三条线路叫 "DVD"，
	// 一条叫 "粤语"）。这不是异常，但客户端光看线路名看不出来，必须提示。
	if len(variants) > 1 {
		v.Warning = "不同线路的版本/语言可能不同（" + strings.Join(sortedKeys(variants), " / ") +
			"）：quality 选的是线路，播放失败或语言不符请换 backup_urls 里的线路。"
	}
	return v, nil
}

// parseLines 解析并合并 source_list_source 与 vip_source_list_source（按线路名去重）。
func parseLines(data jsonx.Node) []rawLine {
	var out []rawLine
	seen := map[string]bool{}
	add := func(ln jsonx.Node, fromVIP bool) {
		name := strings.TrimSpace(jsonx.String(ln, "name"))
		if name == "" {
			return
		}
		if seen[name] {
			// 同一条线路可能同时出现在 source_list_source 与
			// vip_source_list_source 里：这里是**合并**而不是跳过，
			// 否则"VIP 标记"会因为先被普通列表登记而丢掉。
			if fromVIP {
				for i := range out {
					if out[i].name == name {
						out[i].vip = true
					}
				}
			}
			return
		}
		seen[name] = true
		line := rawLine{
			name: name,
			key:  jsonx.String(ln, "source_key"),
			vip:  fromVIP || jsonx.Int(ln, "vip_source") == 1 || strings.Contains(strings.ToUpper(name), "VIP"),
		}
		for _, ep := range jsonx.Slice(ln, "source_list") {
			line.episodes = append(line.episodes, rawEpisode{
				id:   strconv.FormatInt(jsonx.Int(ep, "id"), 10),
				name: firstNonEmpty(jsonx.String(ep, "source_name"), jsonx.String(ep, "weight")),
				url:  jsonx.String(ep, "url"),
				vip:  jsonx.Int(ep, "vip_source") == 1,
			})
		}
		if len(line.episodes) > 0 {
			out = append(out, line)
		}
	}
	for _, ln := range jsonx.Slice(data, "source_list_source") {
		add(ln, false)
	}
	for _, ln := range jsonx.Slice(data, "vip_source_list_source") {
		add(ln, true)
	}
	return out
}

// preferVIP 把 VIP 线路排到前面（稳定排序，其余保持上游顺序）。
func preferVIP(lines []rawLine) []rawLine {
	out := make([]rawLine, 0, len(lines))
	for _, ln := range lines {
		if ln.vip {
			out = append(out, ln)
		}
	}
	for _, ln := range lines {
		if !ln.vip {
			out = append(out, ln)
		}
	}
	return out
}

// findEpisodeByID 在所有线路里按单集 ID 找那一集（VIP 线路优先）。
//
// 返回的 idx 是该集在**找到它的那条线路**里的 1 基序号。
func findEpisodeByID(lines []rawLine, episodeID string) (rawEpisode, int, bool) {
	for _, ln := range lines {
		for j, ep := range ln.episodes {
			if ep.id != "" && ep.id == episodeID {
				return ep, j + 1, true
			}
		}
	}
	return rawEpisode{}, 0, false
}

// findEpisode 在另一条线路里找"同一集"。
//
// 三级对齐，缺一不可（都是实测踩出来的）：
//
//  1. **规范化集名**：同一部剧不同线路的集名不一致（"第01集" vs "第1集"），
//     抽数字后都变成 n1；
//  2. **单集 ID**：少数线路会共用同一个上游 ID；
//  3. **同序号**：电影的各线路"集名"其实是版本/语言（"DVD"/"粤语"），
//     名称对不上，只能按序号（第 1 个对第 1 个）对齐。
func findEpisode(eps []rawEpisode, want rawEpisode, wantIdx int) (rawEpisode, bool) {
	if key := episodeKey(want.name); key != "" {
		for _, ep := range eps {
			if episodeKey(ep.name) == key {
				return ep, true
			}
		}
	}
	if want.id != "" {
		for _, ep := range eps {
			if ep.id == want.id {
				return ep, true
			}
		}
	}
	if wantIdx >= 1 && wantIdx <= len(eps) {
		return eps[wantIdx-1], true
	}
	return rawEpisode{}, false
}

// episodeKey 把集名规范化成对齐键：
//
//	"第01集" / "第1集" / "01" → "n1"
//	"DVD" / "粤语"            → 原样（电影的各线路是不同版本/语言）
func episodeKey(name string) string {
	if n := firstInt(name); n > 0 {
		return "n" + strconv.Itoa(n)
	}
	return strings.TrimSpace(name)
}

// firstInt 取字符串里的第一段连续数字。
func firstInt(s string) int {
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			n, _ := strconv.Atoi(s[start:i])
			return n
		}
	}
	if start >= 0 {
		n, _ := strconv.Atoi(s[start:])
		return n
	}
	return 0
}

// parseID 拼"可以直接解析"的 ID：`<影片ID>_<单集ID>`。
func parseID(movieID, episodeID string) string {
	if movieID == "" || episodeID == "" {
		return ""
	}
	return movieID + "_" + episodeID
}

// splitID 拆解析 ID：`553300` 或 `553300_33921`。
func splitID(raw string) (movieID, episodeID string, err error) {
	raw = strings.TrimSpace(raw)
	movieID, episodeID, hadSep := strings.Cut(raw, "_")
	if hadSep && episodeID == "" {
		return "", "", core.BadInput(core.PlatformJianpian,
			"单集 ID 不能为空：%q（格式：<影片ID> 或 <影片ID>_<单集ID>）", raw)
	}
	if !isNumericID(movieID) {
		return "", "", core.BadInput(core.PlatformJianpian,
			"荐片影片 ID 必须是数字：%q（格式：<影片ID> 或 <影片ID>_<单集ID>）", raw)
	}
	if episodeID != "" && !isNumericID(episodeID) {
		return "", "", core.BadInput(core.PlatformJianpian,
			"单集 ID 必须是数字：%q（格式：<影片ID> 或 <影片ID>_<单集ID>）", raw)
	}
	return movieID, episodeID, nil
}

// sortedKeys 返回排序后的键（用于生成稳定的提示文案/测试断言）。
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ftpMap 把 ftp_list 整理成 集名 → 直链。
//
// 注意：实测该 FTP 域名（a.gbl.114s.com）在当前网络**DNS 都解析不了**，
// 所以它只作为信息透传，不作为下载主路径（详见 docs/evidence）。
func ftpMap(data jsonx.Node) map[string]string {
	out := map[string]string{}
	for _, it := range jsonx.Slice(data, "ftp_list") {
		title := strings.TrimSpace(jsonx.String(it, "title"))
		url := jsonx.String(it, "url")
		if title == "" || url == "" {
			continue
		}
		out[title] = url
	}
	return out
}

// imageURL 把相对封面路径补成绝对地址（域名来自 resourceDomainConfig）。
func (e *Extractor) imageURL(ctx context.Context, src string) string {
	if src == "" || strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		return src
	}
	if _, img := e.config(ctx); img != "" {
		return "https://" + img + src
	}
	return src
}

// --- 工具 ---

func isNumericID(s string) bool {
	if len(s) < 4 || len(s) > 12 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
