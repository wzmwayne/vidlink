package tier

import (
	"encoding/json"
	"strings"
	"testing"

	"vidlink/internal/core"
)

// seriesVideo 造一个"剧集型内容"：两条线路（VIP 优先）、每线两集，
// 同集的 ID 在两条线路上不同（真实情况就是如此）。
func seriesVideo() *core.Video {
	return &core.Video{
		Platform: core.PlatformJianpian,
		ID:       "553300",
		Title:    "太空部队",
		Latest:   "第2集",
		Lines: []core.Line{
			{Name: "VIP线路", Key: "back_a", Count: 2, VIP: true},
			{Name: "线路乙", Key: "back_b", Count: 2},
		},
		Episodes: []core.Episode{
			{ID: "33921", Name: "第01集", Line: "VIP线路", LineIdx: 1, Index: 1,
				URL: "https://mv/a/33921.m3u8", ParseID: "553300_33921",
				FTP: "ftp://f/1.mp4", VIP: true},
			{ID: "33922", Name: "第02集", Line: "VIP线路", LineIdx: 1, Index: 2,
				URL: "https://mv/a/33922.m3u8", ParseID: "553300_33922", VIP: true},
			{ID: "6583678", Name: "第1集", Line: "线路乙", LineIdx: 2, Index: 1,
				URL: "https://mv/b/6583678.m3u8", ParseID: "553300_6583678"},
			{ID: "6583679", Name: "第2集", Line: "线路乙", LineIdx: 2, Index: 2,
				URL: "https://mv/b/6583679.m3u8", ParseID: "553300_6583679"},
		},
		Videos: []core.Stream{
			{URL: "https://mv/a/33921.m3u8", Quality: "VIP线路", QualityID: 1,
				MimeType: "application/vnd.apple.mpegurl"},
			{URL: "https://mv/b/6583678.m3u8", Quality: "线路乙", QualityID: 2,
				MimeType: "application/vnd.apple.mpegurl"},
		},
	}
}

// TestInfoSeriesListsDefaultLineWithoutURLs：info 档默认列 VIP 线路，
// 且**不带任何播放地址**（那是 links/detail 的事）。
func TestInfoSeriesListsDefaultLineWithoutURLs(t *testing.T) {
	info := NewInfo(seriesVideo())
	if info.Series == nil {
		t.Fatal("荐片内容应带 series 段")
	}
	s := info.Series
	if len(s.Lines) != 2 || s.Lines[0].Name != "VIP线路" {
		t.Fatalf("线路清单不对：%+v", s.Lines)
	}
	if len(s.Episodes) != 2 {
		t.Fatalf("默认应列 VIP 线路的 2 集，得到 %d", len(s.Episodes))
	}
	if s.Total != 2 || s.EpisodesCount != 2 || s.InLines[0] != "VIP线路" {
		t.Errorf("计数/线路归属不对：total=%d count=%d in_lines=%v", s.Total, s.EpisodesCount, s.InLines)
	}
	if s.Episodes[0].ParseID != "553300_33921" {
		t.Errorf("应给出可直接解析的 parse_id：%+v", s.Episodes[0])
	}
	if s.Episodes[0].URL != "" || s.Episodes[0].FTP != "" {
		t.Errorf("info 档不能出现播放地址：%+v", s.Episodes[0])
	}
	if s.Latest != "第2集" {
		t.Errorf("latest 应透传：%q", s.Latest)
	}
	raw, _ := json.Marshal(info)
	if strings.Contains(string(raw), ".m3u8") {
		t.Errorf("info 档的 JSON 里不该出现直链：%s", raw)
	}
}

// TestInfoSeriesHonoursQuality：quality 指定线路时列那条线路的集。
func TestInfoSeriesHonoursQuality(t *testing.T) {
	info := NewInfoFor(seriesVideo(), "线路乙")
	if len(info.Series.Episodes) != 2 || info.Series.Episodes[0].ID != "6583678" {
		t.Fatalf("应列线路乙的集：%+v", info.Series.Episodes)
	}
	if info.Series.InLines[0] != "线路乙" {
		t.Errorf("in_lines 应反映实际线路：%v", info.Series.InLines)
	}
	// qn:<序号> 也要能用
	info = NewInfoFor(seriesVideo(), "qn:2")
	if info.Series.Episodes[0].Line != "线路乙" {
		t.Errorf("qn:2 应选中第二条线路：%+v", info.Series.Episodes[0])
	}
	// 不存在的线路：退回默认，不报错（info 是只读接口，不该因为选择参数失败）
	info = NewInfoFor(seriesVideo(), "不存在的线路")
	if len(info.Series.Episodes) == 0 {
		t.Error("无效线路应退回默认线路，而不是返回空列表")
	}
}

// TestDetailSeriesCarriesURLs：detail 档的选集才带地址。
func TestDetailSeriesCarriesURLs(t *testing.T) {
	d := NewDetail(seriesVideo())
	if d.Series == nil || len(d.Series.Episodes) == 0 {
		t.Fatal("detail 应带 series")
	}
	ep := d.Series.Episodes[0]
	if ep.URL == "" || ep.FTP == "" {
		t.Errorf("detail 档应带播放地址与 ftp：%+v", ep)
	}
	if d.BestVideoURL != "https://mv/a/33921.m3u8" {
		t.Errorf("best_video_url 应是默认（VIP）线路：%q", d.BestVideoURL)
	}
}

// TestSeriesEpisodeOnlyKeepsCrossLineMatches：传了单集 ID 时，
// 即使这一集不在默认线路里，也不能被"按线路筛"筛没了。
func TestSeriesEpisodeOnlyKeepsCrossLineMatches(t *testing.T) {
	v := seriesVideo()
	v.Episodes = []core.Episode{{ID: "6583678", Name: "第1集", Line: "线路乙", LineIdx: 2, Index: 1,
		URL: "https://mv/b/6583678.m3u8", ParseID: "553300_6583678"}}
	v.EpisodeOnly = true

	info := NewInfo(v)
	if len(info.Series.Episodes) != 1 || info.Series.Episodes[0].Line != "线路乙" {
		t.Fatalf("应保留非默认线路上的那一集：%+v", info.Series.Episodes)
	}
	if len(info.Series.InLines) != 1 || info.Series.InLines[0] != "线路乙" {
		t.Errorf("in_lines 就是'这个单集属于哪条线路'的答案：%v", info.Series.InLines)
	}
}

// TestSeriesTruncation：超长剧集截断并标记。
func TestSeriesTruncation(t *testing.T) {
	v := &core.Video{
		Platform: core.PlatformJianpian,
		ID:       "1",
		Lines:    []core.Line{{Name: "唯一线路", Count: EpisodeListLimit + 5}},
	}
	for i := 0; i < EpisodeListLimit+5; i++ {
		v.Episodes = append(v.Episodes, core.Episode{ID: "e", Line: "唯一线路", Index: i + 1})
	}
	info := NewInfo(v)
	if !info.Series.Truncated {
		t.Error("超过上限应标记 truncated")
	}
	if len(info.Series.Episodes) != EpisodeListLimit {
		t.Errorf("应截断到 %d 条：%d", EpisodeListLimit, len(info.Series.Episodes))
	}
	if info.Series.Total != EpisodeListLimit+5 {
		t.Errorf("total 应是截断前的集数：%d", info.Series.Total)
	}
	if !strings.Contains(info.Warning, "截断") {
		t.Errorf("应给出可读的 warning：%q", info.Warning)
	}
}

// TestSelectByLineName：线路名就是荐片的"清晰度"，要能按名字选。
func TestSelectByLineName(t *testing.T) {
	v := seriesVideo()
	s, err := Select(v.Videos, "线路乙")
	if err != nil {
		t.Fatalf("按线路名应能选中：%v", err)
	}
	if s.Quality != "线路乙" {
		t.Errorf("选中的不对：%+v", s)
	}
	_, err = Select(v.Videos, "不存在的线路")
	if err == nil {
		t.Fatal("不存在的线路应报错")
	}
	if !strings.Contains(err.Error(), "VIP线路") {
		t.Errorf("报错应列出可用线路：%v", err)
	}
}

// TestLinksForSeriesHasBackupsAndHLS：荐片的 links 要有 HLS 标记、
// 同集其它线路作为备份、以及人能读的说明。
func TestLinksForSeriesHasBackupsAndHLS(t *testing.T) {
	links, err := NewLinks(seriesVideo(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !links.HLS {
		t.Error("m3u8 应标记 hls=true")
	}
	if links.URL != "https://mv/a/33921.m3u8" {
		t.Errorf("默认应是第一条（VIP）线路：%q", links.URL)
	}
	if len(links.BackupURLs) != 1 || links.BackupURLs[0] != "https://mv/b/6583678.m3u8" {
		t.Errorf("backup_urls 应是同集其它线路：%v", links.BackupURLs)
	}
	if links.Note == "" {
		t.Error("应给出线路/HLS 的说明")
	}
	if links.NeedsMux {
		t.Error("荐片是单文件 HLS，不需要混流")
	}
}

// TestNewSearchProjectsCostAndNext：搜索投影给出计价与翻页提示。
func TestNewSearchProjectsCostAndNext(t *testing.T) {
	out := NewSearch(&core.SearchResult{
		Platform: core.PlatformJianpian, Keyword: "太空", Page: 1, Limit: 20, Total: 2940,
		Items: []core.SearchItem{{Platform: core.PlatformJianpian, ID: "553300", Title: "太空部队",
			Score: 8, Year: 2020, Category: "电视剧", Actors: []string{"A"}}},
	})
	if out.Count != 1 || out.Items[0].Detail == "" {
		t.Errorf("投影不对：%+v", out)
	}
	if out.HasMore {
		t.Error("只返回 1 条、上限 20 时不该说还有更多")
	}

	out = NewSearch(&core.SearchResult{
		Platform: core.PlatformJianpian, Page: 2, Limit: 1, Total: 10,
		Items: []core.SearchItem{{ID: "1"}},
	})
	if !out.HasMore || !strings.Contains(out.Next, "page=3") {
		t.Errorf("翻页提示不对：%+v", out.Next)
	}
}
