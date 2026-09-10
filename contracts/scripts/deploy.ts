/**
 * ══════════════════════════════════════════════════════════════════════
 *  把托管合约部到一条 EVM 链上
 * ══════════════════════════════════════════════════════════════════════
 *
 *  用法（在仓库根目录）：
 *      make chain-deploy                    演练：只检查，一笔交易都不发
 *      make chain-deploy A="send"           真的部署
 *      make chain-deploy A="send write"     部署完把地址写回 .env
 *
 *  参数全部来自仓库根目录的 .env（已在 .gitignore 里）：
 *
 *      ATARA_SIGNER_KEY   部署者私钥，同时是 Demo 里唯一的放行签名方
 *      ATARA_RPC_URL      节点
 *      ATARA_TOKEN_USDT   已有的 USDT 合约。留空则部一个测试币
 *      ATARA_TOKEN_USDC   同上
 *      MIN_SCORE          放行所需最低共识评分，默认 70
 *
 *  为什么默认是演练：这个脚本会往链上发不可撤销的交易、花掉真的 gas。
 *  默认发交易意味着敲错一次就多一份没人管的合约。
 */
import { readFileSync, writeFileSync, existsSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import {
  createPublicClient, createWalletClient, http, defineChain,
  formatEther, formatUnits, getAddress, parseUnits,
  type Hex, type Address,
} from "viem";
import { privateKeyToAccount } from "viem/accounts";

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(HERE, "../..");          // atara-pay/
const ENV_PATH = resolve(ROOT, ".env");

const SEND = process.argv.includes("send");
const WRITE = process.argv.includes("write");

const die = (m: string): never => { console.error(`\x1b[31m×\x1b[0m ${m}`); process.exit(1); };
const ok = (m: string) => console.log(`\x1b[32m✓\x1b[0m ${m}`);
const step = (m: string) => console.log(`\n\x1b[1m${m}\x1b[0m`);

// ── .env ────────────────────────────────────────────────────────────
// 自己解析，不引 dotenv：这里只需要「一行一个 KEY=VALUE」，
// 多一个依赖不如多这十行。
function readEnv(): Record<string, string> {
  if (!existsSync(ENV_PATH)) die(`没有 ${ENV_PATH}。先 cp .env.example .env 再填`);
  const out: Record<string, string> = {};
  for (const line of readFileSync(ENV_PATH, "utf8").split("\n")) {
    const m = /^\s*([A-Z_][A-Z0-9_]*)\s*=\s*(.*)$/.exec(line);
    if (!m) continue;
    out[m[1]] = m[2].trim().replace(/^["']|["']$/g, "");
  }
  return out;
}
const env = readEnv();
const cfg = (k: string, dflt = "") => (process.env[k] ?? env[k] ?? dflt).trim();

// ── 编译产物 ─────────────────────────────────────────────────────────
function artifact(file: string, name: string) {
  const p = resolve(HERE, `../artifacts/src/${file}/${name}.json`);
  if (!existsSync(p)) die(`没有编译产物 ${p}\n  先跑：cd contracts && npx hardhat compile`);
  const a = JSON.parse(readFileSync(p, "utf8"));
  return { abi: a.abi, bytecode: a.bytecode as Hex };
}

// ── 起步 ────────────────────────────────────────────────────────────
const rawKey = cfg("ATARA_SIGNER_KEY");
if (!rawKey) die("在 .env 里设 ATARA_SIGNER_KEY（部署者私钥）");
const key = (rawKey.startsWith("0x") ? rawKey : `0x${rawKey}`) as Hex;
if (!/^0x[0-9a-fA-F]{64}$/.test(key)) die("私钥应该是 64 个十六进制字符");

const rpc = cfg("ATARA_RPC_URL");
if (!rpc) die("在 .env 里设 ATARA_RPC_URL");

const account = privateKeyToAccount(key);

step("① 节点与部署者");
const probe = createPublicClient({ transport: http(rpc) });
let chainId: number;
try {
  chainId = await probe.getChainId();
} catch (e) {
  die(`连不上 ${rpc}\n  ${e instanceof Error ? e.message : String(e)}`);
}
// 链的元信息自己拼：不从 viem/chains 里挑，那份表里没有的链就没法部署，
// 而 chainId 是从节点问来的，本来就比表里写死的更可信。
const chain = defineChain({
  id: chainId!,
  name: cfg("ATARA_NETWORK", `chain-${chainId!}`),
  nativeCurrency: { name: "native", symbol: "ETH", decimals: 18 },
  rpcUrls: { default: { http: [rpc] } },
});
const pub = createPublicClient({ chain, transport: http(rpc) });
const wallet = createWalletClient({ account, chain, transport: http(rpc) });

const bal = await pub.getBalance({ address: account.address });
ok(`节点 ${rpc} · chainId ${chainId!}`);
ok(`部署者 ${account.address} · 余额 ${formatEther(bal)}`);
if (bal === 0n) die("余额是 0，付不起 gas。先去水龙头领测试币");

// ── 现成的代币先验一眼 ───────────────────────────────────────────────
// 地址填错照样能部署成功，等到挂单锁币那一刻才炸，那时错误信息只会说
// call reverted，查起来毫无线索。
const ERC20 = [
  { name: "decimals", type: "function", stateMutability: "view", inputs: [], outputs: [{ type: "uint8" }] },
  { name: "symbol", type: "function", stateMutability: "view", inputs: [], outputs: [{ type: "string" }] },
] as const;

step("② 代币");
const tokens: Record<string, Address | ""> = {};
for (const sym of ["USDT", "USDC"] as const) {
  const raw = cfg(`ATARA_TOKEN_${sym}`);
  if (!raw) { console.log(`  ${sym} 没给地址 → 这次部一个测试币`); tokens[sym] = ""; continue; }
  let addr!: Address;
  try { addr = getAddress(raw); } catch { die(`${sym} 的地址不合法：${raw}`); }
  const code = await pub.getCode({ address: addr });
  if (!code || code === "0x") die(`${sym} 的地址 ${addr} 上没有合约（chainId ${chainId!}）`);
  let dec!: number, s!: string;
  try {
    dec = await pub.readContract({ address: addr, abi: ERC20, functionName: "decimals" });
    s = await pub.readContract({ address: addr, abi: ERC20, functionName: "symbol" });
  } catch {
    die(`${sym} 的地址 ${addr} 不像个 ERC-20（读不到 decimals / symbol）`);
  }
  ok(`${sym} 用现成的 ${addr} · symbol ${s} · ${dec} 位精度`);
  tokens[sym] = addr;
}

const minScore = Number(cfg("MIN_SCORE", "70"));

step("③ 要部署的东西");
console.log(`  AtaraEscrow    签名方 [${account.address}] · 阈值 1 · minScore ${minScore}`);
console.log(`  AtaraSpending  无构造参数`);
for (const sym of ["USDT", "USDC"] as const) {
  if (!tokens[sym]) console.log(`  TestStablecoin ${sym} · 18 位 · 给部署者铸 100 万`);
}

if (!SEND) {
  console.log(`
\x1b[33m演练结束，一笔交易都没发。\x1b[0m
真的部署：  make chain-deploy A="send"
顺便回填：  make chain-deploy A="send write"   （地址直接写进 .env）`);
  process.exit(0);
}

// ── 部署 ────────────────────────────────────────────────────────────
async function deploy(file: string, name: string, args: unknown[]): Promise<Address> {
  const { abi, bytecode } = artifact(file, name);
  const hash = await wallet.deployContract({ abi, bytecode, args, chain, account });
  const rc = await pub.waitForTransactionReceipt({ hash });
  if (rc.status !== "success" || !rc.contractAddress) die(`${name} 部署失败：${hash}`);
  ok(`${name.padEnd(14)} ${rc.contractAddress}  gas ${rc.gasUsed}`);
  return getAddress(rc.contractAddress!);
}

step("④ 部署中");
const escrow = await deploy("AtaraEscrow.sol", "AtaraEscrow",
  [[account.address], 1, minScore]);
const spending = await deploy("AtaraSpending.sol", "AtaraSpending", []);

for (const sym of ["USDT", "USDC"] as const) {
  if (tokens[sym]) continue;
  const label = sym === "USDT" ? "Test BSC-USD" : "Test USD Coin";
  const t = await deploy("TestStablecoin.sol", "TestStablecoin", [label, sym, 18]);
  // 给部署者铸一些，方便端到端跑
  const { abi } = artifact("TestStablecoin.sol", "TestStablecoin");
  const h = await wallet.writeContract({
    address: t, abi, functionName: "mint",
    args: [account.address, parseUnits("1000000", 18)], chain, account,
  });
  await pub.waitForTransactionReceipt({ hash: h });
  ok(`  └─ 给部署者铸了 ${formatUnits(parseUnits("1000000", 18), 18)} ${sym}`);
  tokens[sym] = t;
}

// ── 回填 ────────────────────────────────────────────────────────────
const lines: Record<string, string> = {
  ATARA_CHAIN_IMPL: "evm",
  ATARA_RPC_URL: rpc,
  ATARA_NETWORK: cfg("ATARA_NETWORK", `chain-${chainId!}`),
  ATARA_EXPLORER: cfg("ATARA_EXPLORER", ""),
  ATARA_ESCROW_ADDR: escrow,
  ATARA_SPENDING_ADDR: spending,
  ATARA_TOKEN_USDT: tokens.USDT as string,
  ATARA_TOKEN_USDC: tokens.USDC as string,
};

step("⑤ 填回 .env");
console.log("────────────────────────────────────────────");
for (const [k, v] of Object.entries(lines)) console.log(`${k}=${v}`);
console.log("────────────────────────────────────────────");

if (WRITE) {
  // 逐行改，不重写整个文件：.env 里的注释和私钥都要原样留着。
  let src = readFileSync(ENV_PATH, "utf8");
  for (const [k, v] of Object.entries(lines)) {
    const re = new RegExp(`^${k}=.*$`, "m");
    src = re.test(src) ? src.replace(re, `${k}=${v}`) : `${src.trimEnd()}\n${k}=${v}\n`;
  }
  writeFileSync(ENV_PATH, src);
  ok(`已写回 ${ENV_PATH}`);
} else {
  console.log("（没加 write，上面这些要自己填进 .env）");
}

const ex = cfg("ATARA_EXPLORER");
if (ex) console.log(`\n浏览器：${ex}/address/${escrow}`);
console.log(`\n然后 \x1b[1mmake fresh-chain\x1b[0m 重起后端。`);
console.log(`合约地址会从 GET /api/v1/catalog/chain 发给前端。`);
