package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/advaita/atara-pay/internal/domain/model"
)

// 一个对手方一条线程。聊天、订单卡、系统播报、评估结论共用同一条流——
// 消息归人，状态归事，但它们出现在同一个地方。

func (s *Store) Post(ctx context.Context, ownerID, peerID string, m *model.Message) error {
	if m.ID == "" {
		m.ID = NewID()
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = Now()
	}
	p, _ := json.Marshal(m.Payload)
	_, err := s.db.ExecContext(ctx,
		`insert into messages(id,owner_id,peer_id,author,kind,body,order_id,payload,created_at)
		 values(?,?,?,?,?,?,?,?,?)`,
		m.ID, ownerID, peerID, m.Author, m.Kind, m.Body, emptyToNull(m.OrderID), string(p), ts(m.CreatedAt))
	return err
}

func PostTx(tx *sql.Tx, ownerID, peerID string, m *model.Message) error {
	if m.ID == "" {
		m.ID = NewID()
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = Now()
	}
	p, _ := json.Marshal(m.Payload)
	_, err := tx.Exec(
		`insert into messages(id,owner_id,peer_id,author,kind,body,order_id,payload,created_at)
		 values(?,?,?,?,?,?,?,?,?)`,
		m.ID, ownerID, peerID, m.Author, m.Kind, m.Body, emptyToNull(m.OrderID), string(p), ts(m.CreatedAt))
	return err
}

func (s *Store) Thread(ctx context.Context, ownerID, peerID string) ([]model.Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`select id,peer_id,author,kind,body,order_id,payload,created_at
		   from messages where owner_id=? and peer_id=? order by created_at, id`, ownerID, peerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Message{}
	for rows.Next() {
		var m model.Message
		var oid sql.NullString
		var payload, created string
		if err := rows.Scan(&m.ID, &m.PeerID, &m.Author, &m.Kind, &m.Body, &oid, &payload, &created); err != nil {
			return nil, err
		}
		m.OrderID = nullStr(oid)
		m.CreatedAt = parseTS(created)
		m.Payload = map[string]string{}
		_ = json.Unmarshal([]byte(payload), &m.Payload)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ThreadSummary 是左栏那份列表：每个对手方一行，带最后一条与未读数。
type ThreadSummary struct {
	PeerID   string `json:"peer_id"`
	PeerName string `json:"peer_name"`
	Last     string `json:"last"`
	LastAt   string `json:"last_at"`
	Count    int    `json:"count"`
}

func (s *Store) Threads(ctx context.Context, ownerID string) ([]ThreadSummary, error) {
	rows, err := s.db.QueryContext(ctx,
		`select m.peer_id, u.display_name, count(*),
		        (select body from messages x where x.owner_id=m.owner_id and x.peer_id=m.peer_id
		          order by x.created_at desc, x.id desc limit 1),
		        max(m.created_at)
		   from messages m join users u on u.id=m.peer_id
		  where m.owner_id=? group by m.peer_id, u.display_name
		  order by max(m.created_at) desc`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ThreadSummary{}
	for rows.Next() {
		var t ThreadSummary
		if err := rows.Scan(&t.PeerID, &t.PeerName, &t.Count, &t.Last, &t.LastAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
