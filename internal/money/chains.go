package money

// 这一版支持的链。
//
// 「网络」以前是几个标签（ETH / POLYGON / TRON），跟实际发交易的那条链没有
// 关系——挂单上写着 ETH，币却锁在 BSC 测试网的合约里。前端因此没法按挂单
// 决定钱包该切到哪儿，只能去问后端「你连的是哪条」，于是四条链只能有一条。
//
// 现在网络就是链本身：一个网络码对应一个 chainId，钱包该切到哪条由挂单
// 自己说清楚。
//
// 加一条链要做三件事，缺一条都是坏的：
//   1. 这里加一行
//   2. 把托管合约与代币部到那条链上（make chain-deploy）
//   3. 把地址记进 chain_deployments —— 没有这一步，那条链上挂不了卖单，
//      因为没有合约可以锁币。接口会照实说它没部署，而不是让人点完才发现。
type Chain struct {
	// Code 是网络码，挂单、订单、目录里用的就是它。
	Code string `json:"code"`
	Name string `json:"name"`
	// ChainID 是 EIP-155 的链号。钱包切链认的是它，不是名字。
	ChainID int64 `json:"chain_id"`
	// Testnet 上的币没有价值。界面要能把它跟主网分开说——
	// 不分的话，有人会以为自己在主网上真的锁了钱。
	Testnet  bool   `json:"testnet"`
	Explorer string `json:"explorer"`
	// Native 是这条链上付 gas 用的币。钱包添加链时要用。
	Native string `json:"native"`
}

var chains = []Chain{
	{Code: "BSC", Name: "BNB Smart Chain", ChainID: 56,
		Explorer: "https://bscscan.com", Native: "BNB"},
	{Code: "ETHEREUM", Name: "Ethereum", ChainID: 1,
		Explorer: "https://etherscan.io", Native: "ETH"},
	{Code: "BASE", Name: "Base", ChainID: 8453,
		Explorer: "https://basescan.org", Native: "ETH"},
	{Code: "BSC-TESTNET", Name: "BNB Smart Chain Testnet", ChainID: 97, Testnet: true,
		Explorer: "https://testnet.bscscan.com", Native: "tBNB"},
}

// Chains 返回这一版支持的全部链。
func Chains() []Chain { return append([]Chain(nil), chains...) }

// ChainOf 按网络码查。第二个返回值是「认不认识这个码」。
func ChainOf(code string) (Chain, bool) {
	for _, c := range chains {
		if eqFold(c.Code, code) {
			return c, true
		}
	}
	return Chain{}, false
}

// ChainByID 按链号查。钱包报的是链号，不是名字。
func ChainByID(id int64) (Chain, bool) {
	for _, c := range chains {
		if c.ChainID == id {
			return c, true
		}
	}
	return Chain{}, false
}

// KnownNetwork 说这个网络码是不是这一版认的链。
func KnownNetwork(code string) bool {
	_, ok := ChainOf(code)
	return ok
}

func eqFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
