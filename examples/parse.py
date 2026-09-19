#!/usr/bin/env python3
"""vidlink 解析客户端示例（Python，只用标准库）。

它演示"推荐用法"的完整闭环：

  1. 用账号 Key **现算一张签名**（明文 Key 不进网络，只留在你本机）；
  2. 把签名放进 `X-API-Key` 请求头调用解析接口；
  3. 打印标题/作者/直链，并显示这次消耗了多少配额；
  4. 需要下载时走服务端代理 `/v1/proxy`（服务端开启才行）。

签名实现见同目录的 sign.py（本文件直接复用，不重复一份密码学代码）。

用法：

    export VIDLINK_BASE=https://vl.wzml.cc.cd
    export VIDLINK_KEY=vl_xxx

    python3 examples/parse.py --url 'https://www.bilibili.com/video/BV1xx411c7mD'
    python3 examples/parse.py --url '...' --endpoint info
    python3 examples/parse.py --url '...' --endpoint detail --quality 720
    python3 examples/parse.py --usage
    python3 examples/parse.py --url '...' --download out.mp4
    python3 examples/parse.py --platform bilibili --id BV1xx411c7mD
    python3 examples/parse.py --url '...' --json      # 只打印原始 JSON
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from sign import credential  # noqa: E402  （同目录的签名实现）

ENDPOINTS = ("info", "links", "detail")


def request(base: str, path: str, key: str, query: dict | None = None) -> tuple[int, dict, dict]:
    """带签名头发一次 GET，返回 (状态码, 响应头, JSON)。

    每个请求都**现算签名**：签名只有几十秒有效期，缓存它只会换来
    一堆签名过期的 403，而一次 HMAC 的代价可以忽略（微秒级）。
    """
    qs = ("?" + urllib.parse.urlencode(query)) if query else ""
    req = urllib.request.Request(base + path + qs, method="GET")
    req.add_header("X-API-Key", credential(key))
    req.add_header("User-Agent", "vidlink-example/1")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.status, dict(resp.headers), json.load(resp)
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", "replace")
        try:
            body = json.loads(raw)
        except ValueError:
            body = {"raw": raw}
        return e.code, dict(e.headers), body


def quota_line(headers: dict) -> str:
    used = headers.get("X-Quota-Consumed")
    left = headers.get("X-Quota-Remaining")
    if used is None and left is None:
        return ""
    return "（本次消耗 %s，剩余 %s）" % (used or "?", left or "?")


def show_info(body: dict) -> None:
    print("标题:", body.get("title", ""))
    author = body.get("author") or {}
    print("作者:", author.get("name", ""))
    stats = body.get("stats") or {}
    print("数据:", "播放 %s / 点赞 %s / 时长 %s 秒"
          % (stats.get("view", "?"), stats.get("like", "?"), stats.get("duration", "?")))
    for q in body.get("qualities") or []:
        print("  档位:", q.get("label"), "height=%s" % q.get("height"), "id=%s" % q.get("quality_id"))


def show_links(body: dict) -> None:
    """/v1/links 的形状是**单个 Links 对象**（不是数组）：
    {url, backup_urls, headers, audio_url, needs_mux}，见 docs/API.md §4.2。"""
    if body.get("url"):
        print("视频   ", body["url"])
    if body.get("audio_url"):
        print("音频   ", body["audio_url"])
        print("# DASH 分离流：需要自行混流（服务端只给两条独立直链）")
    for i, u in enumerate(body.get("backup_urls") or [], 1):
        print("备用 %d " % i, u)
    if body.get("headers"):
        print("# 必须透传的请求头：", json.dumps(body["headers"], ensure_ascii=False))
    for k in ("note", "warning"):
        if body.get(k):
            print("#", body[k])


def show_detail(body: dict) -> None:
    show_info(body)
    print("直链:")
    show_links(body)


def download(base: str, key: str, target_url: str, out: str) -> int:
    """走服务端代理下载（浏览器里同样是这条路径，只是凭据放在 URL 上）。"""
    # 代理是"原生下载"的形态：把签名拼进 URL（<a>/<video> 无法自定义请求头）。
    q = urllib.parse.urlencode({"url": target_url, "filename": os.path.basename(out),
                                "key": credential(key)})
    req = urllib.request.Request(base + "/v1/proxy?" + q, method="GET")
    with urllib.request.urlopen(req, timeout=300) as resp, open(out, "wb") as f:
        headers = dict(resp.headers)   # 响应头要在 with 里取出来（退出时连接就关了）
        total = int(headers.get("Content-Length") or 0)
        got = 0
        while True:
            chunk = resp.read(1 << 16)
            if not chunk:
                break
            f.write(chunk)
            got += len(chunk)
            if total:
                pct = got * 100 // total
                print("\r下载中 %d%% (%d/%d 字节)" % (pct, got, total), end="", file=sys.stderr)
    print("\r已保存 %s（%d 字节）%s" % (out, os.path.getsize(out), quota_line(headers)))
    return 0


def main(argv: list[str]) -> int:
    p = argparse.ArgumentParser(description="vidlink 解析客户端示例（Python）")
    p.add_argument("--base", default=os.environ.get("VIDLINK_BASE", "http://127.0.0.1:8080"),
                   help="服务端地址，默认取 VIDLINK_BASE")
    p.add_argument("--key", default=os.environ.get("VIDLINK_KEY", ""),
                   help="账号 Key，默认取 VIDLINK_KEY（公共 Key 为 vl_public）")
    p.add_argument("--endpoint", default="links", choices=ENDPOINTS)
    p.add_argument("--url", help="视频链接或整段分享文案")
    p.add_argument("--platform", help="已知平台时用它 + --id 省一次跳转")
    p.add_argument("--id", dest="vid", help="内容 ID")
    p.add_argument("--quality", help="仅 links/detail：1080 / 720P / qn:80")
    p.add_argument("--usage", action="store_true", help="只查自己的配额与用量")
    p.add_argument("--download", metavar="FILE", help="经服务端代理下载到文件")
    p.add_argument("--json", action="store_true", help="只打印原始 JSON")
    p.add_argument("--timeout", type=float, default=30)
    args = p.parse_args(argv)
    base = args.base.rstrip("/")

    if not args.key:
        print("缺少 Key：用 --key 或环境变量 VIDLINK_KEY（公共 Key 为 vl_public）", file=sys.stderr)
        return 2
    if not args.usage and not args.url and not (args.platform and args.vid):
        print("缺少目标：用 --url，或 --platform + --id", file=sys.stderr)
        return 2

    if args.usage:
        status, hdrs, body = request(base, "/v1/usage", args.key)
        if args.json:
            print(json.dumps(body, ensure_ascii=False, indent=2))
        elif status == 200:
            print("账号: %s  剩余配额: %s  累计消耗: %s  调用: %s  倍率: %s"
                  % (body.get("name"), body.get("quota"), body.get("used"),
                     body.get("calls"), body.get("multiplier")))
        else:
            print("失败 %d: %s" % (status, body), file=sys.stderr)
        return 0 if status == 200 else 1

    query: dict = {}
    if args.url:
        query["url"] = args.url
    else:
        query["platform"] = args.platform
        query["id"] = args.vid
    if args.quality:
        query["quality"] = args.quality

    if args.download:
        # 先拿到直链，再交给代理下载（推荐档位由服务端选）
        status, _, body = request(base, "/v1/links", args.key, query)
        if status != 200:
            print("取直链失败 %d: %s" % (status, json.dumps(body, ensure_ascii=False)), file=sys.stderr)
            return 1
        direct = body.get("url") or ""
        if not direct:
            print("没有可下载的直链", file=sys.stderr)
            return 1
        return download(base, args.key, direct, args.download)

    status, hdrs, body = request(base, "/v1/" + args.endpoint, args.key, query)
    if args.json or status != 200:
        print(json.dumps(body, ensure_ascii=False, indent=2))
    elif args.endpoint == "info":
        show_info(body)
    elif args.endpoint == "detail":
        show_detail(body)
    else:
        show_links(body)
    if status == 200:
        q = quota_line(hdrs)
        if q:
            print(q)
    return 0 if status == 200 else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
