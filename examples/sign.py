#!/usr/bin/env python3
"""生成 vidlink 签名凭据（只用标准库，零依赖）。

为什么要有签名：明文 Key 一旦进了 URL，就会留在浏览器历史、下载记录、
隧道与反向代理日志、聊天记录与截屏里，而它是**长期有效**的；签名只有
几十秒，且签名本身不含任何秘密。

凭据形态（定长，三段）：

    acc_<16位小写hex句柄>.<8位小写hex时间戳>.<64位小写hex签名>
    └──────── 20 ───────┘ └────── 8 ──────┘ └────── 64 ──────┘

其中：

    句柄   = "acc_" + hex(SHA-256(Key))[:16]      （16 位十六进制）
    时间戳 = unix 秒，写成 8 位小写十六进制
    签名   = hex(HMAC-SHA256(Key, "句柄|时间戳hex"))   完整 64 位，不截断

用法：

    python3 examples/sign.py vl_xxx                    # 用当前时间
    python3 examples/sign.py vl_xxx 1755571800         # 指定时间戳
    python3 examples/sign.py vl_xxx --prefix adm_      # 管理签名（管理 Key）
    python3 examples/sign.py vl_xxx --url /v1/usage    # 直接给可用的 URL

    # 也可以当模块用
    from sign import credential
    cred = credential("vl_xxx")
"""

from __future__ import annotations

import hashlib
import hmac
import sys
import time

TS_HEX_LEN = 8
SIG_HEX_LEN = 64


def handle(key: str, prefix: str = "acc_") -> str:
    """由 Key 派生公开句柄：prefix + SHA-256(key) 的前 8 字节（16 位十六进制）。"""
    return prefix + hashlib.sha256(key.encode("utf-8")).hexdigest()[:16]


def mac(key: str, h: str, ts: int) -> str:
    """签名 = hex(HMAC-SHA256(Key, "句柄|时间戳hex"))。

    消息里的时间戳用**票面那 8 位十六进制原样**（不是十进制秒数）：
    双方零转换，少一类"格式不对但对不上"的排查。
    密钥是 Key 字符串的原始 UTF-8 字节——不要把 vl_ 后面那串 hex 解码。
    """
    msg = "%s|%s" % (h, format_ts(ts))
    return hmac.new(key.encode("utf-8"), msg.encode("utf-8"), hashlib.sha256).hexdigest()


def format_ts(ts: int) -> str:
    """unix 秒 → 8 位小写十六进制（零填充，够用到 2106 年）。"""
    return "%08x" % ts


def credential(key: str, ts: int | None = None, prefix: str = "acc_") -> str:
    """生成一条完整凭据（ts 省略则取当前秒）。"""
    if ts is None:
        ts = int(time.time())
    h = handle(key, prefix)
    return "%s.%s.%s" % (h, format_ts(ts), mac(key, h, ts))


def signed_url(url: str, key: str, ts: int | None = None, prefix: str = "acc_") -> str:
    """把凭据以 ?key= 拼到 URL 上（服务端允许 ±30 秒时间偏差）。"""
    cred = credential(key, ts, prefix)
    return url + ("&" if "?" in url else "?") + "key=" + cred


def _main(argv: list[str]) -> int:
    if len(argv) < 2 or argv[1] in ("-h", "--help"):
        print(__doc__.strip())
        return 0
    key = argv[1]
    ts: int | None = None
    prefix = "acc_"
    url: str | None = None
    rest = argv[2:]
    i = 0
    while i < len(rest):
        a = rest[i]
        if a == "--prefix" and i + 1 < len(rest):
            prefix = rest[i + 1]
            i += 2
        elif a == "--url" and i + 1 < len(rest):
            url = rest[i + 1]
            i += 2
        else:
            ts = int(a)
            i += 1
    if url is not None:
        print(signed_url(url, key, ts, prefix))
    else:
        print(credential(key, ts, prefix))
    return 0


if __name__ == "__main__":
    sys.exit(_main(sys.argv))
