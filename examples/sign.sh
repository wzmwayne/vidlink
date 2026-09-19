#!/bin/sh
# 生成 vidlink 签名凭据（只用 openssl，零依赖；适合 CI 与一次性脚本）。
#
# 用法：
#   sh examples/sign.sh vl_xxx                 # 用当前时间
#   sh examples/sign.sh vl_xxx 1755571800      # 指定时间戳
#   sh examples/sign.sh vl_xxx "" adm_         # 管理签名（第二个参数留空取当前时间）
#   sh examples/sign.sh vl_xxx "" acc_ /v1/usage   # 直接给可用的 URL
#
# 凭据形态：acc_<16位hex句柄>.<8位hex时间戳>.<64位hex签名>
#   句柄 = acc_ + hex(SHA-256(Key))[:16]
#   签名 = hex(HMAC-SHA256(Key, "句柄|时间戳hex"))   完整 64 位
#
# 注意：签名原料里的时间戳是**票面那 8 位十六进制原样**，不是十进制秒数；
# HMAC 的密钥是 Key 字符串本身（不要把 vl_ 后面那串 hex 解码成二进制）。
set -eu

KEY=${1:?用法: sh examples/sign.sh <key> [ts] [prefix] [url]}
TS=${2:-$(date +%s)}
PREFIX=${3:-acc_}
URL=${4:-}

[ -n "$TS" ] || TS=$(date +%s)
HANDLE="${PREFIX}$(printf '%s' "$KEY" | openssl dgst -sha256 -r | cut -c1-16)"
TSHEX=$(printf '%08x' "$TS")
SIG=$(printf '%s' "$HANDLE|$TSHEX" | openssl dgst -sha256 -hmac "$KEY" -r | cut -d' ' -f1)
CRED="$HANDLE.$TSHEX.$SIG"

if [ -n "$URL" ]; then
  case "$URL" in
    *\?*) printf '%s&key=%s\n' "$URL" "$CRED" ;;
    *)    printf '%s?key=%s\n' "$URL" "$CRED" ;;
  esac
else
  printf '%s\n' "$CRED"
fi
