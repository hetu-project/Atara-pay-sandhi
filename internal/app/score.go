package app

import (
	"crypto/sha256"
	"encoding/binary"

	"github.com/advaita/atara-pay/internal/domain/model"
	"github.com/shopspring/decimal"
)

// 每笔工单的风控评分。
//
// ── 它是什么，以及不是什么 ──
//
// 这不是模型跑出来的。它是一个算术式：一半来自这笔单能查到的事实（对手方
// 的成交与纠纷记录、资质件、这一单的大小），一半来自工单号推出来的伪随机
// 抖动。演示要的是「每一单有一个看起来合理、彼此不同的分」，而真正的风控
// 模型还没有——把这件事写在这里，好过让下一个读代码的人以为背后有模型。
//
// ── 为什么要存下来 ──
//
// 算完存进工单，之后只读不重算。每次拉取现算的话，同一笔单在订单列表和
// 详情页会显示成两个数——哪怕算法是确定的，只要输入里有一个「对手方当前
// 纠纷数」这种会变的量，历史单的分就会跟着后来的事变，那就不是「当时的
// 判断」了。评分是对下单那一刻的判断，判断做完就固定。
//
// ── 取值范围 ──
//
// 60–99。下限 60 是产品要求：低于 60 在界面上是红的，等于劝退，而这一版
// 没有任何真实依据支撑「这一单危险」。上限 99 不给满分：100 分意味着
// 零风险，任何对手方风险模型都不该说得出这句话。
const (
	scoreMin = 60
	scoreMax = 99
)

// ScoreOrder 给一笔新工单算分。
//
// peer 是对手方的商户画像，可能为 nil（对方还没有成交记录）。
// amountUSD 是这一单的美元口径大小，用来给大额减一点分。
func ScoreOrder(orderID string, peer *model.Merchant, amountUSD decimal.Decimal) int {
	// 基础抖动：由工单号推出来，稳定且分散。用哈希不用 rand 是因为
	// 同一个工单号任何时候都要得到同一个分——补算历史单时才不会飘。
	h := sha256.Sum256([]byte("atara-score|" + orderID))
	base := scoreMin + int(binary.BigEndian.Uint32(h[:4])%20) // 60–79

	s := base
	if peer != nil {
		// 成交记录：攒得越多越稳，但封顶 +12——刷单不该能把分堆到满分。
		if peer.Deals > 0 {
			d := peer.Deals
			if d > 60 {
				d = 60
			}
			s += d / 5
		}
		// 纠纷比成交重：一次纠纷抵掉大约十笔顺利成交。
		s -= peer.Disputes * 3
		// 资质件是对手方交上来的凭据，一件 +2。
		for _, on := range peer.Docs {
			if on {
				s += 2
			}
		}
	}

	// 大额减分。金额本身不代表对方有问题，但一笔单越大，出事的代价越大，
	// 分数是给人做决定用的，得把这件事算进去。
	switch {
	case amountUSD.GreaterThan(decimal.NewFromInt(100000)):
		s -= 6
	case amountUSD.GreaterThan(decimal.NewFromInt(20000)):
		s -= 3
	}

	if s < scoreMin {
		s = scoreMin
	}
	if s > scoreMax {
		s = scoreMax
	}
	return s
}
