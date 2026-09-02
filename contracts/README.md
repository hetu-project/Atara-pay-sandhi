# Atara 合约

两份合约。**未部署到公链**，只在本地测试链验证过。

| | 作用 | 授权模型 |
|---|---|---|
| `AtaraEscrow` | 条件支付与 OTC 结算的托管 | **N-of-M 共识证明**——放行的判断权不在任何单一方手上 |
| `AtaraSpending` | 支配权策略（额度） | **账户自己签**——签发额度的人就是出钱的人，`msg.sender` 即授权 |

两者的授权模型刻意不同。托管里「该不该放行」是需要共识的判断，所以要阈值签名；
额度里「我允许谁花我多少钱」只需要我本人同意，加签名证明是多余的仪式。

```bash
export PATH="$HOME/.foundry/bin:$PATH"
forge build
forge test           # 141 个用例
forge coverage
```

Solidity 0.8.24，零外部依赖（EIP-712 与安全转账都是自己实现的），审计面尽量小。
ERC-20 接口，TRC-20 兼容，同一份字节码可以部到 TRON。

## 一条设计主张

**资金只能通过带阈值签名证明的调用离开这个合约。**

合约里没有 owner 提款、没有「运营方放行」、没有紧急转出。这不是疏漏，是全部设计的落点：

如果后端能直接调 `release()`，那「放行由 AI 共识决定」这件事就只存在于后端进程里，
合约只是个傀儡——协议对外声称的「放行由可核验的凭证触发，不由平台的话」在链上就是假的。
用户要信的还是 Atara，而不是合约。

所以放行与退款都要求 **N-of-M 的共识签名方**对一份 EIP-712 证明签名，
合约自己验签、自己数人数、自己比分数。后端能做的只是**收集签名并转发**。

owner 能做的事被刻意限制在治理层：换签名方名单、改阈值、暂停新入金。
`depositsPaused` **不影响已存在仓位的放行与退款**——否则暂停就等于冻结用户资金，
那是托管，不是非托管。测试 `test_PauseBlocksDepositsNotRelease` 钉住这条。

## 两条资金入口

| 入口 | 用在哪 | 发生什么 |
|---|---|---|
| `deposit(orderId, token, amount, beneficiary)` | 条件支付的付款方；OTC 里 taker **卖币** | 币从调用者转进合约，建一个 Escrowed 仓位 |
| `lockListing(offerId, token, amount)` | 做市方挂单 | **挂出即锁币**。买家看到的可成交量真的在合约里 |

`bindListingLock(orderId, offerId, amount, beneficiary)` 把挂单里锁好的一块划给一笔订单。
OTC **买方向**走这条：币在挂单那一刻就进合约了，绑定没有新的资金动作。

它对调用者不设限，因为它动不了钱——只是把 maker 已锁的量划走一块，
划走之后仍然只能通过带证明的 `release` / `refund` 才能真的转出去。

## 唯一的资金出口

```solidity
release(Attestation att, bytes[] signatures)         // Escrowed → Released
refund(Attestation att, bytes[] signatures)          // Escrowed → Refunded
resolveDispute(Attestation att, bytes[] signatures)  // Disputed → Released | Refunded
```

```solidity
struct Attestation {
    bytes32 orderId;   // 只对这一笔工单有效
    Verdict verdict;   // Release | Refund | Hold
    uint16  score;     // 共识评分 0-100
    uint256 nonce;
    uint256 deadline;  // 过期的共识结论不该还能放款
}
```

放行的四道闸门，缺一不可：

1. `verdict == Release`
2. `score >= minScore`
3. 去重后的签名方数量 `>= threshold`
4. 每个签名方都在名单里，且证明未过期

`signatures` 必须**按恢复出的签名方地址严格升序**排列。升序既把去重做成 O(n)，
又顺手堵住「同一个私钥签两次算两票」——同一个私钥恢复出同一个地址，不满足严格大于。

退款**不看分数**：条件没成立、超时、撤单都走退款，它们与「风险评分」无关。

挂单来的仓位退款**还回挂单**而不是 maker 个人余额——一笔订单没成不等于他要下架。
挂单已下架的情况下才还给 maker（那部分当时算在 `bound` 里没退出去）。

## 与后端的对接

后端现有的 `chain.Chain` 接口（`internal/chain/chain.go`）与这份合约一一对应：

| `chain.Chain` | 合约 |
|---|---|
| `SignDeposit` / `WatchDeposit` | `deposit` |
| `LockListing` / `UnlockListing` | `lockListing` / `unlockListing` |
| `BindListingLock` | `bindListingLock` |
| `Position` | `positionOf` |
| `Release` | `release`（**签名变了**：要多带证明与签名） |
| `Refund` | `refund`（同上） |

`Release` / `Refund` 的签名必须改——现在的接口是 `Release(ctx, orderID, to)`，
合约要求的是证明加签名。接真链时这两个方法要变成：

```go
Release(ctx context.Context, att Attestation, sigs [][]byte) (txHash string, err error)
```

### 共识结果怎么变成证明

现有的放行共识出的是 `agent.Decision{Outcome, Votes, Rationale}`，风控出的是
`agent.Assessment{Score, Passed, Total, Threshold}`。映射关系：

| 后端 | 证明字段 |
|---|---|
| `Outcome == OutcomeRelease` | `verdict = Release` |
| `Outcome == OutcomeHoldForReview` | `verdict = Hold`（不动资金，转 Disputed） |
| 条件不成立 / 超时 / 撤单 | `verdict = Refund` |
| `Assessment.Score` | `score` |

**每个共识签名方各自独立签名。** 这一点是整套设计成立的前提：
如果所有签名都由后端一个进程用同一批私钥生成，阈值就是自欺——一台机器被拿下，
N 个私钥一起丢。真实部署里这 N 个签名方应当是**独立进程、独立密钥、独立主机**，
后端只负责收集与转发。

签名的计算（Go 侧参考）：

```go
// EIP-712 摘要
domainSeparator := keccak256(abi.encode(
    keccak256("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"),
    keccak256("AtaraEscrow"), keccak256("1"), chainID, escrowAddr,
))
structHash := keccak256(abi.encode(
    keccak256("Attestation(bytes32 orderId,uint8 verdict,uint16 score,uint256 nonce,uint256 deadline)"),
    orderID, uint8(verdict), score, nonce, deadline,
))
digest := keccak256(append([]byte{0x19, 0x01}, domainSeparator..., structHash...))
```

合约也暴露了 `hashAttestation(att)`，链下可以直接读它来对账，不必自己拼。

签完之后**按签名方地址升序排列**再提交。

## 部署前必须定的三件事

**没定这三件就不该部署。**

1. **签名方名单与阈值。** 谁持有这 N 把私钥、放在哪、怎么轮换。
   `threshold` 太低等于没有共识，太高等于一个节点掉线就全网停摆。
2. **`minScore`。** 现在的 `mockagent` 出的分与真实风控模型出的分不是一个量纲，
   接真模型前这个数没有依据。
3. **owner 交给谁。** 单个 EOA 持有 owner 意味着一把私钥能换掉整个签名方名单——
   虽然换不走已托管的资金，但能让未来的放行由新名单说话。生产环境应当是多签或时间锁。

## 已知的取舍

**不支持手续费型代币**（转账时扣税）。`deposit` 与 `lockListing` 用实际到账量校验，
不符就直接拒——否则仓位会记着一个合约里并不存在的数，最后放款时会拿别人的钱去补。
测试 `test_RevertWhen_FeeOnTransferToken` 钉住这条。

**不支持原生币**（ETH / TRX）。只走 ERC-20 / TRC-20。要支持原生币需要另一条
`payable` 入口和一套单独的余额核算，那是另一次改动。

**`attestationUsed` 是纵深防御，当前路径下撞不到。** 主守卫是仓位状态机：
放行成功后仓位进终态，同一份证明再来会先撞 `NotEscrowed`。
保留它是为了将来若加入「仓位可回到 Escrowed」的路径（比如部分成交）时，
重放防护已经在位。测试 `test_AttestationMarkedUsedAndCannotReplay` 断言的是
标记确实被置位，没有假装能测出 `AttestationReplayed`。

**未审计。** 141 个用例，Escrow 行覆盖 98.3% / 分支 89.8%，
Spending 行覆盖 100% / 分支 96.3%——都不等于审计过。上真钱之前需要外部审计。


---

# AtaraSpending

## 为什么不能用 ERC-20 的 approve

`approve` 是一个扁平数字：没有周期窗口、没有单笔上限、没有到期。
产品说的「额度」是一份**可撤销、有周期、有单笔天花板**的支配权——
那需要一份策略合约来记和执行。

```solidity
grant(id, spender, token, perPayment, windowCap, cycleSecs, expiresAt)  // 只有账户能调
revoke(id)                                                              // 只有账户能撤
spend(id, amount, to)                                                   // 只有 spender 能调
available(id) / isLive(id) / policyOf(id)                               // 读
```

## 双闸门

花钱要两个条件**同时**成立：

1. **策略允许**（本合约执行）：live、未过期、单笔不超上限、窗口余量够
2. **账户 approve 过本合约**（代币合约执行）

任何一个撤掉，钱就花不出去。撤 approve 是账户随时能拉的紧急刹车，
不必经过策略。测试 `test_RevertWhen_ApprovalRevoked` 钉住这条。

## 两个容易写错的地方

**窗口边界必须稳定。** 滚动时按整数倍前推（`windowStart += (elapsed / cycle) * cycle`），
不是设成当前时间——后者会让每次花钱都把窗口往后推，等效于窗口永远不结束，
周期限制就形同虚设。`test_WindowStartAdvancesInWholeCycles` 守这条。

**改额度不清零窗口用量。** 否则「改一次额度」就成了绕过窗口的手段。
只有换币种才重开窗口（那等于换了一份策略）。`test_RegrantKeepsWindowUsage` 守这条。

## 链上是权威

后端的 `checkAllowance` 在平台侧校验之后，还会读一次链上策略
（`internal/chain/chain.go` 的 `AllowanceState`）。链上说撤了或余量不够，
平台就不放行——**只有平台库记着的额度是装饰，链是权威，平台记录只是缓存**。

`scripts/allowance-e2e.py` 验的就是这件事：绕过平台直接在链上 `revoke`，
平台库里还写着 `live`，下一笔请求立刻拿到 `ALLOWANCE_REVOKED`。
