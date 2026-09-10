package store

import "testing"

// 占位符必须跟列数一致。这条测试存在的理由：手写 ?,?,?… 跟列表错开时，
// 编译和大部分测试都过得去，直到某次插入才炸「N values for M columns」。
func TestPlaceholdersMatchColumns(t *testing.T) {
	for _, cols := range []string{orderCols, userCols, allowCols, makerCols} {
		want := 1
		for _, c := range cols {
			if c == ',' {
				want++
			}
		}
		got := 1
		for _, c := range placeholders(cols) {
			if c == ',' {
				got++
			}
		}
		if got != want {
			t.Fatalf("placeholders gave %d for %d columns: %s", got, want, cols)
		}
	}
}
