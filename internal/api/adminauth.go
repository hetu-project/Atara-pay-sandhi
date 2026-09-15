package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/advaita/atara-pay/internal/auth"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/go-chi/chi/v5"
)

// loginLim throttles admin login attempts per client IP (brute-force guard).
var loginLim = newLoginLimiter()

// AdminLogin authenticates username + password and, on success, issues a session
// token. Public endpoint (there is of course no session before login).
func (h *Handler) AdminLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	// Rate limit before doing any password work — don't let a blocked key keep
	// spending bcrypt time.
	key := clientIP(r)
	if okToTry, retry := loginLim.allow(key); !okToTry {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		httpx.Error(w, httpx.Fail(http.StatusTooManyRequests, "TOO_MANY_ATTEMPTS", "",
			"too many failed attempts — try again later"))
		return
	}
	a, err := h.St.VerifyAdminPassword(r.Context(), req.Username, req.Password)
	if err != nil {
		loginLim.fail(key)
		// Do not distinguish "no such account" from "wrong password" — same reply for both.
		httpx.Error(w, httpx.Fail(http.StatusUnauthorized, "BAD_CREDENTIALS", "",
			"username or password is incorrect"))
		return
	}
	loginLim.reset(key)
	token, exp, err := h.St.CreateAdminSession(r.Context(), a.ID)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	name := a.Name
	if name == "" {
		name = a.Email
	}
	ok(w, map[string]any{
		"token":      token,
		"expires_at": exp.Format(time.RFC3339),
		"admin":      map[string]string{"username": a.Email, "name": name, "role": a.Role},
	})
}

// AdminLogout revokes the current session token. Behind RequireAdmin — being able
// to log out means you were logged in.
func (h *Handler) AdminLogout(w http.ResponseWriter, r *http.Request) {
	token := bearer(r)
	if token != "" {
		_ = h.St.DeleteAdminSession(r.Context(), token)
	}
	ok(w, map[string]any{"ok": true})
}

// AdminMe returns who is currently signed in (the frontend uses it to render the
// footer and to confirm the session is still valid).
func (h *Handler) AdminMe(w http.ResponseWriter, r *http.Request) {
	u := auth.Actor(r.Context())
	if u == nil {
		httpx.Error(w, httpx.Fail(http.StatusUnauthorized, "ADMIN_AUTH_REQUIRED", "", "not signed in"))
		return
	}
	ok(w, map[string]any{"id": u.ID, "name": u.DisplayName, "username": u.Email, "role": u.Role})
}

// selfAdminID is the real admin-account id of the caller (the actor id carries an
// "admin:" prefix; strip it).
func (h *Handler) selfAdminID(r *http.Request) string {
	if u := auth.Actor(r.Context()); u != nil {
		return strings.TrimPrefix(u.ID, "admin:")
	}
	return ""
}

// AdminListAdmins lists the admin accounts (no password hashes).
func (h *Handler) AdminListAdmins(w http.ResponseWriter, r *http.Request) {
	rows, err := h.St.AdminAccounts(r.Context())
	if err != nil {
		httpx.Error(w, err)
		return
	}
	self := h.selfAdminID(r)
	// Mark which row is the caller so the UI can prevent disabling yourself.
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, map[string]any{
			"id": a.ID, "username": a.Username, "name": a.Name, "role": a.Role,
			"disabled": a.Disabled, "created_at": a.CreatedAt, "is_self": a.ID == self,
		})
	}
	ok(w, map[string]any{"admins": out})
}

// AdminCreateAdmin adds a new admin account.
func (h *Handler) AdminCreateAdmin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Name     string `json:"name"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || len(req.Password) < 6 {
		httpx.Error(w, httpx.Fail(http.StatusUnprocessableEntity, "BAD_INPUT", "",
			"username is required and the password must be at least 6 characters"))
		return
	}
	if _, err := h.St.AdminByEmail(r.Context(), req.Username); err == nil {
		httpx.Error(w, httpx.Fail(http.StatusConflict, "USERNAME_TAKEN", "username",
			"that username already exists"))
		return
	}
	if err := h.St.CreateAdminAccount(r.Context(), req.Username, req.Password, req.Name, "admin"); err != nil {
		httpx.Error(w, err)
		return
	}
	h.audit(r, "admin.create", "admin", req.Username, "")
	ok(w, map[string]any{"ok": true})
}

// AdminResetPassword resets another admin's password.
func (h *Handler) AdminResetPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	if len(req.Password) < 6 {
		httpx.Error(w, httpx.Fail(http.StatusUnprocessableEntity, "BAD_INPUT", "password",
			"the password must be at least 6 characters"))
		return
	}
	id := chi.URLParam(r, "id")
	if err := h.St.SetAdminPassword(r.Context(), id, req.Password); err != nil {
		httpx.Error(w, httpx.NotFound("admin"))
		return
	}
	h.audit(r, "admin.reset_password", "admin", id, "")
	ok(w, map[string]any{"ok": true})
}

// AdminSetDisabled enables/disables an admin. You can't disable yourself.
func (h *Handler) AdminSetDisabled(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Disabled bool `json:"disabled"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	id := chi.URLParam(r, "id")
	if id == h.selfAdminID(r) {
		httpx.Error(w, httpx.Fail(http.StatusConflict, "CANNOT_DISABLE_SELF", "",
			"you can't disable your own account"))
		return
	}
	if err := h.St.SetAdminDisabled(r.Context(), id, req.Disabled); err != nil {
		httpx.Error(w, httpx.NotFound("admin"))
		return
	}
	action := "admin.disable"
	if !req.Disabled {
		action = "admin.enable"
	}
	h.audit(r, action, "admin", id, "")
	ok(w, map[string]any{"ok": true})
}

// AdminChangePassword changes the caller's own password (needs the current one).
func (h *Handler) AdminChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	if len(req.New) < 6 {
		httpx.Error(w, httpx.Fail(http.StatusUnprocessableEntity, "BAD_INPUT", "new",
			"the new password must be at least 6 characters"))
		return
	}
	u := auth.Actor(r.Context())
	if u == nil {
		httpx.Error(w, httpx.Fail(http.StatusUnauthorized, "ADMIN_AUTH_REQUIRED", "", "not signed in"))
		return
	}
	// Verify the current password by re-authenticating with the caller's username.
	if _, err := h.St.VerifyAdminPassword(r.Context(), u.Email, req.Current); err != nil {
		httpx.Error(w, httpx.Fail(http.StatusUnauthorized, "BAD_CURRENT", "current",
			"the current password is incorrect"))
		return
	}
	if err := h.St.SetAdminPassword(r.Context(), h.selfAdminID(r), req.New); err != nil {
		httpx.Error(w, err)
		return
	}
	h.audit(r, "admin.change_password", "admin", h.selfAdminID(r), "self")
	// The password change revoked all this admin's sessions (including this one),
	// so the client must log in again.
	ok(w, map[string]any{"ok": true, "relogin": true})
}

// bearer pulls the token out of the Authorization header (logout needs it to delete).
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return ""
}
