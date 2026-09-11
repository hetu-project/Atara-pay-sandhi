#!/usr/bin/env bash
#
# 一条命令起整套：后端 + 前端。Ctrl-C 一起停。
#
# 由 Makefile 的 serve 目标调用，参数全从环境变量来（见那边的注释）。
# 写成脚本而不是塞进 Makefile：这里要装信号处理和等待健康检查，
# 而 make 的每一行都是独立的 shell，trap 活不过一行。
set -u

fail() { printf '\033[31m%s\033[0m\n' "$*" >&2; exit 1; }
say()  { printf '\033[2m%s\033[0m\n' "$*"; }

: "${DB:?}" "${UPLOADS:?}" "${ADDR:?}" "${WEB_DIR:?}" "${WEB_HOST:?}" "${WEB_PORT:?}"
: "${CHAIN:=real}" "${FRESH:=}"

[ -d "$WEB_DIR" ] || fail "找不到前端目录 $WEB_DIR —— 用 WEB_DIR=... 指过去"

PORT=${ADDR##*:}
HOST=${ADDR%:*}

# ── 清场 ──
#
# 先停再删。顺序反过来是没用的：SQLite 那个进程还开着文件句柄，删掉的只是
# 目录项，它照样读写那个 inode，重启前你看到的还是老数据。
for p in $(lsof -ti "tcp:$PORT" -sTCP:LISTEN 2>/dev/null); do
  pp=$(ps -o ppid= -p "$p" 2>/dev/null | tr -d ' ')
  kill "$p" 2>/dev/null
  case "$(ps -o command= -p "$pp" 2>/dev/null)" in *"go run"*) kill "$pp" 2>/dev/null;; esac
  say "   停掉 $PORT 上的旧进程 $p"
done
for p in $(lsof -ti "tcp:$WEB_PORT" -sTCP:LISTEN 2>/dev/null); do
  kill "$p" 2>/dev/null; say "   停掉 $WEB_PORT 上的旧进程 $p"
done
sleep 1

if [ -n "$FRESH" ]; then
  rm -f "$DB" "$DB-wal" "$DB-shm"
  rm -rf "$UPLOADS"
  say "   删掉 $DB 与 $UPLOADS —— 这一次从空库起"
else
  say "   保留 $DB 里的历史数据（要清空加 FRESH=1）"
fi

# ── 真链参数 ──
#
# .env 只在 CHAIN=real 时读。mock 下读了就会去连真链，RPC 一不通后端直接
# 起不来——而人可能只是想开个演示。
if [ "$CHAIN" = real ]; then
  [ -f .env ] || fail ".env 不在——真链参数都在里面。要跑演示用 CHAIN=mock"
  set -a; . ./.env; set +a
  : "${ATARA_CHAIN_IMPL:=evm}"; export ATARA_CHAIN_IMPL
else
  # 显式清掉，免得外层 shell 里残留的真链参数被带进来
  unset ATARA_CHAIN_IMPL ATARA_RPC_URL ATARA_ESCROW_ADDR ATARA_SIGNER_KEY
fi

export ATARA_DB_PATH="$DB" ATARA_UPLOAD_DIR="$UPLOADS" ATARA_HTTP_ADDR="$ADDR"
# 前端走 Vite 的 /api 代理，浏览器看到的是同源，所以后端不需要放开 CORS。
export ATARA_CORS_ORIGINS="${ATARA_CORS_ORIGINS:-http://localhost:$WEB_PORT}"

# 打开作业控制：这样每个后台任务各自成一个进程组，下面才能整组杀掉。
#
# 不整组杀是不行的：`go run` 和 `npm run dev` 都只是壳，真正监听端口的是
# 它们的子进程。kill 掉壳，子进程被 init 收养，继续占着端口活下去——
# Ctrl-C 之后界面还开着，下次再起就是「端口被占用」。
set -m

back= ; front=
# killtree 杀整个进程组；组不在了就退回去杀单个 pid。
killtree() {
  [ -n "${1:-}" ] || return 0
  kill -- "-$1" 2>/dev/null || kill "$1" 2>/dev/null
}
stop() {
  trap - INT TERM EXIT
  killtree "$front"
  killtree "$back"
  wait 2>/dev/null
  # 兜底：万一有谁逃出了进程组，按端口收拾干净。留着不管的话，
  # 下一次 make serve 会撞上「端口被占用」，而原因在上一次退出时。
  for p in $(lsof -ti "tcp:$PORT" -sTCP:LISTEN 2>/dev/null) \
           $(lsof -ti "tcp:$WEB_PORT" -sTCP:LISTEN 2>/dev/null); do
    kill "$p" 2>/dev/null
  done
  say "   都停了"
  # Ctrl-C 是正常的收工方式，不是失败——照 130 退出的话 make 会印一行
  # 红色的 Error 130，看着像出了什么事。
  exit 0
}
trap stop INT TERM EXIT

go run ./cmd/atara-pay & back=$!

# 等后端真的能应答再起前端。不等的话前端先亮起来，第一批请求全是
# connection refused，而界面只会显示一片空——看着像数据没了。
for _ in $(seq 1 60); do
  curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break
  kill -0 "$back" 2>/dev/null || fail "后端没起来，往上翻看它自己报了什么"
  sleep 0.5
done
curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 || fail "后端 30 秒内没有应答"

# 后端只听 127.0.0.1，对外的那一面是 Vite：它把 /api 转进来。这样从别的
# 机器能打开界面，而这个没有任何鉴权的接口不直接暴露在网络上。
( cd "$WEB_DIR" && ATARA_API="http://127.0.0.1:$PORT" \
    npm run dev -- --host "$WEB_HOST" --port "$WEB_PORT" --strictPort ) & front=$!

echo
say "   后端 http://$HOST:$PORT   链=$([ "$CHAIN" = real ] && echo "${ATARA_NETWORK:-真链}" || echo mock)   库=$DB"
case "$WEB_HOST" in
  127.0.0.1|localhost)
    say "   前端 http://localhost:$WEB_PORT" ;;
  *)
    ip=$(ipconfig getifaddr en0 2>/dev/null || hostname -I 2>/dev/null | awk '{print $1}')
    say "   前端 http://${ip:-$WEB_HOST}:$WEB_PORT  （同网段的机器都能打开）"
    printf '\033[33m%s\033[0m\n' \
      "   注意：这套演示没有鉴权——X-Atara-User 头写谁就是谁，上传也不设限。" \
      "   只在你信得过的网段上这么开，别放到公网。" ;;
esac
echo

wait
