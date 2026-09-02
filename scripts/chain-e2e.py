import json,subprocess,sys,time,urllib.request,urllib.error
"""真链端到端。

跑之前：anvil 起着、合约部好、后端以 ATARA_CHAIN_IMPL=evm 起着。
地址从环境变量读（.env 里就有），不写死。

    set -a; . ./.env; set +a
    python3 scripts/chain-e2e.py

每一步都拿 cast 去链上对账——只看后端返回的 JSON 不够，
那只能证明后端自己认为发生了什么。
"""
import os
A = os.environ.get('ATARA_API', 'http://localhost:8080/api/v1')
RPC = os.environ.get('ATARA_RPC_URL', 'http://127.0.0.1:8545')
ESC = os.environ['ATARA_ESCROW_ADDR']
USDT = os.environ['ATARA_TOKEN_USDT']
CAST = os.environ.get('CAST', os.path.expanduser('~/.foundry/bin/cast'))
FAIL=[]
def call(p,b=None,who='Demo',conf=None):
    d=json.dumps(b).encode() if b is not None else None
    r=urllib.request.Request(A+p,data=d,method='POST' if d is not None else 'GET')
    r.add_header('Content-Type','application/json'); r.add_header('X-Atara-User',who)
    if conf: r.add_header('X-Atara-Confirmation',conf)
    try: return json.load(urllib.request.urlopen(r))
    except urllib.error.HTTPError as e: return json.load(e)
def bal(addr):
    o=subprocess.run([CAST,'call',USDT,'balanceOf(address)(uint256)',addr,'--rpc-url',RPC],
                     capture_output=True,text=True).stdout.strip().split()[0]
    return int(o)/1e18
def chk(l,g,w):
    ok=g==w
    if not ok: FAIL.append(f'{l}: {g!r} != {w!r}')
    print(f"  [{'ok ' if ok else 'FAIL'}] {l}: {g}")

print(f"托管合约起始余额: {bal(ESC):,.2f} USDT")
me=call('/me'); print(f"我的地址: {me['address']}")

print("\n-- 1 池子（挂单的可成交量来自链上锁仓）--")
offers=[o for o in call('/offers?side=buy')['offers'] if o['asset']=='USDT' and o['fiat']=='CNY']
chk('有 USDT/CNY 卖单', len(offers)>0, True)
o=offers[0]; peer=o['maker']['name']
print(f"       {peer} · 单价 {o['unit_price']} · 余量 {o['remaining_qty']} USDT")

print("\n-- 2 吃单 --")
d=call(f"/offers/{o['id']}/take",{'amount':o['min_lot'],'amount_kind':'fiat','network':o['network']})
oid=d['id']; chk('state', d['state'], 'match')
coin=d['amount']['amount']
print(f"       工单 {d['ref']} · {coin} USDT")

print("\n-- 3 承诺 --")
t=call('/passkey/assert',{'scope':'accept','parts':[oid],'grade':'commit'})['confirmation']
d=call(f'/orders/{oid}/accept',{},conf=t); chk('state', d.get('state'), 's1')

print("\n-- 4 调度器上链绑定挂单锁仓 --")
for _ in range(60):
    d=call(f'/orders/{oid}')
    if d['state']=='s3' or d.get('terminal'): break
    time.sleep(1)
chk('state', d['state'], 's3')
chk('phase', f"{d['phase']}/{d['actor']}", 'pay/you')
e=d.get('escrow') or {}
print(f"       链上合约 {e.get('contract')}")
pos=subprocess.run([CAST,'call',ESC,
  'positionOf(bytes32)((address,uint256,address,address,bytes32,uint8))',
  subprocess.run([CAST,'keccak',oid],capture_output=True,text=True).stdout.strip(),
  '--rpc-url',RPC],capture_output=True,text=True).stdout.strip()
print(f"       合约里的仓位: {pos[:110]}")
chk('仓位状态是 escrowed(1)', pos.rstrip(')').split(',')[-1].strip(), '1')

print("\n-- 5 上传回执 --")
open('/tmp/e2e-rc.txt','w').write('bank receipt')
up=subprocess.run(['curl','-s','-X','POST','-H','X-Atara-User: Demo','-F','file=@/tmp/e2e-rc.txt',
                   A+'/uploads'],capture_output=True,text=True).stdout
ref=json.loads(up)['file_ref']
d=call(f'/orders/{oid}/receipt',{'file_ref':ref}); chk('state', d['state'], 's3v')

print("\n-- 6 对手方核验（放行的闸门）--")
p=call(f'/orders/{oid}',who=peer); chk('对手方 phase', f"{p['phase']}/{p['actor']}", 'verify/you')
bad=call(f'/orders/{oid}/verify-receipt',{'ok':True})
chk('上传者不能自核', bad.get('error',{}).get('code'), 'NOT_YOUR_CALL')
before=bal(ESC)
d=call(f'/orders/{oid}/verify-receipt',{'ok':True},who=peer); chk('state', d['state'], 's4')

print("\n-- 7 放行：后端签 EIP-712 证明，合约验签后放款 --")
for _ in range(60):
    d=call(f'/orders/{oid}')
    if d.get('terminal'): break
    time.sleep(1)
chk('终态', d['terminal'], 'completed')
after=bal(ESC)
moved=before-after
print(f"       托管合约: {before:,.2f} → {after:,.2f} USDT（放出 {moved:,.2f}）")
chk('放出的量等于订单量', f"{moved:.6f}", f"{float(coin):.6f}")
pos2=subprocess.run([CAST,'call',ESC,
  'positionOf(bytes32)((address,uint256,address,address,bytes32,uint8))',
  subprocess.run([CAST,'keccak',oid],capture_output=True,text=True).stdout.strip(),
  '--rpc-url',RPC],capture_output=True,text=True).stdout.strip()
chk('仓位状态变 released(2)', pos2.rstrip(')').split(',')[-1].strip(), '2')

print()
if FAIL:
    print(f'{len(FAIL)} 处失败：'); [print('  ',f) for f in FAIL]; sys.exit(1)
print('真链端到端全部通过 — 钱确实进了合约，放行确实靠签名证明')
