// SPDX-License-Identifier: MIT
pragma solidity 0.8.24;

import {Script, console} from "forge-std/Script.sol";
import {AtaraEscrow} from "../src/AtaraEscrow.sol";
import {AtaraSpending} from "../src/AtaraSpending.sol";
import {TestStablecoin} from "../src/TestStablecoin.sol";

/**
 * @notice 部署脚本。
 *
 * Demo 配置：**单签名方、阈值 1**。签名方就是后端持有的那一把私钥。
 *
 * 这个配置下「共识决定放行」在链上退化成「后端决定放行」——后端那把私钥
 * 丢了，合约里的钱就能被放走。阈值机制本身还在，把签名方名单换成多个
 * 独立主机上的独立密钥、阈值调到 2 以上就恢复了，不需要改合约。
 * Demo 阶段这是需求方明确选的取舍。
 *
 * 环境变量：
 *   PRIVATE_KEY    部署者私钥（也是 Demo 里唯一的签名方）
 *   MIN_SCORE      放行所需最低共识评分，默认 70
 *   TOKEN_USDT     已有的 USDT 合约地址。留空则部一个测试币
 *   TOKEN_USDC     同上
 *
 * 每种币各自决定用现成的还是新部一个——链上已经有一个能用的币时，
 * 再部一个测试币只会让钱包里多一种看着一样、其实不通用的代币。
 * 全部留空就是「一条干净的测试链」，两个币都新部。
 */
contract Deploy is Script {
    function run() external {
        uint256 pk = vm.envUint("PRIVATE_KEY");
        address signer = vm.addr(pk);
        uint16 minScore = uint16(vm.envOr("MIN_SCORE", uint256(70)));
        address usdt = vm.envOr("TOKEN_USDT", address(0));
        address usdc = vm.envOr("TOKEN_USDC", address(0));

        vm.startBroadcast(pk);

        address[] memory signers = new address[](1);
        signers[0] = signer;
        AtaraEscrow escrow = new AtaraEscrow(signers, 1, minScore);

        // 支配权策略。签发额度的人就是出钱的人，所以这份合约不需要
        // 签名方与阈值——msg.sender 就是授权。
        AtaraSpending spending = new AtaraSpending();

        console.log("ATARA_ESCROW_ADDR=%s", address(escrow));
        console.log("ATARA_SPENDING_ADDR=%s", address(spending));
        console.log("ATARA_SIGNER_ADDR=%s", signer);
        console.log("MIN_SCORE=%s", minScore);

        // BSC 上 USDT(BSC-USD) 与 USDC 都是 18 位精度，测试币照抄
        if (usdt == address(0)) {
            TestStablecoin t = new TestStablecoin("Test BSC-USD", "USDT", 18);
            t.mint(signer, 1_000_000 ether); // 给部署者铸一些，方便端到端跑
            usdt = address(t);
        }
        if (usdc == address(0)) {
            TestStablecoin t = new TestStablecoin("Test USD Coin", "USDC", 18);
            t.mint(signer, 1_000_000 ether);
            usdc = address(t);
        }
        console.log("ATARA_TOKEN_USDT=%s", usdt);
        console.log("ATARA_TOKEN_USDC=%s", usdc);

        vm.stopBroadcast();
    }
}
