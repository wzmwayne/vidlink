#!/usr/bin/env node
/**
 * 生成 vidlink 签名凭据（Node / 浏览器都能用，零依赖）。
 *
 * 为什么要有签名：明文 Key 一旦进了 URL，就会留在浏览器历史、下载记录、
 * 隧道与反向代理日志、聊天记录与截屏里，而它是**长期有效**的；签名只有
 * 几十秒，且签名本身不含任何秘密。vidlink 的两个内嵌页面用的就是本文件
 * 里这套纯 JS 实现（浏览器里没有 crypto.subtle 可用性保证：它只在安全
 * 上下文存在，而局域网 http://192.168.x.x 不是）。
 *
 * 凭据形态（定长，三段）：
 *
 *     acc_<16位小写hex句柄>.<8位小写hex时间戳>.<64位小写hex签名>
 *     └──────── 20 ───────┘ └────── 8 ──────┘ └────── 64 ──────┘
 *
 *    句柄   = "acc_" + hex(SHA-256(Key))[:16]
 *    时间戳 = unix 秒，写成 8 位小写十六进制
 *    签名   = hex(HMAC-SHA256(Key, "句柄|时间戳hex"))   完整 64 位
 *
 * 用法：
 *
 *     node examples/sign.js vl_xxx                  # 用当前时间
 *     node examples/sign.js vl_xxx 1755571800       # 指定时间戳
 *     node examples/sign.js vl_xxx --prefix adm_    # 管理签名（管理 Key）
 *     node examples/sign.js vl_xxx --url /v1/usage  # 直接给可用的 URL
 *
 * 浏览器里直接复制本文件的函数即可（页面内嵌的就是同一套实现）。
 */

"use strict";

const VL_K256 = new Uint32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]);

const vlRo = (x, n) => ((x >>> n) | (x << (32 - n))) >>> 0;
const vlUtf8 = (s) => new TextEncoder().encode(s);
const vlHex = (bytes) => {
  let out = "";
  for (let i = 0; i < bytes.length; i++) out += bytes[i].toString(16).padStart(2, "0");
  return out;
};

// vlSha256 计算 SHA-256，返回 32 字节。
function vlSha256(bytes) {
  const len = bytes.length;
  const total = (((len + 9) + 63) >> 6) << 6;   // 0x80 + 0 填充 + 8 字节长度
  const buf = new Uint8Array(total);
  buf.set(bytes);
  buf[len] = 0x80;
  const view = new DataView(buf.buffer);
  const bits = len * 8;
  view.setUint32(total - 8, Math.floor(bits / 4294967296));
  view.setUint32(total - 4, bits >>> 0);

  const h = new Uint32Array([
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
    0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
  ]);
  const w = new Uint32Array(64);
  for (let off = 0; off < total; off += 64) {
    for (let i = 0; i < 16; i++) w[i] = view.getUint32(off + i * 4);
    for (let i = 16; i < 64; i++) {
      const x = w[i - 15], y = w[i - 2];
      const s0 = vlRo(x, 7) ^ vlRo(x, 18) ^ (x >>> 3);
      const s1 = vlRo(y, 17) ^ vlRo(y, 19) ^ (y >>> 10);
      w[i] = (w[i - 16] + s0 + w[i - 7] + s1) >>> 0;
    }
    let a = h[0], b = h[1], c = h[2], d = h[3];
    let e = h[4], f = h[5], g = h[6], hh = h[7];
    for (let i = 0; i < 64; i++) {
      const S1 = vlRo(e, 6) ^ vlRo(e, 11) ^ vlRo(e, 25);
      const ch = (e & f) ^ (~e & g);
      const t1 = (hh + S1 + ch + VL_K256[i] + w[i]) >>> 0;
      const S0 = vlRo(a, 2) ^ vlRo(a, 13) ^ vlRo(a, 22);
      const maj = (a & b) ^ (a & c) ^ (b & c);
      const t2 = (S0 + maj) >>> 0;
      hh = g; g = f; f = e; e = (d + t1) >>> 0;
      d = c; c = b; b = a; a = (t1 + t2) >>> 0;
    }
    h[0] = (h[0] + a) >>> 0; h[1] = (h[1] + b) >>> 0;
    h[2] = (h[2] + c) >>> 0; h[3] = (h[3] + d) >>> 0;
    h[4] = (h[4] + e) >>> 0; h[5] = (h[5] + f) >>> 0;
    h[6] = (h[6] + g) >>> 0; h[7] = (h[7] + hh) >>> 0;
  }
  const out = new Uint8Array(32);
  const ov = new DataView(out.buffer);
  for (let i = 0; i < 8; i++) ov.setUint32(i * 4, h[i]);
  return out;
}

const vlSha256Hex = (s) => vlHex(vlSha256(vlUtf8(s)));

// vlHmacSha256Hex 计算 HMAC-SHA256，返回完整的 64 位小写十六进制。
function vlHmacSha256Hex(key, msg) {
  let k = vlUtf8(key);
  if (k.length > 64) k = vlSha256(k);           // 超长 Key 先哈希（RFC 2104）
  const block = new Uint8Array(64);
  block.set(k);
  const m = vlUtf8(msg);
  const inner = new Uint8Array(64 + m.length);
  const outer = new Uint8Array(64 + 32);
  for (let i = 0; i < 64; i++) {
    inner[i] = block[i] ^ 0x36;                 // ipad
    outer[i] = block[i] ^ 0x5c;                 // opad
  }
  inner.set(m, 64);
  outer.set(vlSha256(inner), 64);
  return vlHex(vlSha256(outer));
}

// vlHandle 由 Key 派生公开句柄（句柄是公开信息，反推不出 Key）。
const vlHandle = (prefix, key) => prefix + vlSha256Hex(key).slice(0, 16);

// vlFormatTs：unix 秒 → 8 位小写十六进制。
const vlFormatTs = (ts) => ts.toString(16).padStart(8, "0");

// vlCredentialAt 生成一条完整凭据。
// 消息里的时间戳用**票面那 8 位十六进制原样**（不是十进制秒数）；
// 密钥是 Key 的原始 UTF-8 字节（不要把 vl_ 后面那串 hex 解码）。
function vlCredentialAt(prefix, key, ts) {
  // ts 省略时取当前秒：调用方不必自己算时间。
  const t = ts === undefined ? Math.floor(Date.now() / 1000) : ts;
  const h = vlHandle(prefix, key);
  const th = vlFormatTs(t);
  return h + "." + th + "." + vlHmacSha256Hex(key, h + "|" + th);
}

// vlSignUrl 把凭据以 ?key= 拼到 URL 上（服务端允许 ±30 秒时间偏差）。
function vlSignUrl(prefix, key, url, ts) {
  const t = ts === undefined ? Math.floor(Date.now() / 1000) : ts;
  const cred = vlCredentialAt(prefix, key, t);
  return url + (url.includes("?") ? "&" : "?") + "key=" + cred;
}

// 作为命令行工具运行：node examples/sign.js <key> [ts] [--prefix adm_] [--url /v1/usage]
if (typeof require !== "undefined" && require.main === module) {
  const argv = process.argv.slice(2);
  if (argv.length === 0 || argv[0] === "-h" || argv[0] === "--help") {
    console.log("用法: node examples/sign.js <key> [ts] [--prefix adm_] [--url /v1/usage]");
    process.exit(0);
  }
  const key = argv[0];
  let ts, prefix = "acc_", url = null;
  for (let i = 1; i < argv.length; i++) {
    if (argv[i] === "--prefix") prefix = argv[++i];
    else if (argv[i] === "--url") url = argv[++i];
    else ts = Number(argv[i]);
  }
  console.log(url !== null ? vlSignUrl(prefix, key, url, ts) : vlCredentialAt(prefix, key, ts === undefined ? Math.floor(Date.now() / 1000) : ts));
}

// 浏览器里没有 module：这里显式挂到全局，方便复制到控制台或脚本里用；
// Node 里同时导出为 CommonJS 模块，供 examples/parse.js 直接复用。
if (typeof globalThis !== "undefined") {
  globalThis.VLSign = { credential: vlCredentialAt, signUrl: vlSignUrl, hmacHex: vlHmacSha256Hex, sha256Hex: vlSha256Hex };
}
if (typeof module !== "undefined" && module.exports) {
  module.exports = {
    credential: vlCredentialAt,
    signUrl: vlSignUrl,
    hmacHex: vlHmacSha256Hex,
    sha256Hex: vlSha256Hex,
    handle: vlHandle,
  };
}
