#!/usr/bin/env bash
#
# 把托管合约部到一条 EVM 链上，并打印后端要用的那几行配置。
#
# 参数全部来自环境变量，从 .env 读（.env 已经在 .gitignore 里）：
#
#   ATARA_SIGNER_KEY   部署者私钥，同时是 Demo 里唯一的放行签名方
#   ATARA_RPC_URL      节点
#   ATARA_TOKEN_USDT   已有的 USDT 合约。留空则部一个测试币
#   ATARA_TOKEN_USDC   同上
#   ATARA_NETWORK      写进配置的网络名，只用于展示
#   ATARA_EXPLORER     浏览器前缀，用于拼交易链接
#
# 私钥绝不进仓库，也绝不出现在命令行上——命令行会进 shell 历史，
# 也会被同机器上别的进程从 /proc 看到。所以这里只从环境变量取。
set -euo pipefail

cd "$(dirname "$0")/.."
[ -f .env ] && set -a && . ./.env && set +a

die() { printf '\033[31m×\033[0m %s\n' "$*" >&2; exit 1; }
ok()  { printf '\033[32m✓\033[0m %s\n' "$*"; }

command -v forge >/dev/null 2>&1 || die "没装 foundry。装：curl -L https://foundry.paradigm.xyz | bash && foundryup"
command -v cast  >/dev/null 2>&1 || die "没装 cast（跟 foundry 一起装）"

: "${ATARA_SIGNER_KEY:?在 .env 里设 ATARA_SIGNER_KEY（部署者私钥，不带 0x 也行）}"
: "${ATARA_RPC_URL:?在 .env 里设 ATARA_RPC_URL}"

KEY="${ATARA_SIGNER_KEY#0x}"
[ ${#KEY} -eq 64 ] || die "私钥应该是 64 个十六进制字符，现在是 ${#KEY} 个"

# 节点通不通、是哪条链——先问清楚。RPC 挂掉时 forge 的报错很难看出是网络问题。
CHAIN_ID=$(cast chain-id --rpc-url "$ATARA_RPC_URL" 2>/dev/null) || die "连不上 $ATARA_RPC_URL"
FROM=$(cast wallet address --private-key "0x$KEY")
BAL=$(cast balance "$FROM" --rpc-url "$ATARA_RPC_URL")
ok "节点 $ATARA_RPC_URL · chainId $CHAIN_ID"
ok "部署者 $FROM · 余额 $(cast from-wei "$BAL") 原生币"
[ "$BAL" != "0" ] || die "余额是 0，付不起 gas。先去水龙头领测试币"

# 现成的币地址先验一眼。填错地址部署照样成功，等到挂单锁币那一刻才炸，
# 那时错误信息只会说「call reverted」，查起来毫无线索。
for pair in "USDT:${ATARA_TOKEN_USDT:-}" "USDC:${ATARA_TOKEN_USDC:-}"; do
  sym=${pair%%:*}; addr=${pair#*:}
  [ -n "$addr" ] || { echo "  $sym 没给地址 → 这次部一个测试币"; continue; }
  code=$(cast code "$addr" --rpc-url "$ATARA_RPC_URL" 2>/dev/null || echo 0x)
  [ "$code" != "0x" ] || die "$sym 的地址 $addr 上没有合约（这条链上）"
  dec=$(cast call "$addr" "decimals()(uint8)" --rpc-url "$ATARA_RPC_URL" 2>/dev/null) \
    || die "$sym 的地址 $addr 不像个 ERC-20（读不到 decimals）"
  ok "$sym 用现成的 $addr · $dec 位精度"
done

echo
echo "开始部署 AtaraEscrow + AtaraSpending …"
cd contracts
PRIVATE_KEY="0x$KEY" \
TOKEN_USDT="${ATARA_TOKEN_USDT:-0x0000000000000000000000000000000000000000}" \
TOKEN_USDC="${ATARA_TOKEN_USDC:-0x0000000000000000000000000000000000000000}" \
MIN_SCORE="${MIN_SCORE:-70}" \
  forge script script/Deploy.s.sol \
    --rpc-url "$ATARA_RPC_URL" \
    --broadcast \
    ${DEPLOY_EXTRA_ARGS:-} \
  2>&1 | tee /tmp/atara-deploy.log

echo
echo "把下面这几行写进 .env（覆盖同名的）："
echo "────────────────────────────────────────────"
grep -E '^\s+ATARA_[A-Z_]+=' /tmp/atara-deploy.log | sed 's/^ *//' | sort -u
echo "ATARA_CHAIN_IMPL=evm"
echo "ATARA_RPC_URL=$ATARA_RPC_URL"
echo "ATARA_NETWORK=${ATARA_NETWORK:-BSC-TESTNET}"
echo "ATARA_EXPLORER=${ATARA_EXPLORER:-https://testnet.bscscan.com}"
echo "────────────────────────────────────────────"
echo "然后 make fresh 重起后端。合约地址会从 GET /api/v1/catalog/chain 发给前端。"
