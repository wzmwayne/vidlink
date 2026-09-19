// Package tier 把解析结果裁剪成三个配额计量档位的出参。
//
// 三档是**互不重叠的三类出参**，而不是同一份数据的三个详略程度：
//
//	info   元信息 + 可选档位列表，**不含任何直链**
//	links  **只有直链**，不含任何内容元信息
//	detail 元信息 + 全部档位的直链（= info ∪ links 的内容）
//
// 为什么 links 要刻意"什么都不给"
//
// 因为它是配额档位的中档。若 links 顺手带上标题作者，调用方就没有理由升到
// detail；而若 info 顺手带上直链，links 就完全没有存在意义。
// 三个档位的内容边界就是系数差异本身，所以这里的字段是**按档位边界刻意裁剪**的，
// 不是"少返回一点省带宽"。
//
// 交付必需的字段不受此限：headers（防盗链）、audio_url（DASH 分离流）、
// backup_urls（CDN 容灾）都必须在——少了它们，客户拿到链接也用不了。
package tier

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"vidlink/internal/core"
)

// Author 是作者信息。
type Author struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	Avatar string `json:"avatar,omitempty"`
}

// Stats 是互动统计。
type Stats struct {
	View     int64 `json:"view,omitempty"`
	Like     int64 `json:"like,omitempty"`
	Comment  int64 `json:"comment,omitempty"`
	Collect  int64 `json:"collect,omitempty"`
	Share    int64 `json:"share,omitempty"`
	Danmaku  int64 `json:"danmaku,omitempty"`
	Duration int   `json:"duration,omitempty"`
}

// Image 是图集里的一张图。
type Image struct {
	URL          string `json:"url"`
	LivePhotoURL string `json:"live_photo_url,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
}

// Subtitle 是一条字幕轨。
type Subtitle struct {
	Lang   string `json:"lang,omitempty"`
	Name   string `json:"name,omitempty"`
	URL    string `json:"url"`
	Format string `json:"format,omitempty"`
}

// Stream 是输出用的流描述（core.Stream 的对外投影）。
type Stream struct {
	URL        string            `json:"url"`
	BackupURLs []string          `json:"backup_urls,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`

	Quality   string `json:"quality,omitempty"`
	QualityID int    `json:"quality_id,omitempty"`
	Codec     string `json:"codec,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`

	Width     int     `json:"width,omitempty"`
	Height    int     `json:"height,omitempty"`
	FrameRate string  `json:"frame_rate,omitempty"`
	Bandwidth int     `json:"bandwidth,omitempty"`
	Size      int64   `json:"size,omitempty"`
	Duration  float64 `json:"duration,omitempty"`
}

// Quality 是一个**可选档位的描述**，刻意不含直链。
//
// 它让调用方能先按 info 档（0.5）问"这条视频有哪些清晰度"，
// 再决定按 links 档（1.0）要哪一档。这是 info 与 links 之间的自然衔接，
// 也解释了为什么 info 值得单独设一个系数。
type Quality struct {
	Height    int      `json:"height"`
	Label     string   `json:"label,omitempty"`
	QualityID int      `json:"quality_id,omitempty"`
	Codecs    []string `json:"codecs,omitempty"`
}

// Line 是剧集型内容的一条线路。
//
// 荐片把线路当作"清晰度"维度暴露（不同来源、质量与可用性差异极大），
// 所以线路名既能出现在 qualities 之外的 series.lines 里，也能用于 quality 选择。
type Line struct {
	Name  string `json:"name"`
	Key   string `json:"key,omitempty"`
	Count int    `json:"count,omitempty"`
	VIP   bool   `json:"vip,omitempty"`
}

// Episode 是剧集型内容的一集。
//
// info 档只给 ID/名称/可直接解析的 parse_id（不含 URL）；detail 档才带上播放地址。
type Episode struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Line     string  `json:"line,omitempty"`
	LineIdx  int     `json:"line_index,omitempty"`
	Index    int     `json:"index,omitempty"`
	URL      string  `json:"url,omitempty"`
	FTP      string  `json:"ftp,omitempty"`
	Duration float64 `json:"duration,omitempty"`
	VIP      bool    `json:"vip,omitempty"`
	// ParseID 是可以直接拿去 /v1/links、/v1/detail 的 ID（剧集型平台如 `<影片ID>_<单集ID>`）。
	ParseID string `json:"parse_id,omitempty"`
}

// Series 是剧集型内容的线路/选集结构（荐片专有，其它平台为空）。
type Series struct {
	Lines []Line `json:"lines"`
	// Episodes 默认是**默认（VIP）线路**的全部集；用 quality 指定线路时是那条线路的集；
	// 传入 `<影片ID>_<单集ID>` 时只有这一集（可能横跨多条线路）。
	Episodes []Episode `json:"episodes,omitempty"`
	Latest   string    `json:"latest,omitempty"`
	Finished bool      `json:"finished,omitempty"`
	// Total 是所选线路的集数（截断前），EpisodesCount 是本次返回的条数。
	Total         int  `json:"total,omitempty"`
	EpisodesCount int  `json:"episodes_count,omitempty"`
	Truncated     bool `json:"truncated,omitempty"`
	// InLines 列出本次返回的这些集存在于哪些线路里。
	//
	// 传入 `<影片ID>_<单集ID>` 时它就是"这个单集属于哪条/哪几条线路"的答案：
	// 实测多条线路可能共用同一个单集 ID，所以答案可能不止一条。
	InLines []string `json:"in_lines,omitempty"`
}

// EpisodeListLimit 是单次返回的选集条数上限（长剧/动漫可能上千集）。
const EpisodeListLimit = 2000

// Info 是 info 档的响应。
type Info struct {
	Platform  string    `json:"platform"`
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Desc      string    `json:"desc,omitempty"`
	Cover     string    `json:"cover,omitempty"`
	Author    Author    `json:"author"`
	Stats     *Stats    `json:"stats,omitempty"`
	SourceURL string    `json:"source_url,omitempty"`
	Qualities []Quality `json:"qualities,omitempty"`
	Images    []Image   `json:"images,omitempty"`
	Series    *Series   `json:"series,omitempty"`
	Warning   string    `json:"warning,omitempty"`
	Cached    bool      `json:"cached,omitempty"`
}

// Links 是 links 档的响应。
//
// **只有把链接用起来所必需的字段**：url 本身、防盗链所需的 headers、
// DASH 场景的 audio_url、以及 CDN 容灾用的 backup_urls。
// 标题、作者、统计、封面一律不给——那些属于 detail 档。
type Links struct {
	URL        string            `json:"url"`
	BackupURLs []string          `json:"backup_urls,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	AudioURL   string            `json:"audio_url,omitempty"`
	NeedsMux   bool              `json:"needs_mux,omitempty"`
	// HLS 表示 url 是 m3u8 播放列表（HLS），不是单个媒体文件。
	//
	// 必须显式告知：客户端拿到 url 要判断"能不能直接下/能不能直接喂 <video>"。
	// 荐片的播放列表还可能是 AES-128 加密的，密钥在播放列表内、由客户端现场取。
	HLS bool `json:"hls,omitempty"`
	// Note 是人能读的补充说明（荐片用来说明线路/加密/下载方式）。
	Note string `json:"note,omitempty"`
}

// Detail 是 detail 档的响应：info 的全部内容 + 全部档位的直链。
type Detail struct {
	Info
	Videos           []Stream          `json:"videos"`
	Audios           []Stream          `json:"audios,omitempty"`
	NeedsMux         bool              `json:"needs_mux,omitempty"`
	Music            *Stream           `json:"music,omitempty"`
	Subtitles        []Subtitle        `json:"subtitles,omitempty"`
	BestVideoURL     string            `json:"best_video_url,omitempty"`
	BestVideoHeaders map[string]string `json:"best_video_headers,omitempty"`
	BestAudioURL     string            `json:"best_audio_url,omitempty"`
}

// --- 构造 ---

// NewInfo 把领域模型投影成 info 档。
func NewInfo(v *core.Video) Info { return NewInfoFor(v, "") }

// NewInfoFor 在 info 档上指定"要列哪条线路的选集"（空 = 默认/VIP 线路）。
func NewInfoFor(v *core.Video, line string) Info {
	if v == nil {
		return Info{}
	}
	out := Info{
		Platform:  string(v.Platform),
		ID:        v.ID,
		Title:     v.Title,
		Desc:      v.Desc,
		Cover:     v.Cover,
		SourceURL: v.SourceURL,
		Warning:   v.Warning,
		Cached:    v.Cached,
		Author:    Author{ID: v.Author.ID, Name: v.Author.Name, Avatar: v.Author.Avatar},
		Qualities: Qualities(v),
		Series:    newSeries(v, false, line),
	}
	if out.Series != nil && out.Series.Truncated {
		out.Warning = appendWarning(out.Warning,
			fmt.Sprintf("该线路集数超过 %d 条，已截断", EpisodeListLimit))
	}
	if v.Stats != nil {
		out.Stats = &Stats{
			View: v.Stats.View, Like: v.Stats.Like, Comment: v.Stats.Comment,
			Collect: v.Stats.Collect, Share: v.Stats.Share,
			Danmaku: v.Stats.Danmaku, Duration: v.Stats.Duration,
		}
	}
	for _, im := range v.Images {
		out.Images = append(out.Images, Image{
			URL: im.URL, LivePhotoURL: im.LivePhotoURL, Width: im.Width, Height: im.Height,
		})
	}
	return out
}

// newSeries 把剧集型结构投影出来。
//
//	withURLs=false  info 档：只给 ID/集名/parse_id，不给播放地址
//	withURLs=true   detail 档：带上播放地址（m3u8）与 ftp 直链
//	line            要列哪条线路的选集；空 = 默认（VIP）线路
//
// 两种裁剪规则：
//
//	v.EpisodeOnly == false：Episodes 里是"全部线路 × 全部集"，按 line 筛；
//	v.EpisodeOnly == true ：Episodes 里已被提取器收敛成"指定的那一集"（可能横跨
//	  多条线路），此时**不能**再按线路筛（否则那条只存在于非默认线路的集会消失），
//	  只有在显式指定 line 且能筛出结果时才筛。
func newSeries(v *core.Video, withURLs bool, line string) *Series {
	if v == nil || (len(v.Lines) == 0 && len(v.Episodes) == 0) {
		return nil
	}
	out := &Series{Latest: v.Latest, Finished: v.Finished}
	for _, ln := range v.Lines {
		out.Lines = append(out.Lines, Line{Name: ln.Name, Key: ln.Key, Count: ln.Count, VIP: ln.VIP})
	}
	if out.Lines == nil {
		out.Lines = []Line{}
	}

	eps := v.Episodes
	want := pickLine(v, line)
	if want != "" {
		if filtered := filterByLine(eps, want); len(filtered) > 0 {
			eps = filtered
		} else if !v.EpisodeOnly {
			// 指定了一条不存在的线路：退回默认线路，而不是返回空列表。
			// info/detail 是只读接口，"选择失败"应该由 links 的 Select 报错，
			// 在这里报错只会让"看线路列表"这种无害请求也失败。
			eps = filterByLine(eps, firstLine(v))
		}
	} else if !v.EpisodeOnly {
		eps = filterByLine(eps, "")
	}
	// Total 是"这条线路一共有多少集"（截断前）；拿不到线路信息时退化为本次条数。
	out.Total = len(eps)
	if n := lineCount(v, want); n > 0 {
		out.Total = n
	}

	if len(eps) > EpisodeListLimit {
		eps = eps[:EpisodeListLimit]
		out.Truncated = true
	}
	seen := map[string]bool{}
	for _, ep := range eps {
		item := Episode{
			ID: ep.ID, Name: ep.Name, Line: ep.Line,
			LineIdx: ep.LineIdx, Index: ep.Index, VIP: ep.VIP,
			Duration: ep.Duration, ParseID: ep.ParseID,
		}
		if withURLs {
			item.URL = ep.URL
			item.FTP = ep.FTP
		}
		out.Episodes = append(out.Episodes, item)
		if ep.Line != "" && !seen[ep.Line] {
			seen[ep.Line] = true
			out.InLines = append(out.InLines, ep.Line)
		}
	}
	out.EpisodesCount = len(out.Episodes)
	return out
}

// pickLine 决定要列哪条线路：显式指定优先，否则默认（VIP 优先的）第一条。
func pickLine(v *core.Video, line string) string {
	if line != "" {
		// qn:<序号> 与线路名两种写法都支持，复用 Select 的匹配口径。
		if rest, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(line)), "qn:"); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
				for _, ln := range v.Lines {
					if ln.Name != "" && lineIndex(v, ln.Name) == n {
						return ln.Name
					}
				}
			}
			return line
		}
		for _, ln := range v.Lines {
			if strings.EqualFold(ln.Name, strings.TrimSpace(line)) {
				return ln.Name
			}
		}
		return line
	}
	if len(v.Lines) > 0 {
		return v.Lines[0].Name
	}
	return ""
}

// firstLine 返回第一条线路（VIP 优先，由提取器保证顺序）的名字。
func firstLine(v *core.Video) string {
	if len(v.Lines) > 0 {
		return v.Lines[0].Name
	}
	return ""
}

// lineIndex 返回线路名的 1 基序号。
func lineIndex(v *core.Video, name string) int {
	for i, ln := range v.Lines {
		if strings.EqualFold(ln.Name, name) {
			return i + 1
		}
	}
	return 0
}

// lineCount 返回某条线路声明的集数。
func lineCount(v *core.Video, name string) int {
	for _, ln := range v.Lines {
		if strings.EqualFold(ln.Name, name) {
			return ln.Count
		}
	}
	return 0
}

// filterByLine 按线路名筛选集；want 为空时按第一条线路筛。
func filterByLine(eps []core.Episode, want string) []core.Episode {
	out := make([]core.Episode, 0, len(eps))
	for _, ep := range eps {
		if want == "" || strings.EqualFold(ep.Line, want) {
			out = append(out, ep)
		}
	}
	return out
}

// appendWarning 拼接警告文案（保持先出现的在前，避免覆盖）。
func appendWarning(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "；" + add
}

// NewDetail 把领域模型投影成 detail 档。
func NewDetail(v *core.Video) Detail { return NewDetailFor(v, "") }

// NewDetailFor 在 detail 档上指定要展开哪条线路的选集（空 = 默认/VIP 线路）。
func NewDetailFor(v *core.Video, line string) Detail {
	if v == nil {
		return Detail{}
	}
	out := Detail{
		Info:     NewInfoFor(v, line),
		Videos:   NewStreams(v.Videos),
		Audios:   NewStreams(v.Audios),
		NeedsMux: v.NeedsMux,
	}
	out.Series = newSeries(v, true, line)
	if v.Music != nil {
		s := NewStream(*v.Music)
		out.Music = &s
	}
	for _, sub := range v.Subtitles {
		out.Subtitles = append(out.Subtitles, Subtitle{
			Lang: sub.Lang, Name: sub.Name, URL: sub.URL, Format: sub.Format,
		})
	}
	if b := v.Best(); b != nil {
		out.BestVideoURL = b.URL
		if len(b.Headers) > 0 {
			out.BestVideoHeaders = b.Headers
		}
	}
	if a := v.BestAudio(); a != nil {
		out.BestAudioURL = a.URL
	}
	return out
}

// NewLinks 把领域模型投影成 links 档。
//
// want 为清晰度选择：
//
//	""            默认最高
//	"720"/"720P"  按高度（不区分大小写）
//	"qn:64"       按平台原始代码
//
// 找不到精确匹配时返回错误并列出可选档位，而不是"就近降级"——
// 客户付了钱，拿到的必须是他要的那一档。静默降级会变成投诉。
func NewLinks(v *core.Video, want string) (Links, error) {
	if v == nil {
		return Links{}, fmt.Errorf("解析结果为空")
	}
	if len(v.Videos) == 0 {
		return Links{}, fmt.Errorf("该内容没有可用的视频流（可能是图文作品，请用 detail 档）")
	}
	s, err := Select(v.Videos, want)
	if err != nil {
		return Links{}, err
	}
	out := Links{
		URL:        s.URL,
		BackupURLs: s.BackupURLs,
		Headers:    s.Headers,
		NeedsMux:   v.NeedsMux,
	}
	if a := v.BestAudio(); a != nil {
		out.AudioURL = a.URL
	}
	if strings.Contains(s.MimeType, "mpegurl") {
		out.HLS = true
	}
	// 剧集型内容（荐片）：同一条流的"备份"就是同集的其他线路。
	// 只有 Lines 非空时才这么算——B 站的多档位是不同清晰度，不是容灾备份。
	if len(v.Lines) > 0 {
		out.BackupURLs = lineBackups(v.Videos, s.URL, 5)
		out.Note = "HLS 播放列表（m3u8）：可交给 ffmpeg/VLC/mpv；浏览器需要 HLS 播放器。" +
			"线路之间是替代关系（可用 quality=<线路名> 选择），个别线路可能失效，" +
			"失败时请换 backup_urls 里的线路。"
	}
	return out, nil
}

// lineBackups 返回同一集在其它线路上的播放地址（最多 n 条）。
func lineBackups(streams []core.Stream, chosen string, n int) []string {
	var out []string
	for i := range streams {
		if streams[i].URL == "" || streams[i].URL == chosen {
			continue
		}
		out = append(out, streams[i].URL)
		if len(out) >= n {
			break
		}
	}
	return out
}

// Select 按 want 从流列表里挑一条。
//
// 入参 streams 假定已按"推荐优先"排好（core.SortStreams），
// 所以 want 为空时直接取第一条。
func Select(streams []core.Stream, want string) (*core.Stream, error) {
	if len(streams) == 0 {
		return nil, fmt.Errorf("没有可用的视频流")
	}
	want = strings.TrimSpace(want)
	if want == "" {
		return &streams[0], nil
	}

	// qn:<原始代码>
	if rest, ok := strings.CutPrefix(strings.ToLower(want), "qn:"); ok {
		if qn, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
			for i := range streams {
				if streams[i].QualityID == qn {
					return &streams[i], nil
				}
			}
			return nil, notFoundErr(want, streams)
		}
	}

	// 标签匹配：荐片把"线路"当作清晰度维度（不同来源，质量差异极大），
	// 所以 quality 也要能按线路名选。放在数字解析之前，避免
	// "VIP线路" 被当成"无法识别的清晰度参数"直接拒掉。
	for i := range streams {
		if streams[i].Quality != "" && strings.EqualFold(strings.TrimSpace(streams[i].Quality), want) {
			return &streams[i], nil
		}
	}

	// 纯数字或带 P 后缀 → 按高度。注意：按高度匹配时可能有多条（不同编码），
	// 已排序的列表保证第一条是兼容性最好的那个。
	h, err := strconv.Atoi(strings.TrimRight(strings.ToUpper(want), "P"))
	if err != nil {
		// 列出现有档位/线路，比一句"参数无法识别"有用得多。
		return nil, notFoundErr(want, streams)
	}
	for i := range streams {
		if streams[i].Height == h {
			return &streams[i], nil
		}
	}
	return nil, notFoundErr(want, streams)
}

// notFoundErr 构造带可选档位列表的错误。
func notFoundErr(want string, streams []core.Stream) error {
	var opts []string
	seen := map[int]bool{}
	for _, s := range streams {
		if s.Height > 0 && !seen[s.Height] {
			seen[s.Height] = true
			opts = append(opts, strconv.Itoa(s.Height)+"P")
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(opts)))
	if len(opts) == 0 {
		// 没有像素高度（荐片的线路就没有）：列出线路名，别让用户猜。
		var labels []string
		seenLabel := map[string]bool{}
		for _, s := range streams {
			if s.Quality != "" && !seenLabel[s.Quality] {
				seenLabel[s.Quality] = true
				labels = append(labels, s.Quality)
			}
		}
		if len(labels) > 0 {
			return fmt.Errorf("没有 %s 这一档；可用线路：%s", want, strings.Join(labels, " / "))
		}
		return fmt.Errorf("没有 %s 这一档；该内容没有可用的清晰度列表", want)
	}
	return fmt.Errorf("没有 %s 这一档；可用档位：%s", want, strings.Join(opts, ", "))
}

// Qualities 汇总某视频有哪些档位（按高度去重、从高到低）。
func Qualities(v *core.Video) []Quality {
	if v == nil {
		return nil
	}
	type acc struct {
		q      Quality
		codecs map[string]bool
	}
	byHeight := map[int]*acc{}
	var order []int

	for _, s := range v.Videos {
		if s.Height <= 0 {
			continue
		}
		a, ok := byHeight[s.Height]
		if !ok {
			a = &acc{
				q: Quality{
					Height:    s.Height,
					Label:     s.Quality,
					QualityID: s.QualityID,
				},
				codecs: map[string]bool{},
			}
			byHeight[s.Height] = a
			order = append(order, s.Height)
		}
		if s.Codec != "" && !a.codecs[s.Codec] {
			a.codecs[s.Codec] = true
			a.q.Codecs = append(a.q.Codecs, s.Codec)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(order)))

	out := make([]Quality, 0, len(order))
	for _, h := range order {
		q := byHeight[h].q
		sort.Strings(q.Codecs)
		out = append(out, q)
	}
	return out
}

// NewStreams 投影一组流；Headers 为空时省略而不是输出 `{}`。
func NewStreams(in []core.Stream) []Stream {
	if len(in) == 0 {
		return nil
	}
	out := make([]Stream, 0, len(in))
	for _, s := range in {
		out = append(out, NewStream(s))
	}
	return out
}

// NewStream 投影单条流。
func NewStream(s core.Stream) Stream {
	return Stream{
		URL:        s.URL,
		BackupURLs: s.BackupURLs,
		Headers:    s.Headers,
		Quality:    s.Quality,
		QualityID:  s.QualityID,
		Codec:      s.Codec,
		MimeType:   s.MimeType,
		Width:      s.Width,
		Height:     s.Height,
		FrameRate:  s.FrameRate,
		Bandwidth:  s.Bandwidth,
		Size:       s.Size,
		Duration:   s.Duration,
	}
}
