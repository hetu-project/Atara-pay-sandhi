package kyc

import (
	"encoding/json"
	"strings"
)

// 决策三档。ID Analyzer 自己按 profile 里的阈值算出来，我们不重算——
// 重算等于把合规判断从一个有审计记录的系统搬进我们的 if 语句里。
const (
	DecisionAccept = "accept"
	DecisionReview = "review"
	DecisionReject = "reject"
)

// 事件名。docupass_conclusive 是唯一一个「这个人走完了」的信号：
// new 只说明他刚传了一张照片，按 new 放行等于没核。
const (
	EventConclusive = "docupass_conclusive"
	EventNew        = "new"
	EventUpdate     = "update"
	EventDelete     = "delete"
)

// Result 是一次核验的结论，webhook 和主动拉取回来的是同一份。
//
// 只留我们真的会用到的字段。`data` 里那一百多个键不逐个建模——原始 JSON
// 整份存库，要查细节去查它。
type Result struct {
	Event         string    `json:"event"`
	TransactionID string    `json:"transactionId"`
	ProfileID     string    `json:"profileId"`
	DocuPass      string    `json:"docupass"`
	Decision      string    `json:"decision"`
	CustomData    string    `json:"customData"`
	ReviewScore   int       `json:"reviewScore"`
	RejectScore   int       `json:"rejectScore"`
	Warnings      []Warning `json:"warning"`
	Data          DataMap   `json:"data"`
}

// Warning 是一条没通过的检查。severity 与 decision 由 profile 的阈值决定。
type Warning struct {
	Code        string  `json:"code"`
	Description string  `json:"description"`
	Severity    string  `json:"severity"`
	Confidence  float64 `json:"confidence"`
	Decision    string  `json:"decision"`
}

// DataMap 是证件上读出来的字段。
//
// 形状是 {key: [{value, confidence, source, index}, …]}——同一个键可能有多项，
// 因为证件正反两面可能都印着它（一面来自 visual，一面来自 MRZ 或条码）。
type DataMap map[string][]DataItem

type DataItem struct {
	Value      string  `json:"value"`
	Confidence float64 `json:"confidence"`
	Source     string  `json:"source"`
	Index      int     `json:"index"`
}

// Get 取一个字段最可信的那一项。
//
// 多项时按置信度挑，不是取第一项：正面 OCR 出来的名字可能被反光打糊，
// 而同一个名字在 MRZ 里是印刷体编码，置信度高得多。取第一项等于按证件的
// 排版顺序碰运气。
func (d DataMap) Get(key string) string {
	best, bestConf := "", -1.0
	for _, it := range d[key] {
		if strings.TrimSpace(it.Value) == "" {
			continue
		}
		if it.Confidence > bestConf {
			best, bestConf = strings.TrimSpace(it.Value), it.Confidence
		}
	}
	return best
}

// First 按顺序取第一个有值的键。证件类型不同，同一件事落在不同键上——
// 护照有 documentNumber，身份证可能只有 personalNumber。
func (d DataMap) First(keys ...string) string {
	for _, k := range keys {
		if v := d.Get(k); v != "" {
			return v
		}
	}
	return ""
}

func ParseResult(raw []byte) (*Result, error) {
	var r Result
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Identity 是从证件上读出来、可以交给前端预填表单的那几项。
//
// 刻意不包含人像、证件影像和完整证件号：那些是这次核验里最敏感的东西，
// 前端拿它们没有用处——表单上要显示的是「已核验：Liu Ellie，护照 …5678」。
// 影像留在 ID Analyzer 那边，我们连拉都不拉。
type Identity struct {
	FirstName   string `json:"first_name,omitempty"`
	LastName    string `json:"last_name,omitempty"`
	FullName    string `json:"full_name,omitempty"`
	DocType     string `json:"doc_type,omitempty"`
	DocNumber   string `json:"doc_number,omitempty"` // 已打码
	DOB         string `json:"dob,omitempty"`        // YYYY-MM-DD
	Issued      string `json:"issued,omitempty"`
	Expiry      string `json:"expiry,omitempty"`
	Sex         string `json:"sex,omitempty"`
	Nationality string `json:"nationality,omitempty"`
	Country     string `json:"country,omitempty"`
}

// docTypes 把单字母的证件类型翻成人话。认不出来的原样留着——
// 编一个「Identity card」比显示一个 "Z" 更糟：前者是假话，后者只是看不懂。
var docTypes = map[string]string{
	"P": "Passport", "D": "Driver's licence", "I": "ID card",
	"V": "Visa", "R": "Residence card", "B": "Business registration", "O": "Other",
}

var sexes = map[string]string{"M": "Male", "F": "Female", "X": "Unspecified"}

// Identity 把 data 读成可下发的那一份。
func (r *Result) Identity() Identity {
	d := r.Data
	id := Identity{
		FirstName:   d.Get("firstName"),
		LastName:    d.Get("lastName"),
		FullName:    d.First("fullName", "fullNameLocal"),
		DOB:         dashDate(d.Get("dob")),
		Issued:      dashDate(d.Get("issued")),
		Expiry:      dashDate(d.Get("expiry")),
		Nationality: d.First("nationalityFull", "nationalityIso3"),
		Country:     d.First("countryFull", "countryIso3"),
		DocNumber:   maskDoc(d.First("documentNumber", "personalNumber")),
	}
	if t := d.Get("documentType"); t != "" {
		if name, ok := docTypes[strings.ToUpper(t)]; ok {
			id.DocType = name
		} else if n := d.Get("documentName"); n != "" {
			id.DocType = n
		} else {
			id.DocType = t
		}
	}
	if s := d.Get("sex"); s != "" {
		if name, ok := sexes[strings.ToUpper(s)]; ok {
			id.Sex = name
		} else {
			id.Sex = s
		}
	}
	// 名字拆不开时证件上只有 fullName。两边都空着不如把那一个摆出来。
	if id.FirstName == "" && id.LastName == "" && id.FullName == "" {
		id.FullName = d.Get("fullNameLocal")
	}
	return id
}

// dashDate 把 ID Analyzer 的 YYYY/MM/DD 换成我们表单里用的 YYYY-MM-DD。
// 不认得的形状原样返回——猜一个格式比留着原文危险。
func dashDate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 10 && s[4] == '/' && s[7] == '/' {
		return s[:4] + "-" + s[5:7] + "-" + s[8:]
	}
	return s
}

// maskDoc 只留末四位。
//
// 证件号是这份材料里最该留在服务端的东西之一：它进了浏览器就进了内存快照、
// 进了任何一个装在这台机器上的扩展。界面上要的只是「是不是同一本证件」，
// 末四位就够回答。要看全号去查库里那份原始 JSON，那条路有服务器日志。
func maskDoc(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 4 {
		if s == "" {
			return ""
		}
		return strings.Repeat("•", len(s))
	}
	return strings.Repeat("•", len(s)-4) + s[len(s)-4:]
}
