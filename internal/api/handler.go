// Package api 是 HTTP 层：DTO 与 handler。这一层不开事务，不碰 *sql.Tx。
package api

import (
	"net/http"

	"github.com/advaita/atara-pay/internal/app"
	"github.com/advaita/atara-pay/internal/auth"
	"github.com/advaita/atara-pay/internal/config"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/store"
)

type Handler struct {
	St  *store.Store
	Svc *app.Service
	Cfg config.Config
}

func New(st *store.Store, svc *app.Service, cfg config.Config) *Handler {
	return &Handler{St: st, Svc: svc, Cfg: cfg}
}

func (h *Handler) actorID(r *http.Request) string {
	if u := auth.Actor(r.Context()); u != nil {
		return u.ID
	}
	return ""
}

func (h *Handler) confirmToken(r *http.Request) string { return r.Header.Get(auth.HeaderConfirm) }

func ok(w http.ResponseWriter, v any) { httpx.JSON(w, http.StatusOK, v) }
