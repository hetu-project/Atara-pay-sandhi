package snowflake

import (
	"sync"
	"testing"
	"time"
)

// 唯一是这个包存在的全部理由——旧的随机号约 4800 单就有一半概率撞上。
func TestUniqueUnderConcurrency(t *testing.T) {
	n, err := New(7)
	if err != nil {
		t.Fatal(err)
	}
	const goroutines, each = 16, 2000
	out := make(chan int64, goroutines*each)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				out <- n.Next()
			}
		}()
	}
	wg.Wait()
	close(out)

	seen := make(map[int64]bool, goroutines*each)
	for id := range out {
		if seen[id] {
			t.Fatalf("duplicate id %d", id)
		}
		seen[id] = true
	}
	if len(seen) != goroutines*each {
		t.Fatalf("got %d ids, want %d", len(seen), goroutines*each)
	}
}

// 号要单调递增：这样按号排序就是按时间排序。
func TestMonotonic(t *testing.T) {
	n, _ := New(0)
	prev := n.Next()
	for i := 0; i < 50000; i++ {
		id := n.Next()
		if id <= prev {
			t.Fatalf("id went backwards: %d then %d", prev, id)
		}
		prev = id
	}
}

// 同一毫秒里发满 4096 个之后要跨到下一毫秒，而不是重复或回绕。
func TestSequenceOverflowRollsToNextMillis(t *testing.T) {
	n, _ := New(1)
	seen := map[int64]bool{}
	for i := 0; i < maxSeq+50; i++ {
		id := n.Next()
		if seen[id] {
			t.Fatalf("duplicate after %d ids", i)
		}
		seen[id] = true
	}
}

// 时钟回拨时不能发出更小的号。
func TestClockRollback(t *testing.T) {
	n, _ := New(3)
	first := n.Next()
	// 把内部时钟推到未来，再让真实时钟「落在后面」——等价于一次回拨。
	n.mu.Lock()
	n.last = time.Now().UnixMilli() + 500
	n.mu.Unlock()
	for i := 0; i < 100; i++ {
		id := n.Next()
		if id <= first {
			t.Fatalf("rollback produced a non-increasing id: %d after %d", id, first)
		}
		first = id
	}
}

// 节点号越界要报错，不能悄悄截断——截断等于两个实例发同一段号。
func TestNodeRange(t *testing.T) {
	if _, err := New(-1); err == nil {
		t.Fatal("negative node accepted")
	}
	if _, err := New(maxNode + 1); err == nil {
		t.Fatal("out-of-range node accepted")
	}
	if _, err := New(maxNode); err != nil {
		t.Fatalf("max node rejected: %v", err)
	}
}

// 编码要定长，而且字典序跟数值序一致——否则按号排序跟按时间排序对不上。
func TestEncodeSortsLikeTheNumber(t *testing.T) {
	n, _ := New(0)
	prev := Encode(n.Next())
	for i := 0; i < 5000; i++ {
		cur := Encode(n.Next())
		if len(cur) != encLen {
			t.Fatalf("encoded length %d, want %d", len(cur), encLen)
		}
		if cur <= prev {
			t.Fatalf("encoding not ordered: %q then %q", prev, cur)
		}
		prev = cur
	}
}

// 字母表里不能有 I / L / O / U —— 这个号要由人抄进银行附言。
func TestAlphabetAvoidsLookalikes(t *testing.T) {
	for _, c := range "ILOU" {
		for _, a := range crockford {
			if a == c {
				t.Fatalf("alphabet contains look-alike %q", c)
			}
		}
	}
}

// 号里带着时间，排查时不用查库。
func TestTimeOf(t *testing.T) {
	n, _ := New(0)
	before := time.Now().UTC().Add(-time.Second)
	id := n.Next()
	got := TimeOf(id)
	if got.Before(before) || got.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("TimeOf(%d) = %s, not near now", id, got)
	}
}
