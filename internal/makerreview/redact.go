package makerreview

import (
	"encoding/json"
	"sort"
)

/*
出网白名单——这个文件是整个模型层的闸门。

申请表里有证件号、住址、电话、邮箱、税号、签名。模型跑在境外第三方 API 上
（DeepSeek），发出去的东西收不回来。所以这里反过来做：**不是「剔掉敏感的」，
是「只放行列出来的」**。漏写一条黑名单会把身份证号发出去；漏写一条白名单
只是少一项判断依据。两种漏写的代价不对称，所以用白名单。

挑选的标准是「模型判那四类问题真的用得上吗」（MAKER-REVIEW-AI.md §2.1）：
辖区一致性、规模合理性、声明矛盾、资质件门槛。按这个标准筛下来，**没有一个
身份标识是必需的**——判断「注册地香港 / 经营地深圳 / 只收 CNY 对不对得上」
不需要知道这家公司叫什么，更不需要知道法人的护照号。

所以下面两张表里没有：姓名、公司名、证件号、注册号、住址街道邮编、电话、
邮箱、税号、出生日期、签名、董事与受益人的任何身份信息。

要加字段就在这里加，加之前先回答一句：模型少了它，具体判不了哪一条？
*/

// outboundKYC 是身份材料里允许出网的字段。
var outboundKYC = []string{
	// 主体类型：个人还是公司，两条路的判断完全不同
	"kind",

	// ── 个人 ──
	"nationality", // 辖区一致性
	"taxcountry",  // 同上；与 nationality 不一致本身是个该被读到的信号
	"empstatus",   // 职业状态 ↔ 财富来源 ↔ 申报规模
	"industry",    // 自由文本，规则层判不了
	"sow",         // 财富来源（选项数组）
	"income",      // 年收入档位，不是具体数字

	// ── 企业 ──
	"regcountry",  // 注册地
	"city",        // 经营地。街道和邮编不发——判辖区一致性用不上门牌号
	"province",    //
	"bizindustry", // 行业，自由文本
	"bizscope",    // 业务描述，自由文本。模型层存在的主要理由就是读它
	"turnover",    // 营业额档位
	"headcount",   // 人数档位
	"csow",        // 资金来源（选项数组）
	"mainrev",     // 主要收入来源，自由文本
	"amlpolicy",   // 声明类，Yes/No，不含身份信息
	"sanction",    //
	"highrisk",    //
}

/*
The listing stage has no whitelist because it never reaches the model.

Every field on that form is structured — sides, assets, two numbers, networks,
a spread, rail names. There is no free text for a model to read, and the rules
already decide all of it: an asset outside the catalogue, a lower limit above
the upper one, a spread outside the band, a rail we cannot settle. Sending it
anyway would cost a call and three seconds of the applicant's time to be told
what the rule layer already said, for nothing.

If free text ever appears there, add a whitelist here and lift the stage check
in AI.Review — not the other way round.
*/

// Redact 按白名单挑出能出网的字段。
//
// 白名单之外的一律不出现在返回值里——不是置空，是根本不存在这个键。
// 置空会让模型看见「这里有个东西被藏起来了」，进而开始猜。
func Redact(raw json.RawMessage, allow []string) (json.RawMessage, error) {
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	ok := map[string]bool{}
	for _, k := range allow {
		ok[k] = true
	}
	out := map[string]any{}
	for k, v := range in {
		if ok[k] {
			out[k] = v
		}
	}
	return json.Marshal(out)
}

// RedactKYC keeps only the identity fields cleared to leave the network.
func RedactKYC(raw json.RawMessage) (json.RawMessage, error) {
	return Redact(raw, outboundKYC)
}

// Outbound lists, sorted, every field that can leave the network.
//
// Used in two places: the test that pins the list (so adding or removing a
// field has to be a deliberate edit), and anywhere the list needs showing to
// a person — what the model can see should not be knowable only by reading
// the source.
func Outbound() []string {
	out := append([]string(nil), outboundKYC...)
	sort.Strings(out)
	return out
}
