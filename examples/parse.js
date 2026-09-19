#!/usr/bin/env node
/**
 * vidlink 解析客户端示例（Node 18+，零依赖；浏览器里把 request() 换成 fetch 即可）。
 *
 * 演示"推荐用法"的完整闭环：
 *
 *   1. 用账号 Key **现算一张签名**（明文 Key 不进网络，只留在你本机）；
 *   2. 把签名放进 `X-API-Key` 请求头调用解析接口；
 *   3. 打印标题/作者/直链，并显示这次消耗了多少配额；
 *   4. 需要下载时走服务端代理 `/v1/proxy`（服务端开启才行）。
 *
 * 签名实现见同目录的 sign.js（本文件直接复用，不重复一份密码学代码）。
 *
 * 用法：
 *
 *     export VIDLINK_BASE=https://vl.wzml.cc.cd
 *     export VIDLINK_KEY=vl_xxx
 *
 *     node examples/parse.js --url 'https://www.bilibili.com/video/BV1xx411c7mD'
 *     node examples/parse.js --url '...' --endpoint info
 *     node examples/parse.js --url '...' --endpoint detail --quality 720
 *     node examples/parse.js --usage
 *     node examples/parse.js --url '...' --download out.mp4
 *     node examples/parse.js --platform bilibili --id BV1xx411c7mD
 *     node examples/parse.js --url '...' --json     # 只打印原始 JSON
 *
 * 浏览器里同样可用（页面内嵌的就是同一套签名实现）：
 *
 *     const cred = VLSign.credential("acc_", KEY);   // 现算签名
 *     const r = await fetch(BASE + "/v1/links?url=" + encodeURIComponent(url),
 *                           { headers: { "X-API-Key": cred } });
 */

"use strict";

const fs = require("fs");
const path = require("path");
const S = require(path.join(__dirname, "sign.js"));

const ENDPOINTS = ["info", "links", "detail"];

function parseArgs(argv) {
  const o = {
    base: process.env.VIDLINK_BASE || "http://127.0.0.1:8080",
    key: process.env.VIDLINK_KEY || "",
    endpoint: "links", quality: "", url: "", platform: "", id: "",
    usage: false, download: "", json: false,
  };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--base") o.base = argv[++i];
    else if (a === "--key") o.key = argv[++i];
    else if (a === "--endpoint") o.endpoint = argv[++i];
    else if (a === "--quality") o.quality = argv[++i];
    else if (a === "--url") o.url = argv[++i];
    else if (a === "--platform") o.platform = argv[++i];
    else if (a === "--id") o.id = argv[++i];
    else if (a === "--download") o.download = argv[++i];
    else if (a === "--usage") o.usage = true;
    else if (a === "--json") o.json = true;
    else if (a === "-h" || a === "--help") { usage(); process.exit(0); }
    else { console.error("未知参数: " + a); usage(); process.exit(2); }
  }
  o.base = o.base.replace(/\/+$/, "");
  return o;
}

function usage() {
  console.log(`用法: node examples/parse.js [--base URL] [--key KEY] [--endpoint info|links|detail]
                            [--url URL | --platform P --id ID] [--quality 720]
                            [--usage] [--download FILE] [--json]`);
}

// request 带签名头发一次 GET，返回 { status, headers, body }。
//
// 每个请求都**现算签名**：签名只有几十秒有效期，缓存它只会换来一堆
// 签名过期的 403，而一次 HMAC 的代价可以忽略（几十微秒）。
async function request(base, pathname, key, query) {
  const qs = query && Object.keys(query).length ? "?" + new URLSearchParams(query).toString() : "";
  const res = await fetch(base + pathname + qs, {
    headers: { "X-API-Key": S.credential("acc_", key), "User-Agent": "vidlink-example/1" },
  });
  const text = await res.text();
  let body;
  try { body = text ? JSON.parse(text) : null; } catch (_) { body = { raw: text }; }
  return { status: res.status, headers: res.headers, body };
}

function quotaLine(headers) {
  const used = headers.get("X-Quota-Consumed");
  const left = headers.get("X-Quota-Remaining");
  if (used === null && left === null) return "";
  return `（本次消耗 ${used ?? "?"}，剩余 ${left ?? "?"}）`;
}

function showInfo(b) {
  console.log("标题:", b.title || "");
  console.log("作者:", (b.author && b.author.name) || "");
  const s = b.stats || {};
  console.log(`数据: 播放 ${s.view ?? "?"} / 点赞 ${s.like ?? "?"} / 时长 ${s.duration ?? "?"} 秒`);
  for (const q of b.qualities || []) console.log("  档位:", q.label, "height=" + q.height, "id=" + q.quality_id);
}

// /v1/links 的形状是**单个 Links 对象**（不是数组）：
// {url, backup_urls, headers, audio_url, needs_mux}，见 docs/API.md §4.2。
function showLinks(b) {
  if (b.url) console.log("视频   ", b.url);
  if (b.audio_url) {
    console.log("音频   ", b.audio_url);
    console.log("# DASH 分离流：需要自行混流（服务端只给两条独立直链）");
  }
  (b.backup_urls || []).forEach((u, i) => console.log("备用 " + (i + 1) + " ", u));
  if (b.headers) console.log("# 必须透传的请求头：", JSON.stringify(b.headers));
  for (const k of ["note", "warning"]) if (b[k]) console.log("#", b[k]);
}

// download 走服务端代理：把签名拼进 URL（<a>/<video> 就是这种形态）。
async function download(base, key, target, out) {
  const q = new URLSearchParams({ url: target, filename: path.basename(out), key: S.credential("acc_", key) });
  const res = await fetch(base + "/v1/proxy?" + q.toString());
  if (!res.ok) {
    console.error(await res.text());
    return 1;
  }
  const ws = fs.createWriteStream(out);
  for await (const chunk of res.body) ws.write(chunk);
  await new Promise((r) => ws.end(r));
  console.log(`已保存 ${out}（${fs.statSync(out).size} 字节）${quotaLine(res.headers)}`);
  return 0;
}

async function main() {
  const o = parseArgs(process.argv.slice(2));
  if (!o.key) { console.error("缺少 Key：用 --key 或环境变量 VIDLINK_KEY（公共 Key 为 vl_public）"); return 2; }
  if (!o.usage && !o.url && !(o.platform && o.id)) {
    console.error("缺少目标：用 --url，或 --platform + --id");
    return 2;
  }
  if (!ENDPOINTS.includes(o.endpoint)) { console.error("--endpoint 只能是 " + ENDPOINTS.join("/")); return 2; }

  if (o.usage) {
    const r = await request(o.base, "/v1/usage", o.key);
    if (o.json) console.log(JSON.stringify(r.body, null, 2));
    else if (r.status === 200) {
      const b = r.body;
      console.log(`账号: ${b.name}  剩余配额: ${b.quota}  累计消耗: ${b.used}  调用: ${b.calls}  倍率: ${b.multiplier}`);
    } else console.error(`失败 ${r.status}: ${JSON.stringify(r.body)}`);
    return r.status === 200 ? 0 : 1;
  }

  const query = o.url ? { url: o.url } : { platform: o.platform, id: o.id };
  if (o.quality) query.quality = o.quality;

  if (o.download) {
    const r = await request(o.base, "/v1/links", o.key, query);
    if (r.status !== 200) {
      console.error(`取直链失败 ${r.status}: ${JSON.stringify(r.body)}`);
      return 1;
    }
    const direct = r.body && r.body.url;
    if (!direct) { console.error("没有可下载的直链"); return 1; }
    return download(o.base, o.key, direct, o.download);
  }

  const r = await request(o.base, "/v1/" + o.endpoint, o.key, query);
  if (o.json || r.status !== 200) console.log(JSON.stringify(r.body, null, 2));
  else if (o.endpoint === "info") showInfo(r.body);
  else if (o.endpoint === "detail") { showInfo(r.body); console.log("直链:"); showLinks(r.body); }
  else showLinks(r.body);
  if (r.status === 200) {
    const q = quotaLine(r.headers);
    if (q) console.log(q);
  }
  return r.status === 200 ? 0 : 1;
}

main().then((code) => process.exit(code)).catch((e) => { console.error(e); process.exit(1); });
