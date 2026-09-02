#!/usr/bin/env python3
"""端到端冒烟：两条主流程 + 非托管的每一处分叉，各跑到终态。

覆盖：条件支付的两种入金路径、OTC 的买卖方向分叉、确认分级、
额度签发与撤销、按地址加联系人、线程、先撮合后评估。
"""
import json, sys, time, urllib.request, urllib.error

B = "http://localhost:%s/api/v1" % (sys.argv[1] if len(sys.argv) > 1 else "8099")
FAIL = []

def call(path, body=None, who=None, conf=None, method=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(B + path, data=data,
                               method=method or ("POST" if data is not None else "GET"))
    r.add_header("Content-Type", "application/json")
    if who:  r.add_header("X-Atara-User", who)
    if conf: r.add_header("X-Atara-Confirmation", conf)
    try:
        return json.load(urllib.request.urlopen(r))
    except urllib.error.HTTPError as e:
        return json.load(e)

def tok(scope, parts, grade, who=None):
    return call("/passkey/assert", {"scope": scope, "parts": parts, "grade": grade}, who)["confirmation"]

def wait(oid, target, secs=40, who=None):
    for _ in range(secs * 2):
        d = call("/orders/" + oid, who=who)
        if d.get("state") == target or d.get("terminal"):
            return d
        time.sleep(0.5)
    return call("/orders/" + oid, who=who)

def check(label, got, want):
    mark = "ok " if got == want else "FAIL"
    if got != want: FAIL.append(f"{label}: got {got}, want {want}")
    print(f"  [{mark}] {label}: {got}")

print("── 非托管：平台不持有余额 ──")
w = call("/wallet")
check("custody", w["custody"], "self")
print(f"       address {w['address']}  escrow contract {w['escrow_contract']['address']}")

print("\n── 条件支付 · 内置钱包签名入金 ──")
t = tok("order", ["cp-hc", "USDT", "3000"], "commit")
o = call("/orders", {"counterparty_id": "cp-hc", "asset": "USDT", "amount": "3000",
                     "allowance_id": "al-me",
                     "conditions": [{"atom_type": "evidence", "params": {"proof": "Delivery record"}}]}, conf=t)
oid = o["id"]
check("建单停在 fund 站（钱没动）", o["state"], "fund")
bad = call(f"/orders/{oid}/fund", {"via": "wallet"}, conf=tok("fund", [oid, "USDT", "3000"], "commit"))
check("承诺档不能签入金", bad["error"]["code"], "SIGNATURE_REQUIRED")
call(f"/orders/{oid}/fund", {"via": "wallet"}, conf=tok("fund", [oid, "USDT", "3000"], "signature"))
check("入金后到终态", wait(oid, "released")["terminal"], "completed")

print("\n── 条件支付 · 外部钱包打款（承诺档 + 链上检测）──")
t = tok("order", ["cp-kj", "USDT", "500"], "commit")
o2 = call("/orders", {"counterparty_id": "cp-kj", "asset": "USDT", "amount": "500",
                      "allowance_id": "al-me", "conditions": []}, conf=t)
oid2 = o2["id"]
r = call(f"/orders/{oid2}/fund", {"via": "external"}, conf=tok("fund", [oid2, "USDT", "500"], "commit"))
check("外部钱包只需承诺档", r.get("escrow", {}).get("funding_via"), "external")
check("立即释放（空条件集）", wait(oid2, "released")["terminal"], "completed")

print("\n── 先撮合后评估 ──")
m = call("/orders/match", {"intent": "buy", "amount": "73100", "amount_kind": "fiat",
                           "asset": "USDT", "fiat": "CNY"})
check("给出候选（不超过 3 个）", len(m["candidates"]) <= 3 and len(m["candidates"]) > 0, True)
print(f"       扫了 {m['scanned']} 条挂单，头名 {m['candidates'][0]['name']} score {m['candidates'][0]['trust_score']}")

print("\n── OTC · taker 买币（对方挂单时已锁仓 → s1 是验证）──")
d = call("/offers/p1/take", {"amount": "73100", "amount_kind": "fiat", "network": "TRON"})
bid = d["id"]
check("taker 方向", d["otc"]["side"], "buy")
check("轨道第二站", d["rail"][1]["label"], "Escrow verified")
call(f"/orders/{bid}/accept", {}, conf=tok("accept", [bid], "commit"))   # 不动钱，普通按钮档
s3 = wait(bid, "s3")
check("绑定挂单锁仓后轮到我付法币", s3["state"], "s3")
call(f"/orders/{bid}/receipt", {"file_ref": "demo-receipt.pdf"})
v = wait(bid, "s3v")
check("上传回执后待对方核验", v["state"], "s3v")
check("付方此刻无事可做", f"{v['phase']}/{v['actor']}", "lock/auto")
mv = call("/orders/" + bid, who="CrabWalk Trading")
check("同一张单在收方眼里该核验", f"{mv['phase']}/{mv['actor']}", "verify/you")
# 放行的依据是回执被对方核过。自己核自己，等于退回成「等对方点确认」——
# 那正是 V1 要甩掉的东西，所以这条断言是这条链路的要害。
bad = call(f"/orders/{bid}/verify-receipt", {"ok": True})
check("上传者不能核自己的回执", bad["error"]["code"], "NOT_YOUR_CALL")
# 显式以对手方身份核验，不依赖调度器代种子商家核那一支——
# 那支是无人值守的兜底，测真实端点比测兜底有意义。
call(f"/orders/{bid}/verify-receipt", {"ok": True}, who="CrabWalk Trading")
check("买方向走到终态", wait(bid, "s5")["terminal"], "completed")

print("\n── OTC · taker 卖币（自己的币上链 → s1 走确认数）──")
d = call("/offers/p9/take", {"amount": "10000", "amount_kind": "coin", "network": "TRON"})
sid = d["id"]
check("taker 方向", d["otc"]["side"], "sell")
check("轨道第二站", d["rail"][1]["label"], "Escrow funded")
bad = call(f"/orders/{sid}/accept", {"via": "wallet"}, conf=tok("accept", [sid], "commit"))
check("卖币要签名档", bad["error"]["code"], "SIGNATURE_REQUIRED")
call(f"/orders/{sid}/accept", {"via": "wallet"}, conf=tok("accept", [sid], "signature"))
sv = wait(sid, "s3v", 60)
check("对方回执到了轮到我核验", sv["state"], "s3v")
check("我该核验", f"{sv['phase']}/{sv['actor']}", "verify/you")
call(f"/orders/{sid}/verify-receipt", {"ok": True})
check("卖方向走到终态", wait(sid, "s5", 60)["terminal"], "completed")

print("\n── 额度：签发 / 过期 / 撤销 ──")
a = call("/allowances", {"spender": "Ops agent", "kind": "agent", "per_payment": "300",
                         "window_cap": "1200", "cycle": "weekly", "expires": "30 days",
                         "recipients": "Verified providers"},
         conf=tok("allowance", ["Ops agent", "300", "1200"], "signature"))
check("签发上链", bool(a.get("chain_tx")), True)
bad = call("/allowances", {"spender": "X", "per_payment": "5000", "window_cap": "1000",
                           "cycle": "weekly"},
           conf=tok("allowance", ["X", "5000", "1000"], "signature"))
check("单笔不得超过窗口总额", bad["error"]["code"], "CAP_ABOVE_WINDOW")
rv = call("/allowances/" + a["id"], None, method="DELETE")
check("撤销", rv["status"], "revoked")

print("\n── 联系人：一个字段收名字或地址 ──")
addr = call("/wallet", who="Kenji M.")["address"]
c = call("/contacts", {"query": addr, "label": "Client"})
check("按地址加上了", c["name"], "Kenji M.")
bad = call("/contacts", {"query": "0xnope"})
check("查无此人", bad["error"]["code"], "NO_SUCH_ACCOUNT")

print("\n── 线程：一个对手方一条流 ──")
call("/threads/cp-hc/messages", {"body": "Invoice attached, please check."})
th = call("/threads/cp-hc")
kinds = {m["kind"] for m in th["messages"]}
check("聊天/订单/播报同流", kinds >= {"chat", "system"}, True)
print(f"       {len(th['messages'])} 条消息 · {len(th['orders'])} 张订单卡")

print("\n" + ("── 全部通过 ──" if not FAIL else "── 有失败 ──"))
for f in FAIL: print("  " + f)
sys.exit(1 if FAIL else 0)
