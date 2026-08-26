package api

import (
	"net/http"
	"strings"

	"github.com/advaita/atara-pay/internal/auth"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func (h *Handler) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
	r.Use(cors(h.Cfg.CORSOrigins))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(auth.Middleware(store.DemoHandle, h.St.UserByHandle))

		r.Route("/catalog", func(r chi.Router) {
			r.Get("/assets", h.Assets)
			r.Get("/fiats", h.Fiats)
			r.Get("/conditions", h.Conditions)
			r.Get("/intents", h.Intents)
		})

		r.Get("/wallet", h.Wallet)
		r.Get("/authorization-cards", h.Cards)
		r.Get("/counterparties", h.Counterparties)
		r.Post("/passkey/assert", h.PasskeyAssert)
		r.Post("/uploads", h.Upload)
		r.Get("/uploads/*", h.ServeUpload)

		// New order
		r.Route("/orders", func(r chi.Router) {
			r.Post("/parse", h.Parse)
			r.Post("/quote", h.Quote)
			r.Post("/", h.CreateOrder)
			r.Get("/", h.ListOrders)
			r.Route("/{id}", func(r chi.Router) {
				r.Get("/", h.GetOrder)
				r.Get("/events", h.OrderEvents)
				r.Get("/release-consensus", h.ReleaseConsensus)
				r.Post("/confirm", h.Confirm)
				r.Post("/evidence", h.Evidence)
				r.Post("/cancel", h.Cancel)
				r.Post("/dispute", h.Dispute)
				r.Post("/accept", h.Accept)
				r.Post("/fund", h.Fund)
				r.Post("/receipt", h.Receipt)
			})
		})

		// Trade
		r.Route("/offers", func(r chi.Router) {
			r.Get("/", h.ListOffers)
			r.Post("/", h.CreateOffer)
			r.Get("/mine", h.MyOffers)
			r.Route("/{id}", func(r chi.Router) {
				r.Get("/", h.GetOffer)
				r.Delete("/", h.Delist)
				r.Get("/dossier", h.Dossier)
				r.Get("/assessment", h.Assessment)
				r.Post("/take", h.Take)
			})
		})
	})
	return r
}

func cors(origins string) func(http.Handler) http.Handler {
	allowed := strings.Split(origins, ",")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			o := r.Header.Get("Origin")
			allow := origins == "*"
			for _, a := range allowed {
				if strings.TrimSpace(a) == o && o != "" {
					allow = true
				}
			}
			if allow {
				if origins == "*" {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				} else {
					w.Header().Set("Access-Control-Allow-Origin", o)
				}
				w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers",
					"Content-Type,"+auth.HeaderUser+","+auth.HeaderConfirm)
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
