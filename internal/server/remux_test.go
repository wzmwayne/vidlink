package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// 这一组测试管的是"页面里自己写的 TS → MP4 转封装"。
//
// 为什么值得端到端跑 ffmpeg：MP4 的索引（stts/ctts/stsz/co64/stss）写错了，
// 产物在浏览器里往往"还能播"，只是某些播放器卡住、拖不动进度、或者音画错位——
// 靠手点几乎发现不了。这里的做法是：拿 ffmpeg 生成真实 TS → 用页面里那段
// 纯逻辑转封装 → 再让 ffmpeg 解码两边，比较**解码后的字节**是否一模一样。
// 哈希一致就等价于"每一个 sample 的数据、顺序、时间戳都没错"。

// remuxCore 抽出解析页里标记的纯逻辑块（不碰 DOM/OPFS，所以能在 node 里跑）。
func remuxCore(t *testing.T) string {
	t.Helper()
	ui := string(uiHTML)
	begin := strings.Index(ui, "// ==TS-REMUX-BEGIN==")
	end := strings.Index(ui, "// ==TS-REMUX-END==")
	if begin < 0 || end < 0 || end <= begin {
		t.Fatal("解析页里找不到 TS 转封装的标记块（==TS-REMUX-BEGIN/END==）")
	}
	return ui[begin:end]
}

// remuxDriver 是 node 侧的驱动：读 TS → 跑转封装 → 写 MP4 → 打印索引信息。
const remuxDriver = `
(async () => {
  const ts = new Uint8Array(fs.readFileSync(process.argv[2]));
  const parts = [];
  let written = 0;
  const rx = createTSRemuxer((b) => { parts.push(b); written += b.byteLength; });
  const CH = 64 * 1024;                      // 故意用小分块：跨块边界的包头/重同步也要过
  for (let off = 0; off < ts.length; off += CH) {
    await rx.push(ts.subarray(off, Math.min(off + CH, ts.length)));
  }
  const info = await rx.finish();
  if (info.error) { console.log(JSON.stringify({ error: info.error })); return; }
  const ftyp = buildFTYP();
  const mdatStart = ftyp.length + 16;
  const moov = buildRemuxMoov(info, mdatStart);
  const out = Buffer.concat([Buffer.from(ftyp), Buffer.from(mdatHeader(written))]
    .concat(parts.map((b) => Buffer.from(b))).concat([Buffer.from(moov)]));
  fs.writeFileSync(process.argv[3], out);
  console.log(JSON.stringify({
    error: "", bytes: written, file: out.length,
    v: info.video ? info.video.count : 0, a: info.audio ? info.audio.count : 0,
    w: info.video ? info.video.width : 0, h: info.video ? info.video.height : 0,
    durMs: info.durationMs, packets: info.packets, tsBytes: info.tsBytes,
  }));
})();
`

type remuxInfo struct {
	Error   string `json:"error"`
	Bytes   int    `json:"bytes"`
	File    int    `json:"file"`
	V       int    `json:"v"`
	A       int    `json:"a"`
	W       int    `json:"w"`
	H       int    `json:"h"`
	DurMs   int    `json:"durMs"`
	Packets int    `json:"packets"`
	TSBytes int    `json:"tsBytes"`
}

// runRemux 在 node 里把 core 跑一遍。返回驱动输出（失败即 t.Fatal）。
func runRemux(t *testing.T, core, tsPath, mp4Path string) remuxInfo {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("环境里没有 node，跳过")
	}
	js := filepath.Join(t.TempDir(), "remux.js")
	if err := os.WriteFile(js, []byte("const fs = require(\"fs\");\n"+core+remuxDriver), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(node, js, tsPath, mp4Path).CombinedOutput()
	if err != nil {
		t.Fatalf("node 转封装失败：%v\n%s", err, raw)
	}
	var got remuxInfo
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("驱动输出不是 JSON：%v\n%s", err, raw)
	}
	return got
}

func ffmpegPath(t *testing.T) (string, string) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("环境里没有 ffmpeg，跳过")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("环境里没有 ffprobe，跳过")
	}
	return ffmpeg, ffprobe
}

func ffprobeStream(t *testing.T, ffprobe, path, stream string, entries string) map[string]string {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-count_packets", "-select_streams", stream,
		"-show_entries", "stream="+entries, "-of", "default=nw=1", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe %s 失败：%v\n%s", path, err, out)
	}
	got := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return got
}

func ffprobeInt(t *testing.T, ffprobe, path, stream, entry string) int {
	t.Helper()
	v := ffprobeStream(t, ffprobe, path, stream, entry)[entry]
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("ffprobe %s 的 %s 不是数字：%q", path, entry, v)
	}
	return n
}

// decodeHash 让 ffmpeg 解码成裸流再取哈希：这是"转封装有没有搬错字节"的判据。
func decodeHash(t *testing.T, ffmpeg, path, kind string) (string, int) {
	t.Helper()
	args := []string{"-v", "error", "-i", path}
	switch kind {
	case "video":
		args = append(args, "-map", "0:v:0", "-an", "-fps_mode", "passthrough",
			"-f", "rawvideo", "-pix_fmt", "rgb24", "-")
	case "audio":
		args = append(args, "-map", "0:a:0", "-vn", "-f", "s16le", "-")
	default:
		t.Fatalf("未知的解码类型 %q", kind)
	}
	var out, errOut bytes.Buffer
	cmd := exec.Command(ffmpeg, args...)
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg 解码 %s（%s）失败：%v\n%s", path, kind, err, errOut.String())
	}
	if out.Len() == 0 {
		t.Fatalf("ffmpeg 解码 %s（%s）没有产出数据", path, kind)
	}
	sum := sha256.Sum256(out.Bytes())
	return hex.EncodeToString(sum[:]), out.Len()
}

// h264TS 用 ffmpeg 造一段真实 TS：320x240@15、High profile + 2 个 B 帧
// （B 帧是重点：没有它 ctts 根本不会被走到）、44.1kHz 单声道 AAC。
func h264TS(t *testing.T, ffmpeg string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.ts")
	cmd := exec.Command(ffmpeg, "-hide_banner", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=15:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=2",
		"-c:v", "libx264", "-profile:v", "high", "-bf", "2", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "64k", "-f", "mpegts", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("ffmpeg 生成测试 TS 失败（可能没编 libx264）：%v\n%s", err, out)
	}
	return path
}

// TestUITSMp4RemuxE2E 拿真实 TS 跑转封装，再用 ffmpeg 校验：
//  1. 帧数与源 TS 完全一致（不多不少）；
//  2. stsd 的宽高就是 SPS 里的宽高；
//  3. 解码后的视频/音频裸流哈希与源 TS 一模一样（逐字节等价）；
//  4. 产物能被 ffmpeg 完整解码（-v error 下无任何报错）。
func TestUITSMp4RemuxE2E(t *testing.T) {
	core := remuxCore(t)
	ffmpeg, ffprobe := ffmpegPath(t)
	tsPath := h264TS(t, ffmpeg)
	mp4Path := filepath.Join(t.TempDir(), "out.mp4")

	info := runRemux(t, core, tsPath, mp4Path)
	if info.Error != "" {
		t.Fatalf("转封装报错：%s", info.Error)
	}

	srcV := ffprobeInt(t, ffprobe, tsPath, "v:0", "nb_read_packets")
	srcA := ffprobeInt(t, ffprobe, tsPath, "a:0", "nb_read_packets")
	if info.V != srcV {
		t.Errorf("视频帧数 = %d，源 TS 是 %d", info.V, srcV)
	}
	if info.A != srcA {
		t.Errorf("音频帧数 = %d，源 TS 是 %d", info.A, srcA)
	}
	dims := ffprobeStream(t, ffprobe, tsPath, "v:0", "width,height")
	want, _ := strconv.Atoi(dims["width"])
	hant, _ := strconv.Atoi(dims["height"])
	if info.W != want || info.H != hant {
		t.Errorf("stsd 宽高 = %dx%d，SPS/源流是 %dx%d", info.W, info.H, want, hant)
	}

	// 产物本身的结构与解码能力
	got := ffprobeStream(t, ffprobe, mp4Path, "v:0", "codec_name,width,height,nb_read_packets")
	if got["codec_name"] != "h264" {
		t.Errorf("视频编码 = %q，想要 h264", got["codec_name"])
	}
	if got["width"] != dims["width"] || got["height"] != dims["height"] {
		t.Errorf("ffprobe 读到的宽高 = %sx%s，想要 %sx%s", got["width"], got["height"], dims["width"], dims["height"])
	}
	if got["nb_read_packets"] != strconv.Itoa(srcV) {
		t.Errorf("MP4 视频包数 = %s，源 TS 是 %d", got["nb_read_packets"], srcV)
	}
	if a := ffprobeStream(t, ffprobe, mp4Path, "a:0", "codec_name,sample_rate,channels"); a["codec_name"] != "aac" ||
		a["sample_rate"] != "44100" || a["channels"] != "1" {
		t.Errorf("音频流信息不对：%v", a)
	}

	// 逐字节校验：转封装不该动任何一帧的比特
	srcHash, _ := decodeHash(t, ffmpeg, tsPath, "video")
	outHash, _ := decodeHash(t, ffmpeg, mp4Path, "video")
	if srcHash != outHash {
		t.Errorf("解码后的视频不一致：转封装动了比特流（源 %s ≠ 产物 %s）", srcHash[:12], outHash[:12])
	}
	srcAHash, _ := decodeHash(t, ffmpeg, tsPath, "audio")
	outAHash, _ := decodeHash(t, ffmpeg, mp4Path, "audio")
	if srcAHash != outAHash {
		t.Errorf("解码后的音频不一致：源 %s ≠ 产物 %s", srcAHash[:12], outAHash[:12])
	}
}

// TestUITSMp4RemuxRejectsForeignCodecs：不支持的编码要给出**说得清**的报错，
// 而不是产出一个能"封"但播不了的 MP4（HEVC）或丢掉音轨（AC-3）。
func TestUITSMp4RemuxRejectsForeignCodecs(t *testing.T) {
	core := remuxCore(t)
	ffmpeg, _ := ffmpegPath(t)

	hevc := filepath.Join(t.TempDir(), "hevc.ts")
	cmd := exec.Command(ffmpeg, "-hide_banner", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=0.5",
		"-c:v", "libx265", "-x265-params", "log-level=error", "-f", "mpegts", hevc)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("没有 libx265，跳过 HEVC 拒绝用例：%v\n%s", err, out)
	}
	info := runRemux(t, core, hevc, filepath.Join(t.TempDir(), "out.mp4"))
	if info.Error == "" {
		t.Fatal("HEVC 应当被拒绝，但转封装成功了")
	}
	if !strings.Contains(info.Error, "HEVC") {
		t.Errorf("HEVC 的报错里应说清编码名，实际：%q", info.Error)
	}

	// 非 TS 数据（全零）也不能悄悄产出一个空壳 MP4
	junk := filepath.Join(t.TempDir(), "junk.bin")
	if err := os.WriteFile(junk, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := runRemux(t, core, junk, filepath.Join(t.TempDir(), "junk.mp4")); got.Error == "" {
		t.Error("全零输入应当被拒绝（没有 PMT），但转封装成功了")
	}
}

// TestUITSMp4RemuxNoNameClash：转封装块的**顶层函数名**不能与页面其它脚本重名。
//
// 这是踩过的坑：块里曾经也有一个 `buildMoov`，而 DASH 混流器早就有一个同名的。
// JS 的函数声明会被提升、后声明的覆盖先声明的——于是新写的转封装在不知不觉中
// 把老的混流器换掉了，语法检查与接线检查全绿，只有真去合 DASH 才会发现。
//
// 只查函数声明：顶层 `const`/`let` 重名是语法错误，TestUIScriptsParse 已经拦住了。
func TestUITSMp4RemuxNoNameClash(t *testing.T) {
	core := remuxCore(t)
	full := string(uiHTML)
	outside := strings.Replace(full, core, "", 1)
	if outside == full {
		t.Fatal("标记块没有被摘出来")
	}
	// 顶层声明都缩进 2 个空格；函数体内的嵌套函数缩进更深，不参与判重。
	declRe := regexp.MustCompile(`(?m)^  function\s+([A-Za-z_$][\w$]*)\s*\(`)
	found := declRe.FindAllStringSubmatch(core, -1)
	if len(found) < 15 {
		t.Fatalf("只解析出 %d 个顶层函数，正则大概失效了", len(found))
	}
	for _, m := range found {
		name := m[1]
		if strings.Contains(outside, "\n  function "+name+"(") {
			t.Errorf("转封装块里的 %q 与页面其它脚本重名：同作用域下后声明会覆盖先声明", name)
		}
	}
}

// TestUITSMp4RemuxWiring：界面接线——按钮只在有 .ts 时出现、转封装**不自动**跑、
// 且不引任何外部库（这一段的全部价值就在于零依赖）。
func TestUITSMp4RemuxWiring(t *testing.T) {
	ui := string(uiHTML)
	for _, want := range []string{
		"// ==TS-REMUX-BEGIN==", "// ==TS-REMUX-END==",
		`id="muxRemux"`, `转成 MP4（不重编码）`,
		`function createTSRemuxer(`, `function buildRemuxMoov(`, `function buildFTYP(`,
		`function parseSPS(`, `function mdatHeader(`, `function setRemuxAvailable(`,
		`$("#muxRemux").addEventListener("click", remuxTS)`,
		`setRemuxAvailable(store, src.quality || "")`,
		`video/mp4`,
		`await out.writeAt(mdatAt, mdatHeader(written))`, // 回填 mdat 长度
		`streamCopy(src, out, mdatStart, written)`,       // 不支持位置写时的兜底
	} {
		if !strings.Contains(ui, want) {
			t.Errorf("解析页缺少 %q", want)
		}
	}
	if strings.Contains(ui, "<script src=") || strings.Contains(ui, "importScripts(") {
		t.Error("页面加载了外部脚本：转封装必须是自写的（注释里提到 mp4box.js/ffmpeg.wasm 不算）")
	}
	// 标记块里必须完全没有 DOM/网络/模块依赖：既是对"零外部资源"的承诺，
	// 也是 node 端到端测试能跑起来的前提（核心只通过 write 回调落盘）。
	core := remuxCore(t)
	for _, forbidden := range []string{"document.", "window.", "navigator.", "fetch(",
		"XMLHttpRequest", "localStorage", "require(", "import ", "new Worker("} {
		if strings.Contains(core, forbidden) {
			t.Errorf("转封装核心块里出现了 %q：这段必须是纯逻辑", forbidden)
		}
	}
	// 不自动转封装：合并（mergeHLS）里不能直接调 remuxTS
	head := strings.Index(ui, "async function mergeHLS(")
	tail := strings.Index(ui[head:], "\n  // startMerge")
	if head < 0 || tail < 0 {
		t.Fatal("找不到 mergeHLS 的范围")
	}
	if strings.Contains(ui[head:head+tail], "remuxTS(") {
		t.Error("mergeHLS 里不该自动调 remuxTS：那会让浏览器存储与 CPU 开销翻倍")
	}
}
