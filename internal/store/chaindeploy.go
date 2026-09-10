// 合约部署记录：这条链上的托管合约、支配权合约、代币合约分别在哪。
//
// 为什么落库而不是每次从环境变量现读：前端要自己发交易，得知道 approve 给谁、
// 锁进哪个合约。这份地址的权威在部署那一侧，读的人（前端、排查问题的人）
// 应该问同一个地方。环境变量只是部署输出的落点，进程一换就可能不一致；
// 库里那一行还带着 updated_at，能回答「这笔钱当时打进了哪个合约」。
package store

import (
	"context"
	"database/sql"
	"time"
)

type ChainToken struct {
	Address  string `json:"address"`
	Decimals int    `json:"decimals"`
}

type ChainDeployment struct {
	Network   string                `json:"network"`
	ChainID   int64                 `json:"chain_id"`
	Impl      string                `json:"impl"`
	RPCURL    string                `json:"rpc_url"`
	Explorer  string                `json:"explorer"`
	Escrow    string                `json:"escrow"`
	Spending  string                `json:"spending"`
	Tokens    map[string]ChainToken `json:"tokens"`
	UpdatedAt time.Time             `json:"updated_at"`
}

// SaveChainDeployment 记下这条链当前用的是哪几个合约。
//
// 代币先删后插，不是逐个 upsert：去掉一种币时，留着的旧行会让前端以为
// 那个币还能用，点下去才发现后端不认。
func (s *Store) SaveChainDeployment(ctx context.Context, d ChainDeployment) error {
	if d.Network == "" {
		d.Network = "unknown"
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		now := ts(Now())
		if _, err := tx.Exec(
			`insert into chain_deployments
			   (network,chain_id,impl,rpc_url,explorer,escrow_addr,spending_addr,updated_at)
			 values(?,?,?,?,?,?,?,?)
			 on conflict(network) do update set
			   chain_id=excluded.chain_id, impl=excluded.impl, rpc_url=excluded.rpc_url,
			   explorer=excluded.explorer, escrow_addr=excluded.escrow_addr,
			   spending_addr=excluded.spending_addr, updated_at=excluded.updated_at`,
			d.Network, d.ChainID, d.Impl, d.RPCURL, d.Explorer,
			d.Escrow, d.Spending, now); err != nil {
			return err
		}
		if _, err := tx.Exec(`delete from chain_tokens where network=?`, d.Network); err != nil {
			return err
		}
		for asset, t := range d.Tokens {
			if t.Address == "" {
				continue
			}
			if _, err := tx.Exec(
				`insert into chain_tokens(network,asset,address,decimals) values(?,?,?,?)`,
				d.Network, asset, t.Address, t.Decimals); err != nil {
				return err
			}
		}
		return nil
	})
}

// ChainDeployments 读所有链的部署记录，按网络码索引。
func (s *Store) ChainDeployments(ctx context.Context) (map[string]*ChainDeployment, error) {
	rows, err := s.db.QueryContext(ctx, `select network from chain_deployments`)
	if err != nil {
		return nil, err
	}
	var nets []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		nets = append(nets, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[string]*ChainDeployment{}
	for _, n := range nets {
		d, err := s.ChainDeployment(ctx, n)
		if err != nil {
			return nil, err
		}
		if d != nil {
			out[n] = d
		}
	}
	return out, nil
}

// ChainDeployment 读某条链的部署记录。查不到返回 nil，不是错误——
// mock 链本来就没有合约，那时「没有记录」就是正确答案。
func (s *Store) ChainDeployment(ctx context.Context, network string) (*ChainDeployment, error) {
	var d ChainDeployment
	var updated string
	err := s.db.QueryRowContext(ctx,
		`select network,chain_id,impl,rpc_url,explorer,escrow_addr,spending_addr,updated_at
		   from chain_deployments where network=?`, network).
		Scan(&d.Network, &d.ChainID, &d.Impl, &d.RPCURL, &d.Explorer,
			&d.Escrow, &d.Spending, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.UpdatedAt = parseTS(updated)
	d.Tokens = map[string]ChainToken{}
	rows, err := s.db.QueryContext(ctx,
		`select asset,address,decimals from chain_tokens where network=?`, network)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var asset string
		var t ChainToken
		if err := rows.Scan(&asset, &t.Address, &t.Decimals); err != nil {
			return nil, err
		}
		d.Tokens[asset] = t
	}
	return &d, rows.Err()
}
