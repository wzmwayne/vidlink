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
func NewInfo(v *core.Video) Info {
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

// NewDetail 把领域模型投影成 detail 档。
func NewDetail(v *core.Video) Detail {
	if v == nil {
		return Detail{}
	}
	out := Detail{
		Info:     NewInfo(v),
		Videos:   NewStreams(v.Videos),
		Audios:   NewStreams(v.Audios),
		NeedsMux: v.NeedsMux,
	}
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
	return out, nil
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

	// 纯数字或带 P 后缀 → 按高度。注意：按高度匹配时可能有多条（不同编码），
	// 已排序的列表保证第一条是兼容性最好的那个。
	h, err := strconv.Atoi(strings.TrimRight(strings.ToUpper(want), "P"))
	if err != nil {
		return nil, fmt.Errorf("无法识别的清晰度参数 %q：可用形如 1080、720P 或 qn:80", want)
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
