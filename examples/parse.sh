#!/bin/sh
# vidlink 解析客户端示例（POSIX sh + curl + openssl，零依赖）。
#
# 演示"推荐用法"：每次请求现算一张签名放进 `X-API-Key` 头 ——
# 明文 Key 不进网络，只作为本机环境变量存在；签名几十秒后就作废。
# 签名实现见同目录的 sign.sh（本脚本直接调用它，不复制一份密码学代码）。
#
# 用法：
#
#     export VIDLINK_BASE=https://vl.wzml.cc.cd
#     export VIDLINK_KEY=vl_xxx
#
#     sh examples/parse.sh --url 'https://www.bilibili.com/video/BV1xx411c7mD'
#     sh examples/parse.sh --url '...' --endpoint info
#     sh examples/parse.sh --url '...' --endpoint detail --quality 720
#     sh examples/parse.sh --usage
#     sh examples/parse.sh --url '...' --download out.mp4
#     sh examples/parse.sh --url '...' --json          # 只打印原始 JSON
#
# 说明：下面用 grep/sed 从 JSON 里抠字段，只为"零依赖"。
# 真实项目里请用 jq，或直接用 examples/parse.py / examples/parse.js。
set -eu

BASE=${VIDLINK_BASE:-http://127.0.0.1:8080}
KEY=${VIDLINK_KEY:-}
ENDPOINT=links
QUALITY=
TARGET=
USAGE=0
DOWNLOAD=
RAWJSON=0

while [ $# -gt 0 ]; do
  case "$1" in
    --base) BASE=$2; shift 2 ;;
    --key) KEY=$2; shift 2 ;;
    --endpoint) ENDPOINT=$2; shift 2 ;;
    --quality) QUALITY=$2; shift 2 ;;
    --url) TARGET=$2; shift 2 ;;
    --usage) USAGE=1; shift ;;
    --download) DOWNLOAD=$2; shift 2 ;;
    --json) RAWJSON=1; shift ;;
    -h|--help)
      sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 2 ;;
  esac
done
BASE=${BASE%/}
SIGN_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

if [ -z "$KEY" ]; then
  echo "缺少 Key：用 --key 或环境变量 VIDLINK_KEY（公共 Key 为 vl_public）" >&2
  exit 2
fi
if [ "$USAGE" -eq 0 ] && [ -z "$TARGET" ]; then
  echo "缺少目标：用 --url" >&2
  exit 2
fi

# 现算一张签名（每次请求都算：缓存它只会换来一堆签名过期的 403）
CRED=$(sh "$SIGN_DIR/sign.sh" "$KEY")

# quota_headers/body 落在临时文件：curl 的 -D 让响应头与响应体分开
HDR=$(mktemp); BODY=$(mktemp)
trap 'rm -f "$HDR" "$BODY"' EXIT

# json_str / json_num / json_strs：从 JSON 里抠标量或列表项（只为"零依赖"）。
# 只匹配 "键":（前面带引号），所以 source_url / audio_url 这类同后缀的键
# 不会被误抓；冒号与引号之间的空格也容忍（不同实现序列化风格不同）。
json_strs() {
  grep -o '"'"$1"'": *"[^"]*"' "$2" | sed 's/.*: *"//; s/"$//'
}
json_str() { json_strs "$1" "$2" | head -1; }
json_num() {
  grep -o '"'"$1"'": *[0-9.]*' "$2" | head -1 | sed 's/.*: *//'
}

get() { # get <path-with-query>
  code=$(curl -sS -o "$BODY" -D "$HDR" -w '%{http_code}' \
         -H "X-API-Key: $CRED" -H 'User-Agent: vidlink-example/1' "$BASE$1" || true)
}

show_quota() {
  used=$(grep -i '^X-Quota-Consumed:' "$HDR" | tr -d '\r' | awk '{print $2}')
  left=$(grep -i '^X-Quota-Remaining:' "$HDR" | tr -d '\r' | awk '{print $2}')
  if [ -n "${used:-}${left:-}" ]; then
    printf '（本次消耗 %s，剩余 %s）\n' "${used:-?}" "${left:-?}"
  fi
}

if [ "$USAGE" -eq 1 ]; then
  get /v1/usage
  if [ "$RAWJSON" -eq 1 ] || [ "$code" != 200 ]; then
    cat "$BODY"; echo
  else
    name=$(json_str name "$BODY")
    quota=$(json_num quota "$BODY")
    printf '账号: %s  剩余配额: %s\n' "$name" "$quota"
  fi
  [ "$code" = 200 ] || exit 1
  exit 0
fi

Q="url=$(python3 -c 'import sys,urllib.parse;print(urllib.parse.quote(sys.argv[1],safe=""))' "$TARGET" 2>/dev/null || printf '%s' "$TARGET")"
[ -n "$QUALITY" ] && Q="$Q&quality=$QUALITY"

if [ -n "$DOWNLOAD" ]; then
  # 先取直链，再交给服务端代理下载（代理要把签名拼进 URL：原生命令行下载
  # 与浏览器 <a>/<video> 是同一种形态）。
  get "/v1/links?$Q"
  if [ "$code" != 200 ]; then
    echo "取直链失败 $code:" >&2; cat "$BODY" >&2; echo >&2; exit 1
  fi
  URL=$(json_strs url "$BODY" | head -1)
  if [ -z "$URL" ]; then echo "没有可下载的流" >&2; exit 1; fi
  ENC=$(python3 -c 'import sys,urllib.parse;print(urllib.parse.quote(sys.argv[1],safe=""))' "$URL" 2>/dev/null || printf '%s' "$URL")
  # 代理下载也把响应头留下来：这一行配额是**下载**的账，不是上面取直链的
  curl -sS -L --fail -D "$HDR" -o "$DOWNLOAD" "$BASE/v1/proxy?url=$ENC&key=$CRED"
  show_quota
  printf '已保存 %s（%s 字节）\n' "$DOWNLOAD" "$(wc -c <"$DOWNLOAD" | tr -d ' ')"
  exit 0
fi

get "/v1/$ENDPOINT?$Q"
if [ "$RAWJSON" -eq 1 ] || [ "$code" != 200 ]; then
  cat "$BODY"; echo
  [ "$code" = 200 ] || exit 1
else
  case "$ENDPOINT" in
    info)
      printf '标题: %s\n' "$(json_str title "$BODY")"
      ;;
    *)
      # 直链一行一条（jq 用户可以用 --json 自己处理）。
      # 只匹配 "url"（前面必须是引号），所以 source_url / audio_url /
      # backup_urls 都不会被误抓；: 与 " 之间的空格也容忍。
      json_strs url "$BODY"
      ;;
  esac
  show_quota
fi
