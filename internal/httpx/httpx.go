// Package httpx 是 HTTP 层的公共件：统一的 JSON 响应与错误体。
package httpx

import (
	"encoding/json"
	"log"
	"net/http"
)

// Remedy 是「可点的替代」。R4 前置拦截要求：后续必然失败的，
// 在提交前拦下并给一条用户点一下就能走通的出路。
type Remedy struct {
	Action string   `json:"action"`
	Value  string   `json:"value,omitempty"`
	Label  string   `json:"label"`
	Values []string `json:"values,omitempty"`
}

// Err 的 Field 对齐 R3 校验就地：校验失败不弹窗、不跳页，
// 把错标在出错的那个字段上。
type Err struct {
	Code    string  `json:"code"`
	Field   string  `json:"field,omitempty"`
	Message string  `json:"message"`
	Remedy  *Remedy `json:"remedy,omitempty"`
	status  int
}

func (e *Err) Error() string { return e.Code + ": " + e.Message }
func (e *Err) Status() int {
	if e.status == 0 {
		return http.StatusUnprocessableEntity
	}
	return e.status
}

func Fail(status int, code, field, msg string) *Err {
	return &Err{Code: code, Field: field, Message: msg, status: status}
}

func (e *Err) With(r *Remedy) *Err { e.Remedy = r; return e }

var (
	NotFound   = func(what string) *Err { return Fail(http.StatusNotFound, "NOT_FOUND", "", what+" not found") }
	BadRequest = func(msg string) *Err { return Fail(http.StatusBadRequest, "BAD_REQUEST", "", msg) }
)

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			log.Printf("encode: %v", err)
		}
	}
}

func Error(w http.ResponseWriter, err error) {
	e, ok := err.(*Err)
	if !ok {
		e = Fail(http.StatusInternalServerError, "INTERNAL", "", err.Error())
	}
	JSON(w, e.Status(), map[string]any{"error": e})
}

func Decode(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return BadRequest("malformed JSON body: " + err.Error())
	}
	return nil
}
