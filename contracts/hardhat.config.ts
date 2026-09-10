import { defineConfig } from "hardhat/config";

/**
 * 这份配置只管一件事：把 src/ 下的三份合约编出来。
 *
 * 没有 networks，也没有 accounts —— 部署走 scripts/deploy.ts，私钥从
 * 仓库根目录的 .env 读（.env 在 .gitignore 里）。**这个文件是提交进
 * git 的，而这两个仓库是公开的**，私钥写进来就等于公开了它。
 *
 * 版本必须是 0.8.24：三份合约的 pragma 都写死了这个版本，不是 ^0.8.24。
 */
export default defineConfig({
  // 合约在 src/，不是 Hardhat 默认的 contracts/。
  //
  // tests 也要挪开：test/ 和 script/ 下面是 Foundry 的 .sol，import 了
  // forge-std。Hardhat 默认把 test/ 当 Solidity 测试目录，会连着一起编，
  // 然后卡在「forge-std is not installed」——那些文件本来就不归它管。
  // 指一个不存在的目录，等于告诉它「这个项目没有 Hardhat 的合约测试」。
  paths: { sources: "src", tests: { solidity: "test-hardhat" } },
  solidity: {
    version: "0.8.24",
    settings: {
      optimizer: { enabled: true, runs: 200 },
    },
  },
});
