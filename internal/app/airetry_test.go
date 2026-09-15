package app

import (
	"strings"
	"testing"
	"time"
)

// 退避要真的在退：连着失败还按 30 秒重来，等于把一个挂掉的上游按在地上捶。
func TestAIBackoffGrows(t *testing.T) {
	prev := time.Duration(0)
	for n := 0; n < 5; n++ {
		d := aiBackoff(n)
		if d <= prev {
			t.Fatalf("第 %d 次退避 %v 没有比上一次 %v 更久", n, d, prev)
		}
		prev = d
	}
}

// 但不能一路涨下去：涨到几小时，一次抖动就把人晾一上午。
func TestAIBackoffIsCapped(t *testing.T) {
	if d := aiBackoff(50); d > 8*time.Minute {
		t.Fatalf("退避封不住顶: %v", d)
	}
}

// 重试次数是有限的。
//
// 到了上限它已经不是抖动，是真出事了——密钥失效、额度耗尽、上游改了格式。
// 那时候该有人知道，而不是让一队申请人在「审核中」里排到天亮。
func TestAIRetriesAreBounded(t *testing.T) {
	if aiMaxAttempts <= 1 {
		t.Fatalf("至少要重试一次，否则一次抖动就把人转去人工队列")
	}
	if aiMaxAttempts > 10 {
		t.Fatalf("重试 %d 次太多：上游真挂了的时候没人会知道", aiMaxAttempts)
	}
	// 总等待时间要在一个人还愿意等的量级内。
	total := time.Duration(0)
	for n := 0; n < aiMaxAttempts; n++ {
		total += aiBackoff(n)
	}
	if total > 30*time.Minute {
		t.Fatalf("试满一轮要等 %v，太久了", total)
	}
}

// 合起来存的那份要能按段取出来——重试时手上只有库里那一份。
func TestStageFormPicksTheRightHalf(t *testing.T) {
	raw := `{"kyc":{"kind":"Individual"},"listing":{"coins":["USDT"]}}`
	if got := string(stageForm(raw, "kyc")); got != `{"kind":"Individual"}` {
		t.Fatalf("kyc = %s", got)
	}
	if got := string(stageForm(raw, "listing")); got != `{"coins":["USDT"]}` {
		t.Fatalf("listing = %s", got)
	}
	// 取不到就是取不到，不拿另一段凑数。
	if stageForm(raw, "offer") != nil {
		t.Fatalf("不存在的段应当返回空")
	}
	if stageForm("not json", "kyc") != nil {
		t.Fatalf("读不出的表单应当返回空")
	}
}

// 试满一轮之后写给申请人的那句话，必须说明「不用你改」。
//
// 这是行为约束，不是措辞偏好：技术故障和材料问题在申请人那里是两件完全
// 不同的事。混成一句，他会回表单里去改一个没有错的地方，改完再交，
// 再被同一个故障挡回来。
func TestGaveUpMessageDoesNotBlameTheForm(t *testing.T) {
	if !strings.Contains(aiGaveUpMessage, "nothing for you to change") {
		t.Fatalf("转人工的措辞要说明不用他改: %q", aiGaveUpMessage)
	}
	for _, bad := range []string{"invalid", "incorrect", "error in your"} {
		if strings.Contains(strings.ToLower(aiGaveUpMessage), bad) {
			t.Fatalf("不该暗示是他的材料有问题: %q", aiGaveUpMessage)
		}
	}
}
