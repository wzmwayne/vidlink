package bilibili

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"vidlink/internal/core"
	"vidlink/internal/jsonx"
	"vidlink/internal/netx"
)

// 番剧（PGC）解析。
//
// 为什么单独一条路：番剧的每一集都不在 x/web-interface/view 的"稿件"体系里，
// 它的入口是 pgc/view/web/season（按 ep_id 或 season_id 查）。
// 但**取流完全复用普通通道**——实测（2026-09-19）：
//
//	pgc/view/web/season?ep_id=733316   → 每集给出 aid/bvid/cid/long_title
//	x/web-interface/view?bvid=<该集>    → code 0（普通稿件接口对 PGC 内容照常返回）
//	x/player/playurl?...&try_look=1     → 12 条 DASH 轨、最高 1080P、dash 时长 1204s
//	                                      与季信息给的 1203160ms 对得上；最高轨 131MiB
//	                                      ——是**整集**，不是试看片段
//
// 不带 try_look 时只有 480P；会员/付费/地区限制的集数仍会拿不到（表现为无流或 404）。

// pgcEpisode 是番剧/影视的一集。
type pgcEpisode struct {
	ID       int64 // ep 号（就是链接里的那个）
	AID      int64
	BVID     string
	CID      int64
	Label    string // 集序号（"1"、"OVA 1"…，可能是非数字）
	Name     string // 这一集自己的名字（long_title）
	Cover    string
	Duration int64 // 毫秒
}

// parsePGC 解析一个番剧单集链接（/bangumi/play/ep123）。
func (e *Extractor) parsePGC(ctx context.Context, ref reference, headers netx.Headers) (*core.Video, error) {
	season, ep, err := e.fetchPGC(ctx, ref.ep, headers)
	if err != nil {
		return nil, err
	}
	if ep.BVID == "" && ep.AID == 0 {
		return nil, core.NotFound(core.PlatformBilibili,
			"这一集没有可用的稿件信息（ep=%d，可能尚未放送）", ref.ep)
	}

	// 复用普通稿件接口：番剧的每一集都有自己的 BV 号，cid/UP 主/统计/封面都在那里。
	epRef := reference{bvid: ep.BVID, aid: ep.AID}
	view, err := e.fetchView(ctx, epRef, headers)
	if err != nil {
		return nil, err
	}
	cid := ep.CID
	if cid == 0 {
		cid = jsonx.Int(view, "data.cid")
	}
	if cid == 0 {
		return nil, core.NotFound(core.PlatformBilibili, "未找到可用的 cid（ep=%d）", ref.ep)
	}

	video := &core.Video{
		Platform:  core.PlatformBilibili,
		ID:        firstNonEmptyStr(jsonx.String(view, "data.bvid"), ep.BVID),
		Title:     pgcTitle(season, ep),
		Desc:      jsonx.String(season, "summary"),
		Cover:     normalizeURL(firstNonEmptyStr(ep.Cover, jsonx.String(season, "cover"), jsonx.String(view, "data.pic"))),
		SourceURL: fmt.Sprintf("https://www.bilibili.com/bangumi/play/ep%d", ep.ID),
		Author: core.Author{
			ID:     strconv.FormatInt(jsonx.Int(view, "data.owner.mid"), 10),
			Name:   jsonx.String(view, "data.owner.name"),
			Avatar: normalizeURL(jsonx.String(view, "data.owner.face")),
		},
		Stats: &core.Stats{
			View:     jsonx.Int(view, "data.stat.view"),
			Like:     jsonx.Int(view, "data.stat.like"),
			Comment:  jsonx.Int(view, "data.stat.reply"),
			Collect:  jsonx.Int(view, "data.stat.favorite"),
			Share:    jsonx.Int(view, "data.stat.share"),
			Danmaku:  jsonx.Int(view, "data.stat.danmaku"),
			Duration: int(jsonx.Int(view, "data.duration")),
		},
	}
	if video.ID == "" {
		video.ID = jsonx.String(view, "data.bvid")
	}

	if err := e.attachStreams(ctx, video, epRef, cid, ep.AID, headers); err != nil {
		return nil, pgcStreamError(err, ep)
	}
	if subs, serr := e.fetchSubtitles(ctx, epRef, cid, ep.AID, headers); serr == nil {
		video.Subtitles = subs
	}

	// 番剧匿名一般最高 1080P（try_look=1 时）；更低只可能是会员/付费/地区限制。
	// 这里只提示现象与可能原因，不替平台下结论。
	if h := maxHeight(video); h > 0 && h < 1080 {
		video.Warning = fmt.Sprintf("本次最高只取到 %dP：该集可能是会员/付费或地区限制内容；"+
			"配置 B 站 Cookie（SESSDATA）或大会员账号可能提高上限", h)
	}
	return video, nil
}

// fetchPGC 按 ep_id 查季信息，并从中取出这一集。
func (e *Extractor) fetchPGC(ctx context.Context, epID int64, headers netx.Headers) (jsonx.Node, pgcEpisode, error) {
	url := fmt.Sprintf("%s?ep_id=%d", e.ep.PGCSeason, epID)
	body, code, err := e.d.Client.GetBytes(ctx, url, headers, 0)
	if err != nil {
		return nil, pgcEpisode{}, core.Upstream(core.PlatformBilibili, "pgc", err)
	}
	if code < 200 || code >= 300 {
		return nil, pgcEpisode{}, core.Errf(core.KindUpstream, core.PlatformBilibili,
			"pgc", "HTTP %d", code)
	}
	node, jerr := jsonx.Unmarshal(body)
	if jerr != nil {
		return nil, pgcEpisode{}, core.E(core.KindUpstream, core.PlatformBilibili,
			"pgc", "响应不是合法 JSON", jerr)
	}
	if apiCode := jsonx.Int(node, "code"); apiCode != 0 {
		return nil, pgcEpisode{}, classifyAPIError(apiCode, jsonx.String(node, "message"))
	}
	season := jsonx.Get(node, "result")
	if season == nil {
		return nil, pgcEpisode{}, core.NotFound(core.PlatformBilibili, "番剧信息为空（ep=%d）", epID)
	}

	// 正片在 result.episodes，花絮/OVA 等在 result.section[].episodes。
	for _, group := range [][]jsonx.Node{
		jsonx.Slice(season, "episodes"),
		sectionEpisodes(season),
	} {
		for _, it := range group {
			if jsonx.Int(it, "id") != epID {
				continue
			}
			return season, pgcEpisode{
				ID:       jsonx.Int(it, "id"),
				AID:      jsonx.Int(it, "aid"),
				BVID:     jsonx.String(it, "bvid"),
				CID:      jsonx.Int(it, "cid"),
				Label:    jsonx.String(it, "title"),
				Name:     jsonx.String(it, "long_title"),
				Cover:    normalizeURL(jsonx.String(it, "cover")),
				Duration: jsonx.Int(it, "duration"),
			}, nil
		}
	}
	return nil, pgcEpisode{}, core.NotFound(core.PlatformBilibili,
		"这一集不在该番剧的集列表里（ep=%d，可能已下架或未放送）", epID)
}

// pgcStreamError 把 PGC 取流失败翻译成"用户能照着做"的报错。
//
// 番剧与普通稿件的差别就在这里：普通稿件 -404 基本就是"内容没了"，
// 而番剧 -404 绝大多数是**会员/付费集或未放送**——直接把它当上游故障
// 报 502 会让人以为服务坏了，反复重试也永远好不了。
func pgcStreamError(err error, ep pgcEpisode) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "code=-10403"):
		return core.Forbidden(core.PlatformBilibili,
			"这是大会员专享集（ep%d）：需要带大会员 Cookie 才能取流", ep.ID)
	case strings.Contains(msg, "code=-404"):
		return core.Forbidden(core.PlatformBilibili,
			"这一集取不到流（ep%d）：通常是会员/付费集、尚未放送或地区限制内容", ep.ID)
	}
	return err
}

// sectionEpisodes 把 result.section[].episodes 摊平。
func sectionEpisodes(season jsonx.Node) []jsonx.Node {
	var out []jsonx.Node
	for _, sec := range jsonx.Slice(season, "section") {
		out = append(out, jsonx.Slice(sec, "episodes")...)
	}
	return out
}

// pgcTitle 拼一个可读的标题：「季名 + 第N集 + 该集名字」。
//
// 为什么不直接用 view 的标题：PGC 稿件的标题带一堆装饰
// （实测「【独家】《凡人修仙传之风起天南》重制版第1集【1月国创】」），
// 而季名 + 集号 + 集名是用户真正需要的信息。
func pgcTitle(season jsonx.Node, ep pgcEpisode) string {
	title := strings.TrimSpace(jsonx.String(season, "title"))
	label := epLabel(ep.Label)
	name := strings.TrimSpace(ep.Name)
	switch {
	case title == "":
		return firstNonEmptyStr(name, label)
	case label != "" && name != "":
		return fmt.Sprintf("%s %s %s", title, label, name)
	case label != "":
		return fmt.Sprintf("%s %s", title, label)
	case name != "":
		return title + " " + name
	}
	return title
}

// epLabel 把集序号变成"第N集"；非数字（"OVA 1"）保持原样。
func epLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if n, err := strconv.Atoi(raw); err == nil {
		return fmt.Sprintf("第%d集", n)
	}
	return raw
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
