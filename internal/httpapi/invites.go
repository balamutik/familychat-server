package httpapi

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"familychat/server/internal/auth"
	"github.com/jackc/pgx/v5"
)

//go:embed invitepage/*
var invitePageFiles embed.FS
var invitePage = template.Must(template.ParseFS(invitePageFiles, "invitepage/index.html"))

type invitation struct {
	ID                  string     `json:"id"`
	Token               string     `json:"-"`
	ServerName          string     `json:"server_name"`
	ServerURL           string     `json:"server_url"`
	URL                 string     `json:"url"`
	CreatedAt           time.Time  `json:"created_at"`
	ExpiresAt           time.Time  `json:"expires_at"`
	RevokedAt           *time.Time `json:"revoked_at"`
	RegistrationEnabled bool       `json:"registration_enabled"`
}

func (v *invitation) setURL() { v.URL = v.ServerURL + "/invite/" + v.Token }

func inviteServerURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(u.Host, "\\ \t\r\n") {
		return "", errors.New("invalid URL")
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return "", errors.New("invalid port")
		}
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	return strings.TrimRight(u.String(), "/"), nil
}
func validInviteToken(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && s == strings.ToLower(s)
}

func registerInvites(mux *http.ServeMux, d Dependencies) {
	mux.Handle("POST /api/v1/admin/invites", adminOnly(d, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ServerName string    `json:"server_name"`
			ServerURL  string    `json:"server_url"`
			ExpiresAt  time.Time `json:"expires_at"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		name := strings.TrimSpace(body.ServerName)
		base, err := inviteServerURL(body.ServerURL)
		if err != nil || len(body.ServerURL) > 2048 || name == "" || utf8.RuneCountInString(name) > 100 || !body.ExpiresAt.After(time.Now()) || body.ExpiresAt.After(time.Now().AddDate(1, 0, 0)) {
			writeError(w, 400, "invalid_invitation")
			return
		}
		var bytes [32]byte
		if _, err = rand.Read(bytes[:]); err != nil {
			writeError(w, 500, "internal")
			return
		}
		v := invitation{Token: hex.EncodeToString(bytes[:]), ServerName: name, ServerURL: base, ExpiresAt: body.ExpiresAt.UTC()}
		p, _ := auth.PrincipalFrom(r.Context())
		err = d.DB.QueryRow(r.Context(), `INSERT INTO invitations(token,server_name,server_url,created_by,expires_at) VALUES($1,$2,$3,$4,$5) RETURNING id::text,created_at`, v.Token, name, base, p.UserID, v.ExpiresAt).Scan(&v.ID, &v.CreatedAt)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		v.setURL()
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 201, v)
	}))
	mux.Handle("GET /api/v1/admin/invites", adminOnly(d, func(w http.ResponseWriter, r *http.Request) {
		rows, err := d.DB.Query(r.Context(), `SELECT id::text,token,server_name,server_url,created_at,expires_at,revoked_at FROM invitations ORDER BY created_at DESC LIMIT 100`)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer rows.Close()
		items := []invitation{}
		for rows.Next() {
			var v invitation
			if err = rows.Scan(&v.ID, &v.Token, &v.ServerName, &v.ServerURL, &v.CreatedAt, &v.ExpiresAt, &v.RevokedAt); err != nil {
				writeError(w, 500, "internal")
				return
			}
			v.setURL()
			items = append(items, v)
		}
		if rows.Err() != nil {
			writeError(w, 500, "internal")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]any{"invitations": items})
	}))
	mux.Handle("DELETE /api/v1/admin/invites/{id}", adminOnly(d, func(w http.ResponseWriter, r *http.Request) {
		// Comparing text also makes malformed IDs a 404, without a SQL cast error.
		result, err := d.DB.Exec(r.Context(), `UPDATE invitations SET revoked_at=COALESCE(revoked_at,now()) WHERE id::text=$1`, r.PathValue("id"))
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		if result.RowsAffected() == 0 {
			writeError(w, 404, "not_found")
			return
		}
		w.WriteHeader(204)
	}))
	resolve := func(w http.ResponseWriter, r *http.Request) (invitation, int) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		var v invitation
		token := r.PathValue("token")
		if !validInviteToken(token) {
			return v, 404
		}
		if d.DB == nil {
			return v, 503
		}
		err := d.DB.QueryRow(r.Context(), `SELECT i.server_name,i.server_url,i.expires_at,i.revoked_at,s.registration_enabled FROM invitations i CROSS JOIN settings s WHERE i.token=$1 AND s.singleton=true`, token).Scan(&v.ServerName, &v.ServerURL, &v.ExpiresAt, &v.RevokedAt, &v.RegistrationEnabled)
		if errors.Is(err, pgx.ErrNoRows) {
			return v, 404
		}
		if err != nil {
			return v, 503
		}
		if v.RevokedAt != nil || !v.ExpiresAt.After(time.Now()) {
			return invitation{}, 410
		}
		v.Token = token
		v.setURL()
		return v, 200
	}
	mux.HandleFunc("GET /api/v1/invites/{token}", func(w http.ResponseWriter, r *http.Request) {
		v, status := resolve(w, r)
		if status != 200 {
			writeError(w, status, "invitation_unavailable")
			return
		}
		writeJSON(w, 200, map[string]any{"server_name": v.ServerName, "server_url": v.ServerURL, "expires_at": v.ExpiresAt, "registration_enabled": v.RegistrationEnabled})
	})
	mux.HandleFunc("GET /invite/{token}", func(w http.ResponseWriter, r *http.Request) {
		v, status := resolve(w, r)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		data := struct {
			Valid                             bool
			Name, Address, InviteURL, Message string
		}{Valid: status == 200, Name: v.ServerName, Address: v.ServerURL, InviteURL: v.URL}
		if status == 410 {
			data.Message = "Срок приглашения истёк или администратор отозвал ссылку. Попросите новое приглашение."
		} else if status != 200 {
			data.Message = "Приглашение недоступно. Проверьте ссылку или попробуйте позже."
		}
		w.WriteHeader(status)
		_ = invitePage.Execute(w, data)
	})
	for _, asset := range []string{"invite.js", "invite.css"} {
		mux.HandleFunc("GET /invite-assets/"+asset, func(w http.ResponseWriter, r *http.Request) {
			body, _ := invitePageFiles.ReadFile("invitepage/" + asset)
			kind := "text/css; charset=utf-8"
			if strings.HasSuffix(asset, ".js") {
				kind = "text/javascript; charset=utf-8"
			}
			w.Header().Set("Content-Type", kind)
			w.Header().Set("X-Content-Type-Options", "nosniff")
			_, _ = w.Write(body)
		})
	}
}
