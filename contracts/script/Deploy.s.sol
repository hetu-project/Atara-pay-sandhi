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
 *   DEPLOY_TOKENS  是否一并部两个测试稳定币，默认 true（测试链用）
 */
contract Deploy is Script {
    function run() external {
        uint256 pk = vm.envUint("PRIVATE_KEY");
        address signer = vm.addr(pk);
        uint16 minScore = uint16(vm.envOr("MIN_SCORE", uint256(70)));
        bool deployTokens = vm.envOr("DEPLOY_TOKENS", true);

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

        if (deployTokens) {
            // BSC 上 USDT(BSC-USD) 与 USDC 都是 18 位精度，测试币照抄
            TestStablecoin usdt = new TestStablecoin("Test BSC-USD", "USDT", 18);
            TestStablecoin usdc = new TestStablecoin("Test USD Coin", "USDC", 18);
            // 给部署者铸一些，方便端到端跑
            usdt.mint(signer, 1_000_000 ether);
            usdc.mint(signer, 1_000_000 ether);
            console.log("ATARA_TOKEN_USDT=%s", address(usdt));
            console.log("ATARA_TOKEN_USDC=%s", address(usdc));
        }

        vm.stopBroadcast();
    }
}
