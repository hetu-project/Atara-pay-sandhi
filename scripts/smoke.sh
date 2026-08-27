#!/usr/bin/env bash
# 端到端冒烟：New order 与 Trade 两条主流程各跑一遍到终态。
# 用一个临时库，不碰开发库。
set -euo pipefail

PORT=${PORT:-8099}
B="localhost:$PORT/api/v1"
DB=$(mktemp -u /tmp/atara-smoke-XXXX.db)

go build -o /tmp/atara-smoke ./cmd/atara-pay
ATARA_HTTP_ADDR=":$PORT" ATARA_DB_PATH="$DB" /tmp/atara-smoke > /tmp/atara-smoke.log 2>&1 &
PID=$!
trap 'kill $PID 2>/dev/null || true; rm -f "$DB"*' EXIT
sleep 3

jqp() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)"; }
tok() { curl -s -X POST "$B/passkey/assert" -H 'Content-Type: application/json' \
        -d "$1" | jqp 'd["confirmation"]'; }

echo "── New order ──"
T=$(tok '{"scope":"order","parts":["cp-hc","USDT","3000"]}')
OID=$(curl -s -X POST "$B/orders" -H 'Content-Type: application/json' -H "X-Atara-Confirmation: $T" \
  -d '{"counterparty_id":"cp-hc","asset":"USDT","amount":"3000","card_id":"card-me",
       "conditions":[{"atom_type":"evidence","params":{"proof":"Delivery record"}}]}' | jqp 'd["id"]')
echo "  created $OID"
for _ in $(seq 1 15); do
  S=$(curl -s "$B/orders/$OID" | jqp 'd["state"]')
  [ "$S" = "released" ] && break
  sleep 2
done
echo "  final state: $S"
[ "$S" = "released" ] || { echo "FAIL: conditional order did not settle"; exit 1; }

echo "── Trade ──"
TID=$(curl -s -X POST "$B/offers/p1/take" -H 'Content-Type: application/json' \
  -d '{"amount":"73100","amount_kind":"fiat","network":"TRON"}' | jqp 'd["id"]')
echo "  matched $TID"
T=$(tok "{\"scope\":\"accept\",\"parts\":[\"$TID\"]}")
curl -s -o /dev/null -X POST "$B/orders/$TID/accept" -H "X-Atara-Confirmation: $T"
for _ in $(seq 1 10); do
  [ "$(curl -s "$B/orders/$TID" | jqp 'd["state"]')" = "s3" ] && break
  sleep 2
done
echo "receipt" > /tmp/atara-receipt.txt
REF=$(curl -s -X POST "$B/uploads" -F "file=@/tmp/atara-receipt.txt" | jqp 'd["file_ref"]')
curl -s -o /dev/null -X POST "$B/orders/$TID/receipt" -H 'Content-Type: application/json' \
  -d "{\"file_ref\":\"$REF\"}"
for _ in $(seq 1 10); do
  S=$(curl -s "$B/orders/$TID" | jqp 'd["state"]')
  [ "$S" = "s5" ] && break
  sleep 2
done
echo "  final state: $S"
[ "$S" = "s5" ] || { echo "FAIL: OTC deal did not settle"; exit 1; }

echo "── OK ──"
