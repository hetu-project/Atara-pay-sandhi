package app

import (
	"context"
	"net/http"
	"strings"

	"github.com/advaita/atara-pay/internal/auth"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/money"
	"github.com/advaita/atara-pay/internal/store"
	"github.com/shopspring/decimal"
)

type AddPayeeReq struct {
	Label   string `json:"label"`
	Chain   string `json:"chain"`
	Address string `json:"address"`
}

func (s *Service) AddPayee(ctx context.Context, ownerID string, req AddPayeeReq) (*store.Payee, error) {
	req.Label, req.Chain, req.Address = strings.TrimSpace(req.Label),
		strings.TrimSpace(req.Chain), strings.TrimSpace(req.Address)
	if req.Address == "" {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "ADDRESS_REQUIRED", "address",
			"paste the address you want to withdraw to")
	}
	if req.Chain == "" {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "CHAIN_REQUIRED", "chain",
			"pick the network — the same string of characters is a different account on another chain")
	}
	if req.Label == "" {
		req.Label = req.Address
	}
	p := store.Payee{ID: store.NewID(), OwnerID: ownerID, Label: req.Label,
		Chain: req.Chain, Address: req.Address, CreatedAt: store.Now()}
	if err := s.St.AddPayee(ctx, p); err != nil {
		// 唯一约束是去重的执行点，不在 Go 里查后写。
		return nil, httpx.Fail(http.StatusConflict, "PAYEE_EXISTS", "address",
			"that address is already in your address book for this chain")
	}
	return &p, nil
}

// WithdrawReq 是前端四步提现一次性提交的内容：
// 地址（收款方）→ 金额 → 用途 → 凭证。
type WithdrawReq struct {
	PayeeID     string `json:"payee_id"`
	Asset       string `json:"asset"`
	Amount      string `json:"amount"`
	Purpose     string `json:"purpose"`
	DocUploadID string `json:"doc_upload_id"`
}

// CreateWithdrawal 记下一次提现意图。
//
// 非托管下链上转账由用户自己签，平台不代持也不代发；这里存的是用途与凭证
// 这类合规材料，加一个待回填的 tx_hash。但「动钱必确认」照旧适用——
// 即便平台不动手，这一步仍要签名档的令牌，不是承诺档。
func (s *Service) CreateWithdrawal(ctx context.Context, ownerID, confirmToken string,
	req WithdrawReq) (*store.Withdrawal, error) {
	if req.PayeeID == "" {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "PAYEE_REQUIRED", "payee_id",
			"pick who you are withdrawing to")
	}
	payee, ok := s.St.Payee(ctx, ownerID, req.PayeeID)
	if !ok {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "PAYEE_REQUIRED", "payee_id",
			"no such payee in your address book")
	}
	if req.Asset == "" {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "ASSET_REQUIRED", "asset",
			"pick the asset to withdraw")
	}
	// 只能提数字资产。法币不入账——钱包里从来没有法币行，法币腿点对点走银行。
	if !money.IsCrypto(req.Asset) {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "ASSET_REQUIRED", "asset",
			req.Asset+" is not a digital asset — fiat never sits in the wallet")
	}
	amt, err := decimal.NewFromString(req.Amount)
	if err != nil {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "AMOUNT_REQUIRED", "amount",
			"amount must be a decimal number")
	}
	if amt.LessThanOrEqual(decimal.Zero) {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "AMOUNT_INVALID", "amount",
			"amount must be greater than zero")
	}
	if strings.TrimSpace(req.Purpose) == "" {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "PURPOSE_REQUIRED", "purpose",
			"say what this payment is for — the receiving bank will ask")
	}
	if err := s.Confirm.Consume(ctx, confirmToken, ownerID,
		Digest("withdraw", req.PayeeID, req.Asset, amt.String()), auth.GradeSignature); err != nil {
		return nil, err
	}
	w := store.Withdrawal{ID: store.NewID(), OwnerID: ownerID, PayeeID: req.PayeeID,
		Asset: req.Asset, Amount: amt, Purpose: req.Purpose, DocUploadID: req.DocUploadID,
		State: "submitted", CreatedAt: store.Now(), UpdatedAt: store.Now(),
		PayeeLabel: payee.Label, PayeeChain: payee.Chain, PayeeAddress: payee.Address}
	if err := s.St.InsertWithdrawal(ctx, w); err != nil {
		return nil, err
	}
	return &w, nil
}

// BroadcastWithdrawal 回填用户自己签出来的那笔链上转账。
//
// 非托管下平台不代发，所以这一步是「我签完了，哈希在这」——协议记下它，
// 提现才从 submitted 走到 broadcast。没有这一步，提现会永久停在 submitted。
func (s *Service) BroadcastWithdrawal(ctx context.Context, ownerID, id, txHash string) (*store.Withdrawal, error) {
	if strings.TrimSpace(txHash) == "" {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "TX_REQUIRED", "tx_hash",
			"paste the transaction hash you signed — the protocol does not broadcast for you")
	}
	ws, err := s.St.Withdrawals(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	var found *store.Withdrawal
	for i := range ws {
		if ws[i].ID == id {
			found = &ws[i]
			break
		}
	}
	if found == nil {
		return nil, httpx.NotFound("withdrawal")
	}
	if found.State != "submitted" {
		return nil, httpx.Fail(http.StatusConflict, "NOT_SUBMITTED", "",
			"this withdrawal is at "+found.State+", not waiting on a transaction")
	}
	if err := s.St.SetWithdrawalTx(ctx, ownerID, id, txHash, "broadcast"); err != nil {
		return nil, err
	}
	found.TxHash, found.State = txHash, "broadcast"
	return found, nil
}
