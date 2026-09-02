// SPDX-License-Identifier: MIT
pragma solidity 0.8.24;

import {Test} from "forge-std/Test.sol";
import {AtaraSpending} from "../src/AtaraSpending.sol";
import {TestStablecoin} from "../src/TestStablecoin.sol";

contract AtaraSpendingTest is Test {
    AtaraSpending sp;
    TestStablecoin tok;

    address account = address(0xA11CE);
    address spender = address(0xA6E27); // agent
    address dest = address(0xB0B);
    address stranger = address(0xDEAD);

    bytes32 constant ID = keccak256("allowance-1");

    uint64 constant WEEK = 7 days;
    uint256 constant PER = 300 ether;
    uint256 constant CAP = 1200 ether;

    function setUp() public {
        // BSC 上 USDT/USDC 都是 18 位
        tok = new TestStablecoin("Test BSC-USD", "USDT", 18);
        sp = new AtaraSpending();
        tok.mint(account, 100_000 ether);
        // 花钱要两个条件：策略允许 + 账户对本合约 approve 过
        vm.prank(account);
        tok.approve(address(sp), type(uint256).max);
    }

    function _grant() internal {
        vm.prank(account);
        sp.grant(ID, spender, address(tok), PER, CAP, WEEK, 0);
    }

    // ══════════ 正路 ══════════

    function test_GrantThenSpend() public {
        _grant();
        assertTrue(sp.isLive(ID));
        assertEq(sp.available(ID), PER, unicode"可花的是单笔上限，不是窗口总额");

        vm.prank(spender);
        sp.spend(ID, 100 ether, dest);
        assertEq(tok.balanceOf(dest), 100 ether);

        AtaraSpending.Policy memory p = sp.policyOf(ID);
        assertEq(p.used, 100 ether, unicode"窗口用量要累计");
    }

    /// 窗口余量不足时，available 给的是余量而不是单笔上限。
    function test_AvailableClampsToWindowRemainder() public {
        _grant();
        vm.startPrank(spender);
        for (uint256 i = 0; i < 3; i++) {
            sp.spend(ID, PER, dest); // 3 × 300 = 900
        }
        vm.stopPrank();
        assertEq(sp.available(ID), CAP - 900 ether, unicode"剩 300 正好等于单笔上限");

        vm.prank(spender);
        sp.spend(ID, 250 ether, dest); // 累计 1150
        assertEq(sp.available(ID), 50 ether, unicode"只剩 50 时给 50，不给 300");
    }

    /// 窗口滚动后用量归零。
    function test_WindowRollsOver() public {
        _grant();
        vm.startPrank(spender);
        sp.spend(ID, PER, dest);
        sp.spend(ID, PER, dest);
        sp.spend(ID, PER, dest);
        sp.spend(ID, PER, dest); // 用满 1200
        vm.stopPrank();
        assertEq(sp.available(ID), 0, unicode"窗口用满");

        vm.prank(spender);
        vm.expectRevert(AtaraSpending.OverWindow.selector);
        sp.spend(ID, 1 ether, dest);

        skip(WEEK);
        assertEq(sp.available(ID), PER, unicode"跨过周期，窗口重开");
        vm.prank(spender);
        sp.spend(ID, PER, dest);
        assertEq(tok.balanceOf(dest), 1500 ether);
    }

    /// 窗口边界必须稳定：按整数倍前推，不是每次花钱都把窗口往后推。
    /// 否则窗口永远不结束，等于没有周期限制。
    function test_WindowStartAdvancesInWholeCycles() public {
        _grant();
        uint64 start = sp.policyOf(ID).windowStart;

        skip(WEEK + 1 days); // 跨过 1 个整周期
        vm.prank(spender);
        sp.spend(ID, 10 ether, dest);
        assertEq(sp.policyOf(ID).windowStart, start + WEEK,
            unicode"前推一个整周期，不是设成当前时间");

        skip(3 * uint256(WEEK)); // 再跨 3 个
        vm.prank(spender);
        sp.spend(ID, 10 ether, dest);
        assertEq(sp.policyOf(ID).windowStart, start + 4 * WEEK);
    }

    function test_RevokeStopsSpending() public {
        _grant();
        vm.prank(account);
        sp.revoke(ID);
        assertFalse(sp.isLive(ID));
        assertEq(sp.available(ID), 0);

        vm.prank(spender);
        vm.expectRevert(AtaraSpending.NotLive.selector);
        sp.spend(ID, 1 ether, dest);
    }

    function test_ExpiryStopsSpending() public {
        vm.prank(account);
        sp.grant(ID, spender, address(tok), PER, CAP, WEEK, uint64(block.timestamp + 30 days));
        vm.prank(spender);
        sp.spend(ID, 10 ether, dest);

        skip(31 days);
        assertFalse(sp.isLive(ID));
        assertEq(sp.available(ID), 0);
        vm.prank(spender);
        vm.expectRevert(AtaraSpending.Expired.selector);
        sp.spend(ID, 1 ether, dest);
    }

    /// 改额度时窗口用量保留——否则「改一次额度」就成了绕过窗口的手段。
    function test_RegrantKeepsWindowUsage() public {
        _grant();
        vm.prank(spender);
        sp.spend(ID, PER, dest);
        assertEq(sp.policyOf(ID).used, PER);

        vm.prank(account);
        sp.grant(ID, spender, address(tok), 500 ether, 5000 ether, WEEK, 0);
        assertEq(sp.policyOf(ID).used, PER, unicode"调高上限不该清零已用");
        assertEq(sp.policyOf(ID).perPayment, 500 ether);
    }

    /// 换币种等于换一份策略，用量不该跟着过来。
    function test_RegrantWithNewTokenResetsUsage() public {
        _grant();
        vm.prank(spender);
        sp.spend(ID, PER, dest);

        TestStablecoin other = new TestStablecoin("Test USD Coin", "USDC", 18);
        vm.prank(account);
        sp.grant(ID, spender, address(other), PER, CAP, WEEK, 0);
        assertEq(sp.policyOf(ID).used, 0, unicode"换币种要重开窗口");
    }

    // ══════════ 授权边界 ══════════

    /// 只有出钱的账户能签发和修改。
    function test_RevertWhen_StrangerRegrants() public {
        _grant();
        vm.prank(stranger);
        vm.expectRevert(AtaraSpending.NotAccount.selector);
        sp.grant(ID, stranger, address(tok), PER, CAP, WEEK, 0);
    }

    /// spender 不能给自己提额——这是最该守住的一条。
    function test_RevertWhen_SpenderRaisesOwnLimit() public {
        _grant();
        vm.prank(spender);
        vm.expectRevert(AtaraSpending.NotAccount.selector);
        sp.grant(ID, spender, address(tok), 99_999 ether, 99_999 ether, WEEK, 0);
    }

    function test_RevertWhen_StrangerRevokes() public {
        _grant();
        vm.prank(stranger);
        vm.expectRevert(AtaraSpending.NotAccount.selector);
        sp.revoke(ID);
        assertTrue(sp.isLive(ID), unicode"策略还在");
    }

    /// spender 不能撤销——撤销是账户的权力，不是花钱的人的。
    function test_RevertWhen_SpenderRevokes() public {
        _grant();
        vm.prank(spender);
        vm.expectRevert(AtaraSpending.NotAccount.selector);
        sp.revoke(ID);
    }

    /// 只有被授权的 spender 能花。
    function test_RevertWhen_StrangerSpends() public {
        _grant();
        vm.prank(stranger);
        vm.expectRevert(AtaraSpending.NotSpender.selector);
        sp.spend(ID, 1 ether, dest);
    }

    /// 账户自己也不能借这条路花钱——它不是 spender。
    /// 账户要花自己的钱直接转就行，不必经过策略。
    function test_RevertWhen_AccountSpendsViaPolicy() public {
        _grant();
        vm.prank(account);
        vm.expectRevert(AtaraSpending.NotSpender.selector);
        sp.spend(ID, 1 ether, dest);
    }

    /// 换了 spender，旧的立刻失效。
    function test_RegrantChangesSpender() public {
        _grant();
        address newSpender = address(0xC0FFEE);
        vm.prank(account);
        sp.grant(ID, newSpender, address(tok), PER, CAP, WEEK, 0);

        vm.prank(spender);
        vm.expectRevert(AtaraSpending.NotSpender.selector);
        sp.spend(ID, 1 ether, dest);

        vm.prank(newSpender);
        sp.spend(ID, 1 ether, dest);
        assertEq(tok.balanceOf(dest), 1 ether);
    }

    // ══════════ 上限 ══════════

    function test_RevertWhen_OverPerPayment() public {
        _grant();
        vm.prank(spender);
        vm.expectRevert(AtaraSpending.OverPerPayment.selector);
        sp.spend(ID, PER + 1, dest);
    }

    function test_RevertWhen_OverWindow() public {
        _grant();
        vm.startPrank(spender);
        sp.spend(ID, PER, dest);
        sp.spend(ID, PER, dest);
        sp.spend(ID, PER, dest);
        sp.spend(ID, PER, dest);
        vm.expectRevert(AtaraSpending.OverWindow.selector);
        sp.spend(ID, 1 ether, dest);
        vm.stopPrank();
    }

    /// 单笔上限超过窗口总额是配置错误：那条单笔限制永远不起作用。
    /// 与后端的 CAP_ABOVE_WINDOW 对应。
    function test_RevertWhen_PerPaymentAboveWindowCap() public {
        vm.prank(account);
        vm.expectRevert(AtaraSpending.BadCaps.selector);
        sp.grant(ID, spender, address(tok), CAP + 1, CAP, WEEK, 0);
    }

    function test_RevertWhen_ZeroCaps() public {
        vm.startPrank(account);
        vm.expectRevert(AtaraSpending.ZeroAmount.selector);
        sp.grant(ID, spender, address(tok), 0, CAP, WEEK, 0);
        vm.expectRevert(AtaraSpending.ZeroAmount.selector);
        sp.grant(ID, spender, address(tok), PER, 0, WEEK, 0);
        vm.stopPrank();
    }

    function test_RevertWhen_ZeroCycle() public {
        vm.prank(account);
        vm.expectRevert(AtaraSpending.BadCycle.selector);
        sp.grant(ID, spender, address(tok), PER, CAP, 0, 0);
    }

    function test_RevertWhen_GrantAlreadyExpired() public {
        vm.prank(account);
        vm.expectRevert(AtaraSpending.Expired.selector);
        sp.grant(ID, spender, address(tok), PER, CAP, WEEK, uint64(block.timestamp));
    }

    function test_RevertWhen_ZeroAddresses() public {
        vm.startPrank(account);
        vm.expectRevert(AtaraSpending.ZeroAddress.selector);
        sp.grant(ID, address(0), address(tok), PER, CAP, WEEK, 0);
        vm.expectRevert(AtaraSpending.ZeroAddress.selector);
        sp.grant(ID, spender, address(0), PER, CAP, WEEK, 0);
        vm.stopPrank();
    }

    function test_RevertWhen_SpendZeroOrToZero() public {
        _grant();
        vm.startPrank(spender);
        vm.expectRevert(AtaraSpending.ZeroAmount.selector);
        sp.spend(ID, 0, dest);
        vm.expectRevert(AtaraSpending.ZeroAddress.selector);
        sp.spend(ID, 1 ether, address(0));
        vm.stopPrank();
    }

    function test_RevertWhen_NoPolicy() public {
        vm.prank(spender);
        vm.expectRevert(AtaraSpending.NoPolicy.selector);
        sp.spend(ID, 1 ether, dest);

        vm.prank(account);
        vm.expectRevert(AtaraSpending.NoPolicy.selector);
        sp.revoke(ID);
    }

    // ══════════ 双闸门 ══════════

    /// **第二道闸门**：策略允许也没用，账户没 approve 就是花不出去。
    /// 撤 approve 是账户随时能做的紧急刹车，不需要经过策略。
    function test_RevertWhen_ApprovalRevoked() public {
        _grant();
        vm.prank(account);
        tok.approve(address(sp), 0);

        vm.prank(spender);
        vm.expectRevert(); // 代币合约的 require("allowance")
        sp.spend(ID, 1 ether, dest);
        assertEq(tok.balanceOf(dest), 0);
    }

    /// approve 给的量不够时同样花不出去，且策略的用量不该被记上。
    function test_PartialApprovalBlocksAndDoesNotConsumeWindow() public {
        _grant();
        vm.prank(account);
        tok.approve(address(sp), 50 ether);

        vm.prank(spender);
        vm.expectRevert();
        sp.spend(ID, 100 ether, dest);
        assertEq(sp.policyOf(ID).used, 0, unicode"整笔回滚，用量不该留下");
    }

    /// 账户余额不足也一样——策略不保证钱在。
    function test_RevertWhen_AccountBalanceInsufficient() public {
        address poor = address(0xF00D);
        vm.prank(poor);
        tok.approve(address(sp), type(uint256).max);
        vm.prank(poor);
        sp.grant(ID, spender, address(tok), PER, CAP, WEEK, 0);

        vm.prank(spender);
        vm.expectRevert();
        sp.spend(ID, 10 ether, dest);
    }

    // ══════════ 不变量 ══════════

    /// 窗口内花出去的总量永远不超过 windowCap。
    function testFuzz_NeverExceedsWindowCap(uint96 a, uint96 b, uint96 c) public {
        _grant();
        uint256[3] memory amts = [uint256(a), uint256(b), uint256(c)];
        uint256 moved = 0;
        for (uint256 i = 0; i < 3; i++) {
            uint256 amt = amts[i] % (PER + 1);
            if (amt == 0) continue;
            vm.prank(spender);
            // 超窗口会 revert，那正是不变量在起作用
            try sp.spend(ID, amt, dest) {
                moved += amt;
            } catch {}
        }
        assertLe(moved, CAP, unicode"窗口内总量不超上限");
        assertEq(tok.balanceOf(dest), moved);
        assertEq(sp.policyOf(ID).used, moved);
    }

    /// 单笔永远不超 perPayment。
    function testFuzz_NeverExceedsPerPayment(uint96 amt) public {
        _grant();
        vm.assume(amt > 0);
        vm.prank(spender);
        if (uint256(amt) > PER) {
            vm.expectRevert(AtaraSpending.OverPerPayment.selector);
            sp.spend(ID, amt, dest);
            assertEq(tok.balanceOf(dest), 0);
        } else {
            sp.spend(ID, amt, dest);
            assertEq(tok.balanceOf(dest), amt);
        }
    }
}
