// Package core 定义跨平台统一的领域模型与提取器接口。
//
// 它是依赖图的最底层叶子包：不依赖本项目的任何其他内部包，
// 也不依赖任何第三方库。所有平台提取器都实现这里的 Extractor 接口。
package core

import "sort"

// Platform 标识一个视频平台。
type Platform string

const (
	PlatformDouyin      Platform = "douyin"      // 抖音
	PlatformBilibili    Platform = "bilibili"    // 哔哩哔哩
	PlatformKuaishou    Platform = "kuaishou"    // 快手
	PlatformXiaohongshu Platform = "xiaohongshu" // 小红书
	PlatformJianpian    Platform = "jianpian"    // 荐片（影视剧集）
)

// Author 是内容作者信息。
type Author struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	Avatar string `json:"avatar,omitempty"`
}

// Image 是图集 / 实况照片中的一张图片。
type Image struct {
	URL          string `json:"url"`
	LivePhotoURL string `json:"live_photo_url,omitempty"` // 实况照片的动态部分
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
}

// Stream 是一条可播放的媒体流。
//
// 同一个 Video 既可能是"单文件直链"（抖音/快手/小红书，或 B 站的 html5 MP4），
// 也可能是 DASH 分离流（B 站，视频轨与音频轨分开，需要客户端或服务端混流）。
// 两种情况都统一用 Stream 表达。
type Stream struct {
	URL        string   `json:"url"`
	BackupURLs []string `json:"backup_urls,omitempty"`

	// Quality 是人类可读的清晰度标签，如 "1080P60"、"4K"、"原画"。
	Quality string `json:"quality,omitempty"`
	// QualityID 是平台原始的清晰度代码（B 站 qn，如 80=1080P、120=4K）。
	QualityID int `json:"quality_id,omitempty"`

	// Codec 是编码格式：avc1 / hevc / av1 / aac 等。
	Codec string `json:"codec,omitempty"`
	// MimeType 如 "video/mp4"、"audio/mp4"。
	MimeType string `json:"mime_type,omitempty"`

	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	FrameRate string `json:"frame_rate,omitempty"`
	// Bandwidth 是所需带宽（bit/s）。
	Bandwidth int `json:"bandwidth,omitempty"`
	// Size 是文件字节数，未知时为 0。
	Size int64 `json:"size,omitempty"`
	// Duration 是媒体时长（秒），未知时为 0。
	Duration float64 `json:"duration,omitempty"`

	// Headers 是播放/下载这条流时必须携带的请求头（典型是 Referer）。
	// 客户端拿到直链后若不带这些头会被 CDN 拒绝。
	Headers map[string]string `json:"headers,omitempty"`
}

// Subtitle 是一条字幕轨。
type Subtitle struct {
	Lang string `json:"lang,omitempty"` // 语言代码，如 zh-CN
	Name string `json:"name,omitempty"` // 展示名，如 "中文（自动生成）"
	URL  string `json:"url"`            // 字幕文件地址（JSON 或 SRT/VTT）
	// Format 为 json / srt / vtt / ass
	Format string `json:"format,omitempty"`
}

// Stats 是内容的互动统计。
type Stats struct {
	View     int64 `json:"view,omitempty"`
	Like     int64 `json:"like,omitempty"`
	Comment  int64 `json:"comment,omitempty"`
	Collect  int64 `json:"collect,omitempty"`
	Share    int64 `json:"share,omitempty"`
	Danmaku  int64 `json:"danmaku,omitempty"`
	Duration int   `json:"duration,omitempty"`
}

// Line 是"剧集型内容"的一条线路（一部剧可能有十几条来源线路）。
//
// 荐片把线路当作可选项暴露，**线路名就是它的"清晰度"维度**
// （与 B 站的 Height/qn 不同：线路是来源，不是像素高度）。
type Line struct {
	Name string `json:"name"`
	// Key 是平台内部的线路标识（荐片 source_key，如 back_source_list_cdn）。
	Key string `json:"key,omitempty"`
	// Count 是该线路的集数。
	Count int `json:"count,omitempty"`
	// VIP 表示这是平台标记的 VIP/蓝光优先线路（荐片匿名也可用，仍然照常返回）。
	VIP bool `json:"vip,omitempty"`
}

// Episode 是"剧集型内容"的一集（或电影的唯一一集）。
//
// ID 是平台内的**单集** ID（荐片的 source id），与 Video.ID（影片 ID）不同。
type Episode struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Line     string  `json:"line,omitempty"`
	LineIdx  int     `json:"line_index,omitempty"` // 线路序号（1 基）
	Index    int     `json:"index,omitempty"`      // 线路内集序号（1 基）
	URL      string  `json:"url,omitempty"`        // 播放地址（荐片为 m3u8）
	FTP      string  `json:"ftp,omitempty"`        // 可选的整集直链（荐片 ftp_list）
	Duration float64 `json:"duration,omitempty"`
	VIP      bool    `json:"vip,omitempty"`
	// ParseID 是"可以直接拿去解析的 ID"。
	//
	// 由提取器自己填（它最清楚自己的 ID 规则）：荐片是 `<影片ID>_<单集ID>`。
	// 放在这里而不是让接口层拼，是为了避免"ID 规则"在两处各写一份。
	ParseID string `json:"parse_id,omitempty"`
}

// Video 是一次解析的完整结果，是 API 的唯一输出模型。
type Video struct {
	Platform Platform `json:"platform"`
	// ID 是平台内的内容 ID（如抖音 aweme_id、B 站 BV 号）。
	ID    string `json:"id"`
	Title string `json:"title"`
	Desc  string `json:"desc,omitempty"`
	Cover string `json:"cover,omitempty"`

	Author Author `json:"author"`
	Stats  *Stats `json:"stats,omitempty"`

	// Videos 是视频轨，按"推荐优先"排序：Videos[0] 即最佳清晰度。
	// 单文件直链时长度为 1。
	Videos []Stream `json:"videos,omitempty"`
	// Audios 是音频轨，仅 DASH 场景非空，同样按推荐优先排序。
	Audios []Stream `json:"audios,omitempty"`
	// NeedsMux 为 true 表示视频与音频分离，需要混流才能得到完整文件。
	NeedsMux bool `json:"needs_mux,omitempty"`

	Images    []Image    `json:"images,omitempty"`
	Music     *Stream    `json:"music,omitempty"`
	Subtitles []Subtitle `json:"subtitles,omitempty"`

	// Lines / Episodes 是"剧集型内容"（荐片）的线路与选集。
	//
	// Episodes 里**只有被选中的那条线路**（默认 VIP 线路，见提取器），
	// 而不是全部线路 × 全部集——否则 detail 响应会被十几条线路撑爆；
	// 要换线路/换集就换 line/episode 参数再解析一次。
	Lines    []Line    `json:"lines,omitempty"`
	Episodes []Episode `json:"episodes,omitempty"`
	// Latest 是平台标注的更新进度（如 "第10集"），Finished 表示已完结。
	Latest   string `json:"latest,omitempty"`
	Finished bool   `json:"finished,omitempty"`
	// EpisodeOnly 表示 Episodes 里**只有被指定的那一集**（可能横跨多条线路，
	// 因为多条线路可能共用同一个单集 ID）。
	//
	// 它决定了出参怎么裁剪：false = "全部集"，投影时按请求的线路筛；
	// true = "精确到集"，投影时不再按线路筛（否则可能筛成空）。
	EpisodeOnly bool `json:"episode_only,omitempty"`

	// SourceURL 是归一化后的原始页面地址，便于回溯。
	SourceURL string `json:"source_url,omitempty"`
	// Cached 标记本次结果是否直接来自服务端缓存（由服务层填充）。
	Cached bool `json:"cached,omitempty"`
	// Warning 承载"部分成功"的提示，例如登录态缺失导致清晰度被降级。
	Warning string `json:"warning,omitempty"`
}

// Best 返回最佳视频轨；没有视频轨时返回 nil。
func (v *Video) Best() *Stream {
	if v == nil || len(v.Videos) == 0 {
		return nil
	}
	return &v.Videos[0]
}

// BestAudio 返回最佳音频轨；没有音频轨时返回 nil。
func (v *Video) BestAudio() *Stream {
	if v == nil || len(v.Audios) == 0 {
		return nil
	}
	return &v.Audios[0]
}

// SortStreams 按"质量优先"稳定排序视频轨：
// 先比像素数，再比帧率，最后比码率；同质量下 H.264 优先于 H.265/AV1，
// 因为 H.264 的浏览器兼容性最好。
//
// 各平台提取器在填充 Videos 后调用它即可保证 Videos[0] 是最佳选择。
func SortStreams(streams []Stream) {
	sort.SliceStable(streams, func(i, j int) bool {
		a, b := streams[i], streams[j]
		if pa, pb := a.Width*a.Height, b.Width*b.Height; pa != pb {
			return pa > pb
		}
		if fa, fb := parseFrameRate(a.FrameRate), parseFrameRate(b.FrameRate); fa != fb {
			return fa > fb
		}
		if ra, rb := codecRank(a.Codec), codecRank(b.Codec); ra != rb {
			return ra < rb
		}
		return a.Bandwidth > b.Bandwidth
	})
}

// codecRank 越小越优先：H.264 > H.265 > AV1 > 未知。
func codecRank(codec string) int {
	switch {
	case hasPrefixFold(codec, "avc"), hasPrefixFold(codec, "h264"):
		return 0
	case hasPrefixFold(codec, "hev"), hasPrefixFold(codec, "h265"):
		return 1
	case hasPrefixFold(codec, "av01"), hasPrefixFold(codec, "av1"):
		return 2
	default:
		return 3
	}
}

func parseFrameRate(s string) float64 {
	var whole, frac float64
	var seen, inFrac bool
	var div float64 = 1
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			if inFrac {
				div *= 10
				frac += float64(r-'0') / div
			} else {
				whole = whole*10 + float64(r-'0')
			}
			seen = true
		case r == '.':
			inFrac = true
		default:
			if seen {
				return whole + frac
			}
		}
	}
	return whole + frac
}

func hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		c, p := s[i], prefix[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if p >= 'A' && p <= 'Z' {
			p += 'a' - 'A'
		}
		if c != p {
			return false
		}
	}
	return true
}
