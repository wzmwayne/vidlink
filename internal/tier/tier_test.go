package tier

import (
	"encoding/json"
	"strings"
	"testing"

	"vidlink/internal/core"
)

// sampleVideo 构造一个典型的 DASH 结果（B 站形状）。
func sampleVideo() *core.Video {
	v := &core.Video{
		Platform:  core.PlatformBilibili,
		ID:        "BV1GJ411x7h7",
		Title:     "【官方 MV】Never Gonna Give You Up",
		Desc:      "描述",
		Cover:     "https://i1.hdslb.com/cover.jpg",
		SourceURL: "https://www.bilibili.com/video/BV1GJ411x7h7",
		Author:    core.Author{ID: "486906719", Name: "索尼音乐中国", Avatar: "https://i2.hdslb.com/a.jpg"},
		Stats:     &core.Stats{View: 105664379, Like: 2918303, Duration: 213},
		NeedsMux:  true,
		Videos: []core.Stream{
			{URL: "https://cdn/1080-avc.m4s", Height: 1080, Width: 1920, Quality: "1080P",
				QualityID: 80, Codec: "avc1.640032", Bandwidth: 1500000,
				Headers: map[string]string{"Referer": "https://www.bilibili.com/"}},
			{URL: "https://cdn/1080-hevc.m4s", Height: 1080, Width: 1920, Quality: "1080P",
				QualityID: 80, Codec: "hev1.1.6.L120.90", Bandwidth: 900000},
			{URL: "https://cdn/720-avc.m4s", Height: 720, Width: 1280, Quality: "720P",
				QualityID: 64, Codec: "avc1.64001F", Bandwidth: 800000},
			{URL: "https://cdn/480-avc.m4s", Height: 480, Width: 852, Quality: "480P",
				QualityID: 32, Codec: "avc1.64001F", Bandwidth: 400000},
		},
		Audios: []core.Stream{
			{URL: "https://cdn/audio.m4s", Quality: "192K", QualityID: 30280, Codec: "mp4a.40.2"},
		},
	}
	core.SortStreams(v.Videos)
	return v
}

// jsonKeys 把结构体转成 map，便于断言"有没有某个字段"。
func jsonKeys(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestInfoHasNoDirectURLs 是 info 档的核心契约。
//
// info 只卖"元信息 + 有哪些档位"，一旦泄露出任何一个可下载的直链，
// 客户就没有理由再买 links——0.5 点的档位会把 1.0 点的档位吃掉。
func TestInfoHasNoDirectURLs(t *testing.T) {
	out := NewInfo(sampleVideo())
	m := jsonKeys(t, out)

	// 唯一允许出现的 URL 是 source_url（页面地址，不可下载）
	for k, v := range m {
		if k == "source_url" || k == "cover" || k == "author" || k == "platform" || k == "id" {
			continue
		}
		if s, ok := v.(string); ok && strings.HasPrefix(s, "https://cdn/") {
			t.Errorf("info 档泄露了直链：字段 %s = %s", k, s)
		}
	}
	if _, ok := m["videos"]; ok {
		t.Error("info 档不应包含 videos（那是 detail 的内容）")
	}
	if _, ok := m["best_video_url"]; ok {
		t.Error("info 档不应包含 best_video_url")
	}

	// 但必须给出可用档位，否则客户无从选择
	qs, ok := m["qualities"].([]any)
	if !ok || len(qs) == 0 {
		t.Fatal("info 档必须给出 qualities 列表")
	}
	if len(qs) != 3 {
		t.Fatalf("应有 3 个档位（1080/720/480），得到 %d", len(qs))
	}
	// qualities 里也不能有直链
	for _, q := range qs {
		qm := q.(map[string]any)
		for k, v := range qm {
			if s, ok := v.(string); ok && strings.HasPrefix(s, "https://cdn/") {
				t.Errorf("qualities[].%s 不应含直链", k)
			}
		}
	}
}

// TestLinksHasNoMetadata 是 links 档的核心契约：
// **只有直链，没有标题/作者/统计/封面**。那是 detail 档的商品。
func TestLinksHasNoMetadata(t *testing.T) {
	out, err := NewLinks(sampleVideo(), "")
	if err != nil {
		t.Fatal(err)
	}
	m := jsonKeys(t, out)

	forbidden := []string{
		"title", "desc", "cover", "author", "stats",
		"platform", "id", "source_url", "qualities", "images", "subtitles",
	}
	for _, f := range forbidden {
		if _, ok := m[f]; ok {
			t.Errorf("links 档不应包含元信息字段 %q（那是 detail 的商品）", f)
		}
	}

	// 交付必需的字段必须在
	if m["url"] == nil {
		t.Fatal("links 档必须有 url")
	}
	if m["audio_url"] == nil {
		t.Error("DASH 分离流必须给出 audio_url，否则客户拿不到完整文件")
	}
	if m["needs_mux"] != true {
		t.Error("应标记 needs_mux")
	}
	hdrs, ok := m["headers"].(map[string]any)
	if !ok || hdrs["Referer"] == nil {
		t.Error("防盗链所需的 headers 必须给出，否则客户下载会 403")
	}
}

// TestDetailContainsBoth：detail = info ∪ links，且打包价低于分开买。
func TestDetailContainsBoth(t *testing.T) {
	out := NewDetail(sampleVideo())
	m := jsonKeys(t, out)

	for _, f := range []string{
		"title", "author", "stats", "qualities", // info 的内容
		"videos", "audios", "best_video_url", "best_video_headers", "needs_mux", // links 的内容
	} {
		if _, ok := m[f]; !ok {
			t.Errorf("detail 档缺少字段 %q", f)
		}
	}
	vs := m["videos"].([]any)
	if len(vs) != 4 {
		t.Fatalf("detail 应给出全部 4 条流，得到 %d", len(vs))
	}
}

func TestSelectDefaultReturnsHighest(t *testing.T) {
	v := sampleVideo()
	s, err := Select(v.Videos, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Height != 1080 {
		t.Fatalf("不指定清晰度应返回最高，得到 %dp", s.Height)
	}
	// 同高度下应选排序更优的（avc1 优先于 hevc）
	if !strings.HasPrefix(s.Codec, "avc1") {
		t.Errorf("同分辨率应优先 H.264，得到 %s", s.Codec)
	}
}

func TestSelectByHeight(t *testing.T) {
	v := sampleVideo()
	for _, want := range []string{"720", "720p", "720P", " 720 "} {
		s, err := Select(v.Videos, want)
		if err != nil {
			t.Errorf("Select(%q) 报错: %v", want, err)
			continue
		}
		if s.Height != 720 {
			t.Errorf("Select(%q) 得到 %dp, want 720p", want, s.Height)
		}
	}
}

func TestSelectByQN(t *testing.T) {
	v := sampleVideo()
	s, err := Select(v.Videos, "qn:32")
	if err != nil {
		t.Fatal(err)
	}
	if s.QualityID != 32 || s.Height != 480 {
		t.Fatalf("qn:32 应选中 480P，得到 qn=%d %dp", s.QualityID, s.Height)
	}
}

// TestSelectNotFoundListsOptions：找不到就报错并列出可选档位，
// 而不是"就近降级"——客户付了钱，拿到的必须是他要的那一档。
func TestSelectNotFoundListsOptions(t *testing.T) {
	v := sampleVideo()
	_, err := Select(v.Videos, "2160")
	if err == nil {
		t.Fatal("2160P 不存在，应报错而不是静默降级")
	}
	msg := err.Error()
	for _, want := range []string{"1080P", "720P", "480P"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应列出可用档位 %s，得到 %q", want, msg)
		}
	}
}

func TestSelectBrokenParam(t *testing.T) {
	v := sampleVideo()
	if _, err := Select(v.Videos, "高清"); err == nil {
		t.Fatal("无法识别的清晰度参数应报错")
	}
}

func TestSelectEmptyStreams(t *testing.T) {
	if _, err := Select(nil, ""); err == nil {
		t.Fatal("没有流时应报错")
	}
}

// TestQualitiesDedupAndOrder：同一高度多个编码变体应合并成一个档位。
func TestQualitiesDedupAndOrder(t *testing.T) {
	q := Qualities(sampleVideo())
	if len(q) != 3 {
		t.Fatalf("应有 3 个档位（1080/720/480），得到 %d", len(q))
	}
	if q[0].Height != 1080 || q[1].Height != 720 || q[2].Height != 480 {
		t.Fatalf("档位应从高到低，得到 %d/%d/%d", q[0].Height, q[1].Height, q[2].Height)
	}
	// 1080P 有 avc1 与 hevc 两个变体，应合并并列出
	if len(q[0].Codecs) != 2 {
		t.Fatalf("1080P 应列出 2 个编码，得到 %v", q[0].Codecs)
	}
	if q[0].QualityID != 80 {
		t.Errorf("档位应带平台原始代码，得到 %d", q[0].QualityID)
	}
}

func TestLinksImageOnlyContent(t *testing.T) {
	v := &core.Video{
		Platform: core.PlatformXiaohongshu,
		ID:       "note1",
		Images:   []core.Image{{URL: "https://img/1.jpg"}},
	}
	if _, err := NewLinks(v, ""); err == nil {
		t.Fatal("图文作品没有视频流，links 档应报错并提示改用 detail")
	}
}

func TestStreamsOmitEmptyHeaders(t *testing.T) {
	// headers 为空时不应输出 "headers": {}，减少无意义字节
	out := NewStreams([]core.Stream{{URL: "https://cdn/x", Height: 1080}})
	m := jsonKeys(t, out[0])
	if _, ok := m["headers"]; ok {
		t.Error("空的 headers 应被省略")
	}
	if _, ok := m["backup_urls"]; ok {
		t.Error("空的 backup_urls 应被省略")
	}
}
