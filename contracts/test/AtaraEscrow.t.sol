// SPDX-License-Identifier: MIT
pragma solidity 0.8.24;

import {Test} from "forge-std/Test.sol";
import {AtaraEscrow, IERC20} from "../src/AtaraEscrow.sol";

/// @dev 最小 ERC-20。返回 bool，与主流实现一致。
contract MockToken {
    string public name = "Mock USDT";
    uint8 public decimals = 6;
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    function mint(address to, uint256 a) external {
        balanceOf[to] += a;
    }

    function approve(address s, uint256 a) external returns (bool) {
        allowance[msg.sender][s] = a;
        return true;
    }

    function transfer(address to, uint256 a) external virtual returns (bool) {
        balanceOf[msg.sender] -= a;
        balanceOf[to] += a;
        return true;
    }

    function transferFrom(address f, address t, uint256 a) external virtual returns (bool) {
        allowance[f][msg.sender] -= a;
        balanceOf[f] -= a;
        balanceOf[t] += a;
        return true;
    }
}

/// @dev 转账时扣税的代币。合约必须拒绝这种——否则仓位会记着一个
/// 合约里并不存在的数。
contract FeeToken is MockToken {
    function transferFrom(address f, address t, uint256 a) external override returns (bool) {
        allowance[f][msg.sender] -= a;
        balanceOf[f] -= a;
        balanceOf[t] += a - (a / 100); // 扣 1%
        return true;
    }
}

contract AtaraEscrowTest is Test {
    AtaraEscrow esc;
    MockToken tok;

    // 五个共识签名方，阈值 3。私钥固定，方便按地址排序。
    uint256[5] pk;
    address[5] sg;

    address payer = address(0xA11CE);
    address maker = address(0x1A4E5);
    address payee = address(0xB0B);
    address stranger = address(0xDEAD);

    bytes32 constant ORDER = keccak256("order-1");
    bytes32 constant ORDER2 = keccak256("order-2");
    bytes32 constant OFFER = keccak256("offer-1");

    uint16 constant MIN_SCORE = 70;

    function setUp() public {
        tok = new MockToken();
        for (uint256 i = 0; i < 5; i++) {
            pk[i] = 0x1000 + i;
            sg[i] = vm.addr(pk[i]);
        }
        address[] memory list = new address[](5);
        for (uint256 i = 0; i < 5; i++) {
            list[i] = sg[i];
        }
        esc = new AtaraEscrow(list, 3, MIN_SCORE);

        tok.mint(payer, 1_000_000);
        tok.mint(maker, 1_000_000);
        vm.prank(payer);
        tok.approve(address(esc), type(uint256).max);
        vm.prank(maker);
        tok.approve(address(esc), type(uint256).max);
    }

    // ── 助手 ──

    function _att(bytes32 orderId, AtaraEscrow.Verdict v, uint16 score, uint256 nonce)
        internal
        view
        returns (AtaraEscrow.Attestation memory)
    {
        return AtaraEscrow.Attestation({
            orderId: orderId,
            verdict: v,
            score: score,
            nonce: nonce,
            deadline: block.timestamp + 1 hours
        });
    }

    /// @dev 用前 n 个签名方签名，并按恢复出的地址升序排列——合约要求严格升序。
    function _sign(AtaraEscrow.Attestation memory a, uint256 n)
        internal
        view
        returns (bytes[] memory)
    {
        uint256[] memory keys = new uint256[](n);
        for (uint256 i = 0; i < n; i++) {
            keys[i] = pk[i];
        }
        return _signWith(a, keys);
    }

    function _signWith(AtaraEscrow.Attestation memory a, uint256[] memory keys)
        internal
        view
        returns (bytes[] memory out)
    {
        // 按地址冒泡排序，保证升序
        for (uint256 i = 0; i < keys.length; i++) {
            for (uint256 j = i + 1; j < keys.length; j++) {
                if (vm.addr(keys[j]) < vm.addr(keys[i])) {
                    (keys[i], keys[j]) = (keys[j], keys[i]);
                }
            }
        }
        bytes32 digest = esc.hashAttestation(a);
        out = new bytes[](keys.length);
        for (uint256 i = 0; i < keys.length; i++) {
            (uint8 v, bytes32 r, bytes32 s) = vm.sign(keys[i], digest);
            out[i] = abi.encodePacked(r, s, v);
        }
    }

    function _deposit(bytes32 orderId, uint256 amount) internal {
        vm.prank(payer);
        esc.deposit(orderId, address(tok), amount, payee);
    }

    // ══════════════ 正路 ══════════════

    function test_DepositThenRelease() public {
        _deposit(ORDER, 1000);
        assertEq(tok.balanceOf(address(esc)), 1000, unicode"币要真的进合约");
        assertEq(uint8(esc.positionOf(ORDER).status), uint8(AtaraEscrow.Status.Escrowed));

        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        esc.release(a, _sign(a, 3));

        assertEq(tok.balanceOf(payee), 1000, unicode"放给收款方");
        assertEq(tok.balanceOf(address(esc)), 0);
        assertEq(uint8(esc.positionOf(ORDER).status), uint8(AtaraEscrow.Status.Released));
    }

    function test_DepositThenRefund() public {
        _deposit(ORDER, 1000);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Refund, 0, 1);
        esc.refund(a, _sign(a, 3));

        assertEq(tok.balanceOf(payer), 1_000_000, unicode"原路退回");
        assertEq(uint8(esc.positionOf(ORDER).status), uint8(AtaraEscrow.Status.Refunded));
    }

    /// 退款不看分数：条件没成立、超时、撤单与「风险评分」无关。
    function test_RefundIgnoresScore() public {
        _deposit(ORDER, 1000);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Refund, 0, 1);
        esc.refund(a, _sign(a, 3));
        assertEq(tok.balanceOf(payer), 1_000_000);
    }

    /// 买方向 OTC：币在挂单那一刻就进了合约，绑定只是划一块额度出来。
    function test_LockListingBindThenRelease() public {
        vm.prank(maker);
        esc.lockListing(OFFER, address(tok), 5000);
        assertEq(tok.balanceOf(address(esc)), 5000);
        assertEq(esc.listingAvailable(OFFER), 5000);

        esc.bindListingLock(ORDER, OFFER, 1200, payee);
        assertEq(esc.listingAvailable(OFFER), 3800, unicode"绑走的量要从可成交量里扣掉");
        assertEq(esc.positionOf(ORDER).payer, maker, unicode"出币方是 maker");

        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 90, 1);
        esc.release(a, _sign(a, 3));
        assertEq(tok.balanceOf(payee), 1200);
        assertEq(tok.balanceOf(address(esc)), 3800, unicode"剩下的还锁在挂单里");
    }

    /// 挂单来的仓位退款要还回挂单，不是还给 maker 个人——
    /// 一笔订单没成不等于他要下架。
    function test_RefundBoundPositionGoesBackToListing() public {
        vm.prank(maker);
        esc.lockListing(OFFER, address(tok), 5000);
        esc.bindListingLock(ORDER, OFFER, 1200, payee);
        assertEq(esc.listingAvailable(OFFER), 3800);

        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Refund, 0, 1);
        esc.refund(a, _sign(a, 3));

        assertEq(esc.listingAvailable(OFFER), 5000, unicode"还回挂单，重新变成可成交量");
        assertEq(tok.balanceOf(address(esc)), 5000, unicode"币没离开合约");
        assertEq(tok.balanceOf(maker), 995_000, unicode"maker 个人余额没变");
    }

    /// 下架后再退款，那部分才还给 maker——它当时算在 bound 里没退出去。
    function test_RefundAfterUnlistGoesToMaker() public {
        vm.prank(maker);
        esc.lockListing(OFFER, address(tok), 5000);
        esc.bindListingLock(ORDER, OFFER, 1200, payee);

        vm.prank(maker);
        esc.unlockListing(OFFER);
        assertEq(tok.balanceOf(maker), 995_000 + 3800, unicode"只退没被绑走的部分");

        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Refund, 0, 1);
        esc.refund(a, _sign(a, 3));
        assertEq(tok.balanceOf(maker), 1_000_000, unicode"绑走那部分现在还回来");
        assertEq(tok.balanceOf(address(esc)), 0);
    }

    function test_UnlockListingOnlyReturnsFree() public {
        vm.prank(maker);
        esc.lockListing(OFFER, address(tok), 5000);
        esc.bindListingLock(ORDER, OFFER, 1200, payee);
        vm.prank(maker);
        esc.unlockListing(OFFER);
        assertEq(esc.listingAvailable(OFFER), 0, unicode"下架后没有可成交量");
        assertEq(tok.balanceOf(address(esc)), 1200, unicode"绑走的还在合约里等那笔单走完");
    }

    function test_LockListingTopUp() public {
        vm.prank(maker);
        esc.lockListing(OFFER, address(tok), 1000);
        vm.prank(maker);
        esc.lockListing(OFFER, address(tok), 500);
        assertEq(esc.listingAvailable(OFFER), 1500);
    }

    // ══════════════ 证明校验：攻击面 ══════════════

    /// 同一份证明只能用一次。
    ///
    /// 注意实际的主守卫是仓位状态机：放行成功后仓位进入终态，同一份证明
    /// 再来会先撞上 NotEscrowed。attestationUsed 是压在状态机下面的纵深防御，
    /// 当前路径下先撞不到它——所以这里断言的是它确实被置位了，
    /// 而不是假装能测出 AttestationReplayed。
    function test_AttestationMarkedUsedAndCannotReplay() public {
        _deposit(ORDER, 1000);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        bytes32 digest = esc.hashAttestation(a);
        assertFalse(esc.attestationUsed(digest));

        bytes[] memory sigs = _sign(a, 3);
        esc.release(a, sigs);
        assertTrue(esc.attestationUsed(digest), unicode"证明用过就该作废");

        // 再来一次：状态机先挡住
        vm.expectRevert(AtaraEscrow.NotEscrowed.selector);
        esc.release(a, sigs);
        assertEq(tok.balanceOf(payee), 1000, unicode"只放了一次");
    }

    /// 给 A 单签的证明不能用来放 B 单——orderId 在摘要里。
    function test_RevertWhen_AttestationForOtherOrder() public {
        _deposit(ORDER, 1000);
        _deposit(ORDER2, 2000);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        bytes[] memory sigs = _sign(a, 3);

        // 把 orderId 换成另一单，摘要就变了，签名恢复出的是随机地址。
        // 具体报 SignersUnsorted 还是 BadSignature 取决于那几个随机地址的
        // 大小关系，不确定——所以这里只断言「一定回滚」，并验资金没动。
        AtaraEscrow.Attestation memory b = a;
        b.orderId = ORDER2;
        vm.expectRevert();
        esc.release(b, sigs);
        assertEq(tok.balanceOf(address(esc)), 3000, unicode"两笔都还在");
        assertEq(tok.balanceOf(payee), 0);
    }

    /// 分数不够不能放行。这是「AI 共识评分」在链上真正生效的地方。
    function test_RevertWhen_ScoreBelowMin() public {
        _deposit(ORDER, 1000);
        AtaraEscrow.Attestation memory a =
            _att(ORDER, AtaraEscrow.Verdict.Release, MIN_SCORE - 1, 1);
        bytes[] memory _sa = _sign(a, 3);
        vm.expectRevert(AtaraEscrow.ScoreTooLow.selector);
        esc.release(a, _sa);
    }

    function test_ScoreExactlyAtMinPasses() public {
        _deposit(ORDER, 1000);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, MIN_SCORE, 1);
        esc.release(a, _sign(a, 3));
        assertEq(tok.balanceOf(payee), 1000, unicode"刚好到线要能过");
    }

    /// 签名方不够阈值不能放行。
    function test_RevertWhen_BelowThreshold() public {
        _deposit(ORDER, 1000);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        bytes[] memory _sa = _sign(a, 2);
        vm.expectRevert(AtaraEscrow.NotEnoughSigners.selector);
        esc.release(a, _sa);
    }

    /// 同一个私钥签三次不算三票。严格升序把这条堵死。
    function test_RevertWhen_SameSignerRepeated() public {
        _deposit(ORDER, 1000);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        bytes32 digest = esc.hashAttestation(a);
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(pk[0], digest);
        bytes[] memory sigs = new bytes[](3);
        sigs[0] = abi.encodePacked(r, s, v);
        sigs[1] = sigs[0];
        sigs[2] = sigs[0];
        vm.expectRevert(AtaraEscrow.SignersUnsorted.selector);
        esc.release(a, sigs);
    }

    /// 名单外的人签名无效，即便凑够了数量。
    function test_RevertWhen_NonSignerSigns() public {
        _deposit(ORDER, 1000);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        uint256[] memory keys = new uint256[](3);
        keys[0] = pk[0];
        keys[1] = pk[1];
        keys[2] = 0xBADBAD; // 不在名单里
        bytes[] memory _sw = _signWith(a, keys);
        vm.expectRevert(AtaraEscrow.BadSignature.selector);
        esc.release(a, _sw);
    }

    /// 顺序不对直接拒——去重靠的就是这个不变量。
    function test_RevertWhen_SignaturesUnsorted() public {
        _deposit(ORDER, 1000);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        bytes[] memory sorted = _sign(a, 3);
        bytes[] memory reversed = new bytes[](3);
        reversed[0] = sorted[2];
        reversed[1] = sorted[1];
        reversed[2] = sorted[0];
        vm.expectRevert(AtaraEscrow.SignersUnsorted.selector);
        esc.release(a, reversed);
    }

    /// 过期的共识结论不该还能放款。
    function test_RevertWhen_AttestationExpired() public {
        _deposit(ORDER, 1000);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        bytes[] memory sigs = _sign(a, 3);
        vm.warp(block.timestamp + 2 hours);
        vm.expectRevert(AtaraEscrow.AttestationExpired.selector);
        esc.release(a, sigs);
    }

    /// 退款证明不能当放行证明用，反之亦然。
    function test_RevertWhen_WrongVerdict() public {
        _deposit(ORDER, 1000);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Refund, 90, 1);
        bytes[] memory _sa = _sign(a, 3);
        vm.expectRevert(AtaraEscrow.WrongVerdict.selector);
        esc.release(a, _sa);

        AtaraEscrow.Attestation memory b = _att(ORDER, AtaraEscrow.Verdict.Release, 90, 2);
        bytes[] memory _sb = _sign(b, 3);
        vm.expectRevert(AtaraEscrow.WrongVerdict.selector);
        esc.refund(b, _sb);
    }

    /// 签名可塑性：翻转 s 与 v 得到的另一份有效签名，会绕过按地址去重。
    /// 必须凑够阈值数量才能走到验签那一步——长度检查在前面。
    function test_RevertWhen_MalleableSignature() public {
        _deposit(ORDER, 1000);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        bytes[] memory sigs = _sign(a, 3);

        // 把第一条换成它的可塑性孪生：同一个私钥、同样有效、但 s 在上半区
        bytes32 digest = esc.hashAttestation(a);
        uint256 N = 0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141;
        uint256 lowest = pk[0];
        for (uint256 i = 1; i < 3; i++) {
            if (vm.addr(pk[i]) < vm.addr(lowest)) lowest = pk[i];
        }
        (uint8 v, bytes32 r, bytes32 sv) = vm.sign(lowest, digest);
        sigs[0] = abi.encodePacked(r, bytes32(N - uint256(sv)), v == 27 ? uint8(28) : uint8(27));

        vm.expectRevert(AtaraEscrow.BadSignature.selector);
        esc.release(a, sigs);
        assertEq(tok.balanceOf(address(esc)), 1000, unicode"高位 s 一律拒");
    }

    function test_RevertWhen_SignatureWrongLength() public {
        _deposit(ORDER, 1000);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        bytes[] memory sigs = new bytes[](3);
        sigs[0] = hex"1234";
        sigs[1] = hex"1234";
        sigs[2] = hex"1234";
        vm.expectRevert(AtaraEscrow.BadSignature.selector);
        esc.release(a, sigs);
    }

    // ══════════════ 状态机 ══════════════

    /// 终态后只读。已放行的仓位不能再退款。
    function test_RevertWhen_ReleasedThenRefund() public {
        _deposit(ORDER, 1000);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        esc.release(a, _sign(a, 3));

        AtaraEscrow.Attestation memory b = _att(ORDER, AtaraEscrow.Verdict.Refund, 0, 2);
        bytes[] memory _sb = _sign(b, 3);
        vm.expectRevert(AtaraEscrow.NotEscrowed.selector);
        esc.refund(b, _sb);
    }

    function test_RevertWhen_NoPosition() public {
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        bytes[] memory _sa = _sign(a, 3);
        vm.expectRevert(AtaraEscrow.NoPosition.selector);
        esc.release(a, _sa);
    }

    function test_RevertWhen_OrderIdReused() public {
        _deposit(ORDER, 1000);
        vm.prank(payer);
        vm.expectRevert(AtaraEscrow.OrderExists.selector);
        esc.deposit(ORDER, address(tok), 500, payee);
    }

    /// 有异议的单不能走普通放行——免得一份放行证明把它悄悄放掉。
    function test_RevertWhen_ReleaseDisputed() public {
        _deposit(ORDER, 1000);
        vm.prank(payer);
        esc.raiseDispute(ORDER);
        assertEq(uint8(esc.positionOf(ORDER).status), uint8(AtaraEscrow.Status.Disputed));

        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 95, 1);
        bytes[] memory _sa = _sign(a, 3);
        vm.expectRevert(AtaraEscrow.NotEscrowed.selector);
        esc.release(a, _sa);
        assertEq(tok.balanceOf(address(esc)), 1000, unicode"资金保持锁定");
    }

    function test_ResolveDisputeRelease() public {
        _deposit(ORDER, 1000);
        vm.prank(payee);
        esc.raiseDispute(ORDER);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 88, 1);
        esc.resolveDispute(a, _sign(a, 3));
        assertEq(tok.balanceOf(payee), 1000);
    }

    function test_ResolveDisputeRefund() public {
        _deposit(ORDER, 1000);
        vm.prank(payer);
        esc.raiseDispute(ORDER);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Refund, 0, 1);
        esc.resolveDispute(a, _sign(a, 3));
        assertEq(tok.balanceOf(payer), 1_000_000);
    }

    /// Hold 不是资金动作，裁决时给 Hold 应当被拒。
    function test_RevertWhen_ResolveWithHold() public {
        _deposit(ORDER, 1000);
        vm.prank(payer);
        esc.raiseDispute(ORDER);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Hold, 0, 1);
        bytes[] memory _sa = _sign(a, 3);
        vm.expectRevert(AtaraEscrow.WrongVerdict.selector);
        esc.resolveDispute(a, _sa);
    }

    /// 局外人不能替别人提异议。
    function test_RevertWhen_StrangerDisputes() public {
        _deposit(ORDER, 1000);
        vm.prank(stranger);
        vm.expectRevert(AtaraEscrow.NotParty.selector);
        esc.raiseDispute(ORDER);
    }

    function test_RevertWhen_BindMoreThanAvailable() public {
        vm.prank(maker);
        esc.lockListing(OFFER, address(tok), 1000);
        vm.expectRevert(AtaraEscrow.ListingInsufficient.selector);
        esc.bindListingLock(ORDER, OFFER, 1001, payee);
    }

    function test_RevertWhen_UnlockByStranger() public {
        vm.prank(maker);
        esc.lockListing(OFFER, address(tok), 1000);
        vm.prank(stranger);
        vm.expectRevert(AtaraEscrow.ListingNotOpen.selector);
        esc.unlockListing(OFFER);
    }

    // ══════════════ 治理的边界 ══════════════

    /// **最要紧的一条**：owner 动不了任何一笔已托管的资金。
    /// 合约里没有提款、没有紧急转出、没有「运营方放行」。
    function test_OwnerCannotMoveFunds() public {
        _deposit(ORDER, 1000);
        uint256 before = tok.balanceOf(address(esc));

        // 把签名方换成 owner 自己一个人，阈值 1——治理能做到的极限
        address[] memory solo = new address[](1);
        solo[0] = address(this);
        esc.setSigners(solo, 1, 0);

        // 但 owner 不是共识签名方的私钥持有者：他没法为 address(this) 产出
        // ecrecover 能恢复的签名（合约地址没有私钥）。资金依然出不来。
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 100, 1);
        bytes[] memory sigs = new bytes[](1);
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(pk[0], esc.hashAttestation(a));
        sigs[0] = abi.encodePacked(r, s, v); // sg[0] 已被移出名单
        vm.expectRevert(AtaraEscrow.BadSignature.selector);
        esc.release(a, sigs);

        assertEq(tok.balanceOf(address(esc)), before, unicode"资金一分没动");
    }

    /// 暂停只挡新入金，**不能影响已有仓位的放行与退款**——
    /// 否则暂停就等于冻结用户资金，那是托管不是非托管。
    function test_PauseBlocksDepositsNotRelease() public {
        _deposit(ORDER, 1000);
        esc.setDepositsPaused(true);

        vm.prank(payer);
        vm.expectRevert(AtaraEscrow.DepositsArePaused.selector);
        esc.deposit(ORDER2, address(tok), 500, payee);

        vm.prank(maker);
        vm.expectRevert(AtaraEscrow.DepositsArePaused.selector);
        esc.lockListing(OFFER, address(tok), 500);

        // 已有仓位照常放行
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        esc.release(a, _sign(a, 3));
        assertEq(tok.balanceOf(payee), 1000, unicode"暂停不能冻结已托管的资金");
    }

    function test_RevertWhen_NonOwnerGoverns() public {
        address[] memory one = new address[](1);
        one[0] = sg[0];
        vm.prank(stranger);
        vm.expectRevert(AtaraEscrow.NotOwner.selector);
        esc.setSigners(one, 1, 0);

        vm.prank(stranger);
        vm.expectRevert(AtaraEscrow.NotOwner.selector);
        esc.setDepositsPaused(true);
    }

    /// 阈值大于名单长度是配置错误——那样谁都放不了款，资金永久卡住。
    function test_RevertWhen_ThresholdAboveSignerCount() public {
        address[] memory two = new address[](2);
        two[0] = sg[0];
        two[1] = sg[1];
        vm.expectRevert(AtaraEscrow.BadThreshold.selector);
        esc.setSigners(two, 3, MIN_SCORE);
    }

    function test_RevertWhen_ThresholdZero() public {
        address[] memory two = new address[](2);
        two[0] = sg[0];
        two[1] = sg[1];
        vm.expectRevert(AtaraEscrow.BadThreshold.selector);
        esc.setSigners(two, 0, MIN_SCORE);
    }

    /// 名单里有重复地址会让阈值形同虚设。
    function test_RevertWhen_DuplicateSignerInList() public {
        address[] memory dup = new address[](3);
        dup[0] = sg[0];
        dup[1] = sg[1];
        dup[2] = sg[0];
        vm.expectRevert(AtaraEscrow.BadSignature.selector);
        esc.setSigners(dup, 2, MIN_SCORE);
    }

    /// 换名单后，旧名单的签名立刻失效。
    function test_SignerRotationInvalidatesOldSigners() public {
        _deposit(ORDER, 1000);
        address[] memory nw = new address[](3);
        nw[0] = sg[2];
        nw[1] = sg[3];
        nw[2] = sg[4];
        esc.setSigners(nw, 3, MIN_SCORE);

        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        bytes[] memory _sa = _sign(a, 3);
        vm.expectRevert(AtaraEscrow.BadSignature.selector);
        esc.release(a, _sa); // 用的是 pk[0..2]，其中两个已被移出

        uint256[] memory keys = new uint256[](3);
        keys[0] = pk[2];
        keys[1] = pk[3];
        keys[2] = pk[4];
        esc.release(a, _signWith(a, keys));
        assertEq(tok.balanceOf(payee), 1000);
    }

    // ══════════════ 代币兼容性 ══════════════

    /// 手续费型代币：实际到账与 amount 不符，直接拒绝。
    /// 否则仓位会记着一个合约里并不存在的数，最后放款时会拿别人的钱去补。
    function test_RevertWhen_FeeOnTransferToken() public {
        FeeToken fee = new FeeToken();
        fee.mint(payer, 10_000);
        vm.prank(payer);
        fee.approve(address(esc), type(uint256).max);
        vm.prank(payer);
        vm.expectRevert(AtaraEscrow.TransferFailed.selector);
        esc.deposit(ORDER, address(fee), 1000, payee);
    }

    function test_RevertWhen_ZeroAmountOrAddress() public {
        vm.prank(payer);
        vm.expectRevert(AtaraEscrow.ZeroAmount.selector);
        esc.deposit(ORDER, address(tok), 0, payee);

        vm.prank(payer);
        vm.expectRevert(AtaraEscrow.ZeroAddress.selector);
        esc.deposit(ORDER, address(tok), 100, address(0));
    }

    // ══════════════ 不变量 ══════════════

    /// 合约余额永远 >= 所有未终态仓位之和。用模糊测试压几组金额。
    function testFuzz_SolvencyAfterRelease(uint96 a1, uint96 a2) public {
        vm.assume(a1 > 0 && a2 > 0);
        vm.assume(uint256(a1) + uint256(a2) <= 1_000_000);

        _deposit(ORDER, a1);
        _deposit(ORDER2, a2);
        assertEq(tok.balanceOf(address(esc)), uint256(a1) + uint256(a2));

        AtaraEscrow.Attestation memory att = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        esc.release(att, _sign(att, 3));

        assertEq(tok.balanceOf(address(esc)), a2, unicode"放完一笔，剩下的正好是另一笔");
        assertEq(tok.balanceOf(payee), a1);
    }

    /// 摘要必须随每个字段变化——任何一个字段能被改而摘要不变，就是伪造入口。
    function testFuzz_DigestBindsEveryField(
        bytes32 orderId,
        uint8 verdict,
        uint16 score,
        uint256 nonce,
        uint256 deadline
    ) public view {
        verdict = uint8(bound(verdict, 0, 2));
        AtaraEscrow.Attestation memory a = AtaraEscrow.Attestation({
            orderId: orderId,
            verdict: AtaraEscrow.Verdict(verdict),
            score: score,
            nonce: nonce,
            deadline: deadline
        });
        bytes32 base = esc.hashAttestation(a);

        AtaraEscrow.Attestation memory b = a;
        b.orderId = orderId ^ bytes32(uint256(1));
        assertTrue(esc.hashAttestation(b) != base, unicode"orderId 变了摘要必须变");

        b = a;
        b.score = score ^ 1;
        assertTrue(esc.hashAttestation(b) != base, unicode"score 变了摘要必须变");

        b = a;
        b.nonce = nonce ^ 1;
        assertTrue(esc.hashAttestation(b) != base, unicode"nonce 变了摘要必须变");

        b = a;
        b.deadline = deadline ^ 1;
        assertTrue(esc.hashAttestation(b) != base, unicode"deadline 变了摘要必须变");
    }

    /// 分叉后域分隔符必须重算，否则一条链上的证明能在另一条链上重放。
    function test_DomainSeparatorChangesOnFork() public {
        bytes32 before = esc.domainSeparator();
        vm.chainId(block.chainid + 1);
        assertTrue(esc.domainSeparator() != before, unicode"chainId 变了域分隔符必须变");
    }
}

/// @dev 转账返回 false 的代币。不 revert，只是返回失败——
/// 不检查返回值的合约会以为转成功了。
contract FalseToken is MockToken {
    bool public failTransfer;
    bool public failPull;

    function setFail(bool t, bool p) external {
        failTransfer = t;
        failPull = p;
    }

    function transfer(address to, uint256 a) external override returns (bool) {
        if (failTransfer) return false;
        balanceOf[msg.sender] -= a;
        balanceOf[to] += a;
        return true;
    }

    function transferFrom(address f, address t, uint256 a) external override returns (bool) {
        if (failPull) return false;
        allowance[f][msg.sender] -= a;
        balanceOf[f] -= a;
        balanceOf[t] += a;
        return true;
    }
}

/// 补齐分支覆盖。拿着钱的合约，59% 分支覆盖不够。
contract AtaraEscrowBranchTest is AtaraEscrowTest {
    // ── 转账失败路径 ──

    /// 返回 false 的代币必须被识别为失败，不能当成功。
    function test_RevertWhen_PullReturnsFalse() public {
        FalseToken ft = new FalseToken();
        ft.mint(payer, 10_000);
        vm.prank(payer);
        ft.approve(address(esc), type(uint256).max);
        ft.setFail(false, true);

        vm.prank(payer);
        vm.expectRevert(AtaraEscrow.TransferFailed.selector);
        esc.deposit(ORDER, address(ft), 1000, payee);
    }

    /// 放款时转账失败，整笔交易必须回滚——不能出现「状态改了钱没到」。
    function test_RevertWhen_PushReturnsFalse() public {
        FalseToken ft = new FalseToken();
        ft.mint(payer, 10_000);
        vm.prank(payer);
        ft.approve(address(esc), type(uint256).max);
        vm.prank(payer);
        esc.deposit(ORDER, address(ft), 1000, payee);

        ft.setFail(true, false);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        bytes[] memory sigs = _sign(a, 3);
        vm.expectRevert(AtaraEscrow.TransferFailed.selector);
        esc.release(a, sigs);

        assertEq(
            uint8(esc.positionOf(ORDER).status),
            uint8(AtaraEscrow.Status.Escrowed),
            unicode"回滚后状态要还原，不能停在 Released"
        );
    }

    // ── 挂单锁仓的边界 ──

    function test_RevertWhen_TopUpByWrongMaker() public {
        vm.prank(maker);
        esc.lockListing(OFFER, address(tok), 1000);
        tok.mint(stranger, 1000);
        vm.prank(stranger);
        tok.approve(address(esc), type(uint256).max);
        vm.prank(stranger);
        vm.expectRevert(AtaraEscrow.ListingNotOpen.selector);
        esc.lockListing(OFFER, address(tok), 500);
    }

    function test_RevertWhen_TopUpWithDifferentToken() public {
        vm.prank(maker);
        esc.lockListing(OFFER, address(tok), 1000);
        MockToken other = new MockToken();
        other.mint(maker, 1000);
        vm.prank(maker);
        other.approve(address(esc), type(uint256).max);
        vm.prank(maker);
        vm.expectRevert(AtaraEscrow.ListingNotOpen.selector);
        esc.lockListing(OFFER, address(other), 500);
    }

    function test_RevertWhen_TopUpAfterUnlist() public {
        vm.prank(maker);
        esc.lockListing(OFFER, address(tok), 1000);
        vm.prank(maker);
        esc.unlockListing(OFFER);
        vm.prank(maker);
        vm.expectRevert(AtaraEscrow.ListingNotOpen.selector);
        esc.lockListing(OFFER, address(tok), 500);
    }

    function test_RevertWhen_UnlockTwice() public {
        vm.prank(maker);
        esc.lockListing(OFFER, address(tok), 1000);
        vm.prank(maker);
        esc.unlockListing(OFFER);
        vm.prank(maker);
        vm.expectRevert(AtaraEscrow.ListingNotOpen.selector);
        esc.unlockListing(OFFER);
    }

    function test_RevertWhen_BindOnClosedListing() public {
        vm.prank(maker);
        esc.lockListing(OFFER, address(tok), 1000);
        vm.prank(maker);
        esc.unlockListing(OFFER);
        vm.expectRevert(AtaraEscrow.ListingNotOpen.selector);
        esc.bindListingLock(ORDER, OFFER, 100, payee);
    }

    function test_RevertWhen_BindOnNonexistentListing() public {
        vm.expectRevert(AtaraEscrow.ListingNotOpen.selector);
        esc.bindListingLock(ORDER, keccak256("nope"), 100, payee);
    }

    function test_RevertWhen_BindZeroAmountOrAddress() public {
        vm.prank(maker);
        esc.lockListing(OFFER, address(tok), 1000);
        vm.expectRevert(AtaraEscrow.ZeroAmount.selector);
        esc.bindListingLock(ORDER, OFFER, 0, payee);
        vm.expectRevert(AtaraEscrow.ZeroAddress.selector);
        esc.bindListingLock(ORDER, OFFER, 100, address(0));
    }

    function test_RevertWhen_BindSameOrderTwice() public {
        vm.prank(maker);
        esc.lockListing(OFFER, address(tok), 1000);
        esc.bindListingLock(ORDER, OFFER, 100, payee);
        vm.expectRevert(AtaraEscrow.OrderExists.selector);
        esc.bindListingLock(ORDER, OFFER, 100, payee);
    }

    function test_RevertWhen_LockListingZero() public {
        vm.prank(maker);
        vm.expectRevert(AtaraEscrow.ZeroAmount.selector);
        esc.lockListing(OFFER, address(tok), 0);
        vm.prank(maker);
        vm.expectRevert(AtaraEscrow.ZeroAddress.selector);
        esc.lockListing(OFFER, address(0), 100);
    }

    function test_ListingAvailableIsZeroWhenClosed() public {
        vm.prank(maker);
        esc.lockListing(OFFER, address(tok), 1000);
        vm.prank(maker);
        esc.unlockListing(OFFER);
        assertEq(esc.listingAvailable(OFFER), 0);
        assertEq(esc.listingAvailable(keccak256("never")), 0, unicode"不存在的挂单也是 0");
    }

    /// 两笔单都绑在同一个挂单上，下架后逐笔退款要各自退对
    function test_TwoBoundPositionsRefundAfterUnlist() public {
        vm.prank(maker);
        esc.lockListing(OFFER, address(tok), 5000);
        esc.bindListingLock(ORDER, OFFER, 1000, payee);
        esc.bindListingLock(ORDER2, OFFER, 2000, payee);
        vm.prank(maker);
        esc.unlockListing(OFFER);
        assertEq(tok.balanceOf(maker), 995_000 + 2000, unicode"先退没绑走的 2000");

        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Refund, 0, 1);
        esc.refund(a, _sign(a, 3));
        assertEq(tok.balanceOf(maker), 995_000 + 3000);

        AtaraEscrow.Attestation memory b = _att(ORDER2, AtaraEscrow.Verdict.Refund, 0, 2);
        esc.refund(b, _sign(b, 3));
        assertEq(tok.balanceOf(maker), 1_000_000, unicode"全退回来了");
        assertEq(tok.balanceOf(address(esc)), 0, unicode"合约清空");
    }

    // ── 异议的边界 ──

    function test_RevertWhen_DisputeNonexistent() public {
        vm.prank(payer);
        vm.expectRevert(AtaraEscrow.NoPosition.selector);
        esc.raiseDispute(keccak256("nope"));
    }

    function test_RevertWhen_DisputeAfterRelease() public {
        _deposit(ORDER, 1000);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        esc.release(a, _sign(a, 3));
        vm.prank(payer);
        vm.expectRevert(AtaraEscrow.NotEscrowed.selector);
        esc.raiseDispute(ORDER);
    }

    function test_RevertWhen_DisputeTwice() public {
        _deposit(ORDER, 1000);
        vm.prank(payer);
        esc.raiseDispute(ORDER);
        vm.prank(payee);
        vm.expectRevert(AtaraEscrow.NotEscrowed.selector);
        esc.raiseDispute(ORDER);
    }

    function test_RevertWhen_ResolveNonDisputed() public {
        _deposit(ORDER, 1000);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        bytes[] memory sigs = _sign(a, 3);
        vm.expectRevert(AtaraEscrow.NotDisputed.selector);
        esc.resolveDispute(a, sigs);
    }

    function test_RevertWhen_ResolveNonexistent() public {
        AtaraEscrow.Attestation memory a =
            _att(keccak256("nope"), AtaraEscrow.Verdict.Release, 85, 1);
        bytes[] memory sigs = _sign(a, 3);
        vm.expectRevert(AtaraEscrow.NoPosition.selector);
        esc.resolveDispute(a, sigs);
    }

    function test_RevertWhen_RefundNonexistent() public {
        AtaraEscrow.Attestation memory a = _att(keccak256("nope"), AtaraEscrow.Verdict.Refund, 0, 1);
        bytes[] memory sigs = _sign(a, 3);
        vm.expectRevert(AtaraEscrow.NoPosition.selector);
        esc.refund(a, sigs);
    }

    // ── 治理的边界 ──

    function test_TransferOwnership() public {
        esc.transferOwnership(stranger);
        assertEq(esc.owner(), stranger);
        address[] memory one = new address[](1);
        one[0] = sg[0];
        vm.expectRevert(AtaraEscrow.NotOwner.selector);
        esc.setSigners(one, 1, 0);
    }

    function test_RevertWhen_TransferOwnershipToZero() public {
        vm.expectRevert(AtaraEscrow.ZeroAddress.selector);
        esc.transferOwnership(address(0));
    }

    function test_RevertWhen_NonOwnerTransfersOwnership() public {
        vm.prank(stranger);
        vm.expectRevert(AtaraEscrow.NotOwner.selector);
        esc.transferOwnership(stranger);
    }

    function test_RevertWhen_MinScoreAbove100() public {
        address[] memory one = new address[](1);
        one[0] = sg[0];
        vm.expectRevert(AtaraEscrow.ScoreTooLow.selector);
        esc.setSigners(one, 1, 101);
    }

    function test_RevertWhen_SignerListHasZeroAddress() public {
        address[] memory bad = new address[](2);
        bad[0] = sg[0];
        bad[1] = address(0);
        vm.expectRevert(AtaraEscrow.ZeroAddress.selector);
        esc.setSigners(bad, 1, MIN_SCORE);
    }

    function test_SignersViewReflectsList() public {
        address[] memory got = esc.signers();
        assertEq(got.length, 5);
        for (uint256 i = 0; i < 5; i++) {
            assertEq(got[i], sg[i]);
            assertTrue(esc.isSigner(sg[i]));
        }
        address[] memory two = new address[](2);
        two[0] = sg[3];
        two[1] = sg[4];
        esc.setSigners(two, 2, MIN_SCORE);
        assertEq(esc.signers().length, 2);
        assertFalse(esc.isSigner(sg[0]), unicode"换名单要把旧成员清掉");
    }

    function test_UnpauseRestoresDeposits() public {
        esc.setDepositsPaused(true);
        esc.setDepositsPaused(false);
        _deposit(ORDER, 1000);
        assertEq(tok.balanceOf(address(esc)), 1000);
    }

    /// 超过阈值的签名数也该接受——多签了不是错。
    function test_MoreSignaturesThanThresholdIsFine() public {
        _deposit(ORDER, 1000);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        esc.release(a, _sign(a, 5));
        assertEq(tok.balanceOf(payee), 1000);
    }

    /// v 用 0/1 而非 27/28 的签名也该接受——两种编码都在流通。
    function test_LowVEncodingAccepted() public {
        _deposit(ORDER, 1000);
        AtaraEscrow.Attestation memory a = _att(ORDER, AtaraEscrow.Verdict.Release, 85, 1);
        bytes[] memory sigs = _sign(a, 3);
        for (uint256 i = 0; i < 3; i++) {
            bytes memory sig = sigs[i];
            sig[64] = bytes1(uint8(sig[64]) - 27);
            sigs[i] = sig;
        }
        esc.release(a, sigs);
        assertEq(tok.balanceOf(payee), 1000);
    }
}
