#!/usr/bin/env python3
"""支配权（额度）的真链端到端。

验的是一件事：**链上策略是权威，平台库只是缓存。**
只有平台库记着的额度是装饰——链上撤了而平台还放行，就是假的非托管。

跑之前：anvil 起着、合约部好（含 ATARA_SPENDING_ADDR）、后端以
ATARA_CHAIN_IMPL=evm 起着。

    set -a; . ./.env; set +a
    python3 scripts/allowance-e2e.py
"""
import json
import os
import subprocess
import sys
import urllib.error
import urllib.request

A = os.environ.get('ATARA_API', 'http://localhost:8080/api/v1')
RPC = os.environ.get('ATARA_RPC_URL', 'http://127.0.0.1:8545')
SP = os.environ['ATARA_SPENDING_ADDR']
KEY = os.environ['ATARA_SIGNER_KEY']
CAST = os.environ.get('CAST', os.path.expanduser('~/.foundry/bin/cast'))
FAIL = []


def call(p, b=None, who='Demo', conf=None):
    d = json.dumps(b).encode() if b is not None else None
    r = urllib.request.Request(A + p, data=d, method='POST' if d is not None else 'GET')
    r.add_header('Content-Type', 'application/json')
    r.add_header('X-Atara-User', who)
    if conf:
        r.add_header('X-Atara-Confirmation', conf)
    try:
        return json.load(urllib.request.urlopen(r))
    except urllib.error.HTTPError as e:
        return json.load(e)


def cast(*args):
    return subprocess.run([CAST, *args, '--rpc-url', RPC],
                          capture_output=True, text=True).stdout.strip()


def kk(s):
    return subprocess.run([CAST, 'keccak', s], capture_output=True, text=True).stdout.strip()


def chk(label, got, want):
    ok = got == want
    if not ok:
        FAIL.append(f'{label}: {got!r} != {want!r}')
    print(f"  [{'ok ' if ok else 'FAIL'}] {label}: {got}")


def order(allowance='al-me', amount='3000'):
    t = call('/passkey/assert', {'scope': 'order', 'parts': ['cp-hc', 'USDT', amount],
                                 'grade': 'commit'})['confirmation']
    return call('/orders', {
        'counterparty_id': 'cp-hc', 'asset': 'USDT', 'amount': amount,
        'allowance_id': allowance,
        'conditions': [{'atom_type': 'evidence', 'params': {'proof': 'Delivery record'}}],
    }, conf=t)


print('-- 1 种子额度真的签上链 --')
for aid, per in [('al-me', 10000), ('al-pa', 500), ('al-da', 200)]:
    chk(f'{aid} 链上 live', cast('call', SP, 'isLive(bytes32)(bool)', kk(aid)), 'true')
    av = int(float(cast('call', SP, 'available(bytes32)(uint256)', kk(aid)).split()[0]) / 1e18)
    chk(f'{aid} 可花额度', av, per)
chk('revoked 的不上链', cast('call', SP, 'isLive(bytes32)(bool)', kk('al-ta')), 'false')

print('\n-- 2 平台侧记下了链上交易哈希 --')
als = {a['id']: a for a in call('/allowances')['allowances']}
chk('al-me 有 chain_tx', bool(als['al-me'].get('chain_tx')), True)
chk('al-ta 没有 chain_tx', bool(als['al-ta'].get('chain_tx')), False)

print('\n-- 3 链上 live 时能放行 --')
d = order()
chk('建单成功', d.get('state'), 'fund')

print('\n-- 4 绕过平台，直接在链上撤销 --')
subprocess.run([CAST, 'send', SP, 'revoke(bytes32)', kk('al-me'),
                '--private-key', KEY, '--rpc-url', RPC],
               capture_output=True, text=True)
chk('链上已撤销', cast('call', SP, 'isLive(bytes32)(bool)', kk('al-me')), 'false')
chk('平台库仍写着 live', als['al-me']['status'], 'live')

print('\n-- 5 链上说撤了，平台必须拒 --')
d = order()
chk('拒绝码', d.get('error', {}).get('code'), 'ALLOWANCE_REVOKED')

print('\n-- 6 单笔上限在链上生效 --')
# al-pa 单笔 500，试 3000
d = order(allowance='al-pa', amount='3000')
chk('超单笔上限被拒', d.get('error', {}).get('code'), 'OVER_CAP')

print()
if FAIL:
    print(f'{len(FAIL)} 处失败：')
    for f in FAIL:
        print('  ', f)
    sys.exit(1)
print('额度真链端到端通过 — 链上策略是权威，平台库只是缓存')
