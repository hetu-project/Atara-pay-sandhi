package makerreview

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/advaita/atara-pay/internal/money"
)

// ── 挂单配置 ────────────────────────────────────────────────────────

// listing 是挂单配置那张表，字段与前端 MakerListing.tsx 的 Listing 一一对应。
type listing struct {
	Dir     []string `json:"dir"`
	Coins   []string `json:"coins"`
	Lo      string   `json:"lo"`
	Hi      string   `json:"hi"`
	Nets    []string `json:"nets"`
	Pricing string   `json:"pricing"`
	Spread  string   `json:"spread"`
	Fixed   string   `json:"fixed"`
	Rails   []string `json:"rails"`
	Agree   bool     `json:"agree"`
}

// CheckListing 审挂单配置。
//
// 这一段的判断几乎全是确定性的——币种在不在目录里、限额是不是一对合法的数、
// 定价有没有越界。全部留在规则层，一条都不交给模型。
func CheckListing(raw json.RawMessage) []Issue {
	var d listing
	if err := json.Unmarshal(raw, &d); err != nil {
		return []Issue{{
			Fields: []string{"*"},
			Says:   "We could not read the trading terms you submitted.",
			Ask:    "Please fill the form in again.",
			Route:  ToEscalate,
		}}
	}
	out := []Issue{}

	if len(d.Dir) == 0 {
		out = append(out, Issue{[]string{"dir"},
			"No trading direction is selected.", "Pick whether you buy, sell, or both.", ToRevise})
	}

	// 币种：表单给了四个选项，而这一版只结算 USDT / USDC。
	//
	// 选了 BTC/ETH 的配置即使放行，挂单那一步也会被 money.Tradable 挡回去
	// （offers.go: "this version settles USDT and USDC"）——审的时候不说，
	// 就是让人通过了准入再去撞一堵看不见的墙。
	switch {
	case len(d.Coins) == 0:
		out = append(out, Issue{[]string{"coins"},
			"No asset is selected.", "Pick at least one asset you want to trade.", ToRevise})
	default:
		bad := []string{}
		for _, c := range d.Coins {
			if !money.IsCrypto(c) || !money.Tradable(c) {
				bad = append(bad, c)
			}
		}
		if len(bad) == len(d.Coins) {
			out = append(out, Issue{[]string{"coins"},
				fmt.Sprintf("%s cannot be settled here — this version settles %s.",
					strings.Join(bad, " and "), tradableCoins()),
				fmt.Sprintf("Switch to %s.", tradableCoins()), ToRevise})
		} else if len(bad) > 0 {
			out = append(out, Issue{[]string{"coins"},
				fmt.Sprintf("%s cannot be settled here. The rest of what you picked is fine.",
					strings.Join(bad, " and ")),
				"Drop those, or submit with the others as they are.", ToRevise})
		}
	}

	// 限额：两个数、都为正、下限不高于上限。
	lo, okLo := num(d.Lo)
	hi, okHi := num(d.Hi)
	switch {
	case !okLo || !okHi || lo <= 0 || hi <= 0:
		out = append(out, Issue{[]string{"lo", "hi"},
			"The per-trade limits are incomplete or not valid amounts.",
			"Set both the lower and upper limit to a number above zero.", ToRevise})
	case lo > hi:
		out = append(out, Issue{[]string{"lo", "hi"},
			fmt.Sprintf("The lower limit %s is above the upper limit %s, so no trade can fall inside the range.",
				d.Lo, d.Hi),
			"Swap the two, or change one of them.", ToRevise})
	}

	if len(d.Nets) == 0 {
		out = append(out, Issue{[]string{"nets"},
			"No settlement network is selected.", "Pick at least one chain you can send and receive on.", ToRevise})
	}

	// 定价：两种模式各有各的合法区间，串着填会得到一个不成立的报价。
	switch d.Pricing {
	case "Fixed":
		if v, ok := num(d.Fixed); !ok || v <= 0 {
			out = append(out, Issue{[]string{"pricing", "fixed"},
				"Fixed pricing is selected but no valid price is set.", "Enter a price above zero.", ToRevise})
		}
	default: // Float
		if v, ok := num(strings.TrimSuffix(strings.TrimSpace(d.Spread), "%")); !ok || v < -5 || v > 5 {
			out = append(out, Issue{[]string{"pricing", "spread"},
				"A floating spread has to sit between -5% and +5%.", "Change it to a number inside that range.", ToRevise})
		}
	}

	if len(d.Rails) == 0 {
		out = append(out, Issue{[]string{"rails"},
			"No payment rail is selected, so the other side has nowhere to send the money.",
			"Pick at least one payment rail.", ToRevise})
	}

	if !d.Agree {
		out = append(out, Issue{[]string{"agree"},
			"The terms have not been confirmed.", "Read them through and tick the box.", ToRevise})
	}
	return out
}

func tradableCoins() string {
	names := []string{}
	for _, a := range money.Cryptos() {
		names = append(names, a.Code)
	}
	return strings.Join(names, " and ")
}

// ── 身份材料 ────────────────────────────────────────────────────────

/*
必填项只复校「影响风险判断」的那一部分，不是全部 25 项。

完整性校验留在前端（badKyc），后端这一道认的是另一件事：**不信前端**——
绕开界面直接打接口的人，这几项一样得有。挑出来的都是后面真的要用到的：
主体身份、税务辖区、财富来源、声明。地址、电话之类缺了不影响任何判断，
在这里拦一道只是重复劳动。
*/
var needIndividual = []struct{ key, label string }{
	{"nationality", "Nationality"}, {"surname", "Last name"}, {"firstname", "First name"},
	{"idtype", "ID type"}, {"idno", "ID number"},
	{"taxcountry", "Tax residency"}, {"sow", "Source of wealth"}, {"income", "Annual income"},
	{"pep", "Politically exposed person"}, {"sign", "Signature"},
}

var needCorporate = []struct{ key, label string }{
	{"company", "Company name"}, {"regno", "Registration no."}, {"regcountry", "Country of registration"},
	{"bizindustry", "Industry"}, {"turnover", "Annual turnover"},
	{"csow", "Source of funds"}, {"sanction", "Sanctions declaration"}, {"highrisk", "High-risk jurisdiction declaration"},
	{"ubo", "Beneficial owner"}, {"csign", "Signature"},
}

// CheckKYC 审身份材料里规则能判的那部分。
//
// 这里**不判**证件真伪、活体、人证比对——那是 IDAnalyzer 的活，已经在做，
// 而且是确定性的第三方核验。这一层只看自述字段。
func CheckKYC(raw json.RawMessage) []Issue {
	var f map[string]any
	if err := json.Unmarshal(raw, &f); err != nil {
		return []Issue{{
			Fields: []string{"*"},
			Says:   "We could not read the identity details you submitted.",
			Ask:    "Please fill the form in again.",
			Route:  ToEscalate,
		}}
	}
	corp := str(f["kind"]) == "Corporate"
	need := needIndividual
	if corp {
		need = needCorporate
	}

	out := []Issue{}
	miss, labels := []string{}, []string{}
	for _, n := range need {
		if !filled(f[n.key]) {
			miss = append(miss, n.key)
			labels = append(labels, n.label)
		}
	}
	if len(miss) > 0 {
		out = append(out, Issue{miss,
			"These are still blank: " + strings.Join(labels, ", ") + ".",
			"Fill them in and submit again.", ToRevise})
	}

	/*
		声明类：答「是」的不是错误，是**需要人看**。

		被列入制裁名单、身处高风险辖区、本人是政治公众人物——这三件申请人
		改不了，把它们写成「请修改」，等于让人对着一个他无论如何都满足不了
		的要求反复提交。所以一律转人工，由人决定收不收。

		PRD 的合规要求也在这儿：PEP 需要加强尽调（kycforms 那一步的原话
		就是 "Politically exposed persons require enhanced due diligence"），
		而「加强尽调」本身就意味着有人去做点什么。
	*/
	if corp {
		if str(f["sanction"]) == "Yes" {
			out = append(out, Issue{[]string{"sanction"},
				"You declared that the entity is subject to sanctions.",
				"A person will review this — there is nothing for you to change.", ToEscalate})
		}
		if str(f["highrisk"]) == "Yes" {
			out = append(out, Issue{[]string{"highrisk"},
				"You declared operations in a high-risk jurisdiction.",
				"A person will review this — there is nothing for you to change.", ToEscalate})
		}
		// 有 AML 政策不是必答项，但答了「没有」要人看一眼：
		// 一个做资金业务的主体自称没有反洗钱政策，是个该被读到的信号。
		if str(f["amlpolicy"]) == "No" {
			out = append(out, Issue{[]string{"amlpolicy"},
				"You declared that the entity has no internal AML policy.",
				"A person will review this — there is nothing for you to change.", ToEscalate})
		}
	} else if p := str(f["pep"]); p == "Yes" || p == "Close associate" {
		out = append(out, Issue{[]string{"pep"},
			"You declared that you are a politically exposed person or a close associate.",
			"This calls for enhanced due diligence — a person will follow up, and there is nothing for you to change.",
			ToEscalate})
	}

	// 美国税务居民要有 TIN——这一条是 CRS/FATCA 的硬要求，且申请人补得上。
	if !corp && str(f["ustax"]) == "Yes" && !filled(f["tin"]) {
		out = append(out, Issue{[]string{"ustax", "tin"},
			"You declared US tax residency but did not give a taxpayer identification number (TIN).",
			"Add your TIN.", ToRevise})
	}
	return out
}

// ── 小工具 ──────────────────────────────────────────────────────────

func str(v any) string {
	s, _ := v.(string)
	return s
}

// filled 说这一项是不是真的填了。
//
// 多选项存的是数组，空数组不算填过——「点开看了一眼」跟「真的选了」
// 是两回事（同一条规矩在 app.docsOf 里也写过一遍）。
func filled(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(t) != ""
	case []any:
		return len(t) > 0
	case bool:
		return t
	default:
		return true
	}
}

// num 容忍用户填的千分位和空格——「1,000」是个有效金额，不该被当成没填。
func num(s string) (float64, bool) {
	c := strings.Map(func(r rune) rune {
		if r == ',' || r == '，' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	if c == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(c, 64)
	return v, err == nil
}
