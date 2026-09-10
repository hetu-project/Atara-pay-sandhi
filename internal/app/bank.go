package app

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/store"
)

// BankReq 是一张法币收款账户的内容。AccountNo 收的是全量号码——
// 校验完就掩码落库，全量不进数据库。
type BankReq struct {
	Holder    string `json:"holder"`
	Bank      string `json:"bank"`
	AccountNo string `json:"account_no"`
	Currency  string `json:"currency"`
	Region    string `json:"region"`
}

var ibanRe = regexp.MustCompile(`^[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}$`)

// ibanOK 是 ISO 13616 的 mod-97 校验：把前四位挪到末尾，字母换成数字，
// 整个数除以 97 余 1 才算合法。它能挡住绝大多数手抄错误。
func ibanOK(v string) bool {
	r := v[4:] + v[:4]
	m := 0
	for _, c := range r {
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'A' && c <= 'Z':
			d = int(c-'A') + 10
		default:
			return false
		}
		if d < 10 {
			m = (m*10 + d) % 97
		} else {
			m = (m*100 + d) % 97
		}
	}
	return m == 1
}

// checkAccount 校验账号并返回掩码形式。
//
// 世界上没有一种统一的银行账号格式，所以只做两件真能做的事：IBAN 走它自带的
// 校验位，其余按纯数字和长度兜底。宁可放过一个古怪的真账号，也不要把用户
// 自己的号判成错的——那种拦截他没法绕过去。
func checkAccount(raw string) (masked string, err error) {
	v := strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(raw))
	fail := func(msg string) (string, error) {
		return "", httpx.Fail(http.StatusUnprocessableEntity, "ACCOUNT_INVALID", "account_no", msg)
	}
	switch {
	case v == "":
		return fail("enter the account number")
	case regexp.MustCompile(`^[A-Z]{2}[0-9]{2}`).MatchString(v):
		if !ibanRe.MatchString(v) {
			return fail("an IBAN is 15–34 characters — check for a missing block")
		}
		if !ibanOK(v) {
			return fail("IBAN checksum does not match — one character is wrong")
		}
	case strings.ContainsFunc(v, func(r rune) bool { return r < '0' || r > '9' }):
		return fail("digits only, or a full IBAN starting with a country code")
	case len(v) < 8:
		return fail("too short — bank accounts are at least 8 digits")
	case len(v) > 19:
		return fail("too long — at most 19 digits")
	}
	// 首四末四留明文。IBAN 的前四位是国家码加校验位，是有用信息，
	// 所以按字符切，而不是只留数字。
	return v[:4] + " **** " + v[len(v)-4:], nil
}

func (s *Service) BankAccounts(ctx context.Context, ownerID string) ([]store.BankAccount, error) {
	return s.St.BankAccounts(ctx, ownerID)
}

func (s *Service) SaveBankAccount(ctx context.Context, ownerID, id string,
	req BankReq) (*store.BankAccount, error) {
	holder, bank := strings.TrimSpace(req.Holder), strings.TrimSpace(req.Bank)
	region, ccy := strings.TrimSpace(req.Region), strings.TrimSpace(req.Currency)
	if holder == "" || bank == "" || region == "" || ccy == "" {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "FIELD_REQUIRED", "",
			"account holder, bank, currency and where the bank is are all required")
	}
	masked, err := checkAccount(req.AccountNo)
	if err != nil {
		return nil, err
	}
	a := store.BankAccount{ID: id, OwnerID: ownerID, Holder: holder, Bank: bank,
		AccountNo: masked, Currency: ccy, Region: region}
	if a.ID == "" {
		a.ID = store.NewID()
		a.CreatedAt = store.Now()
	}
	a.UpdatedAt = store.Now()
	if err := s.St.UpsertBankAccount(ctx, a); err != nil {
		return nil, err
	}
	return s.St.BankAccount(ctx, ownerID, a.ID)
}

func (s *Service) DeleteBankAccount(ctx context.Context, ownerID, id string) error {
	return s.St.DeleteBankAccount(ctx, ownerID, id)
}
