// Package snowflake 生成工单号。
//
// 原来是 6 位随机十六进制（ATR-8220C4）：1670 万种取值，而 orders.ref 是
// unique 约束——按生日悖论，大约 4800 单就有一半概率撞上，撞上就是一次
// 插入失败，用户看到的是「下单失败」，而重试还可能再撞。
//
// 雪花号的三段各自解决一件事：
//
//	41 位毫秒时间戳  号按时间递增，翻页和排序不用另外读一列，
//	                 而且从号本身就能看出这单是什么时候下的
//	10 位节点号      多个后端实例同时发号也不会撞——不用任何协调
//	12 位序列号      同一毫秒内最多 4096 个，够任何单机吞吐
//
// 时间戳 41 位从 2024 年起算，能用到 2093 年。
package snowflake

import (
	"fmt"
	"sync"
	"time"
)

const (
	// epochMs 是 2024-01-01T00:00:00Z。自定义纪元而不是 Unix 纪元：
	// 从 1970 起算的话 41 位在 2039 年就用完了。
	epochMs = 1704067200000

	nodeBits = 10
	seqBits  = 12

	maxNode = -1 ^ (-1 << nodeBits) // 1023
	maxSeq  = -1 ^ (-1 << seqBits)  // 4095
)

type Node struct {
	mu   sync.Mutex
	node int64
	last int64 // 上一次发号的毫秒
	seq  int64
}

// New 建一个发号器。node 必须在同一部署里唯一——两个实例用同一个号会发出
// 重复的 id，而这正是雪花要解决的问题。
func New(node int64) (*Node, error) {
	if node < 0 || node > maxNode {
		return nil, fmt.Errorf("node id %d out of range 0..%d", node, maxNode)
	}
	return &Node{node: node}, nil
}

// Next 发一个号。并发安全。
func (n *Node) Next() int64 {
	n.mu.Lock()
	defer n.mu.Unlock()

	now := time.Now().UnixMilli()
	// 时钟回拨（NTP 校时、虚拟机迁移）时不能按当前时间发号——那会发出比
	// 已经发过的更小的号，甚至重复。停在上一次的毫秒上继续用序列号往前走：
	// 号仍然唯一、仍然不减，代价是这段时间里的号「看起来」都在同一毫秒。
	if now < n.last {
		now = n.last
	}
	if now == n.last {
		n.seq = (n.seq + 1) & maxSeq
		if n.seq == 0 {
			// 这一毫秒的 4096 个用完了，等下一毫秒。
			for now <= n.last {
				now = time.Now().UnixMilli()
			}
		}
	} else {
		n.seq = 0
	}
	n.last = now
	return ((now - epochMs) << (nodeBits + seqBits)) | (n.node << seqBits) | n.seq
}

// TimeOf 从号里读出它是什么时候发的。排查问题时不用去查库。
func TimeOf(id int64) time.Time {
	return time.UnixMilli((id >> (nodeBits + seqBits)) + epochMs).UTC()
}

// crockford 是 Crockford base32 的字母表：去掉了 I / L / O / U——
// I 和 1、O 和 0 手抄时会认错，而这个号是要打进银行转账附言的。
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// encLen 是 64 位定长编码的字符数。定长是有意的：变长的话短号排在长号
// 前面，按字符串排序就跟按时间排序对不上了。
const encLen = 13

// Encode 把号编成 13 位 base32。
//
// 为什么不用十进制：雪花号十进制是 19 位数字，而这个号要由人抄进银行转账
// 的附言里。base32 同样的信息只要 13 位，还避开了容易认错的字符。
func Encode(id int64) string {
	b := make([]byte, encLen)
	v := uint64(id)
	for i := encLen - 1; i >= 0; i-- {
		b[i] = crockford[v&31]
		v >>= 5
	}
	return string(b)
}
