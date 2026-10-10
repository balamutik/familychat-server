package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrUnauthenticated = errors.New("unauthenticated")
var ErrInvalidCredentials = errors.New("invalid credentials")
var ErrRegistrationClosed = errors.New("registration closed")
var ErrConflict = errors.New("conflict")
var ErrInvalidInput = errors.New("invalid input")

var loginPattern = regexp.MustCompile(`^[A-Za-z0-9_]{3,32}$`)

type Principal struct {
	DisplayName string `json:"display_name,omitempty"`
	UserID      string `json:"id"`
	SessionID   string `json:"-"`
	Login       string `json:"login"`
	Role        string `json:"role"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

type Service struct {
	DB  *pgxpool.Pool
	TTL time.Duration
}

type principalKey struct{}

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

func Bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(h, "Bearer ")
	if token == "" || strings.TrimSpace(token) != token || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

func (s *Service) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := Bearer(r)
		if !ok || s == nil || s.DB == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		p, err := s.Authenticate(r.Context(), token)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	if len(token) != 43 {
		return Principal{}, ErrUnauthenticated
	}
	h := sha256.Sum256([]byte(token))
	var p Principal
	err := s.DB.QueryRow(ctx, `SELECT u.id::text,s.id::text,u.login,u.role,u.display_name,
		CASE WHEN u.avatar_key IS NULL THEN '' ELSE '/api/v1/users/'||u.id::text||'/avatar' END
		FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token_hash=$1 AND s.revoked_at IS NULL AND s.expires_at>now() AND NOT u.disabled`, h[:]).Scan(&p.UserID, &p.SessionID, &p.Login, &p.Role, &p.DisplayName, &p.AvatarURL)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	return p, nil
}

func NewToken() (string, []byte, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	h := sha256.Sum256([]byte(token))
	return token, h[:], nil
}

func NormalizeLogin(login string) (string, error) {
	if !loginPattern.MatchString(login) {
		return "", ErrInvalidInput
	}
	return strings.ToLower(login), nil
}

// NormalizeDisplayName keeps presentation separate from the unique login.
func NormalizeDisplayName(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", ErrInvalidInput
	}
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) > 64 {
		return "", ErrInvalidInput
	}
	for _, r := range value {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return "", ErrInvalidInput
		}
	}
	return value, nil
}

func (s *Service) Register(ctx context.Context, login, password, displayName string) (Principal, error) {
	displayName, err := NormalizeDisplayName(displayName)
	if err != nil {
		return Principal{}, err
	}
	login, err = NormalizeLogin(login)
	if err != nil {
		return Principal{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return Principal{}, ErrInvalidInput
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Principal{}, err
	}
	defer tx.Rollback(ctx)
	var enabled bool
	if err := tx.QueryRow(ctx, `SELECT registration_enabled FROM settings WHERE singleton=true FOR SHARE`).Scan(&enabled); err != nil {
		return Principal{}, err
	}
	if !enabled {
		return Principal{}, ErrRegistrationClosed
	}
	var p Principal
	if err := tx.QueryRow(ctx, `INSERT INTO users(login,password_hash,display_name) VALUES($1,$2,$3) RETURNING id::text,login,role,display_name`, login, hash, displayName).Scan(&p.UserID, &p.Login, &p.Role, &p.DisplayName); err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			return Principal{}, ErrConflict
		}
		return Principal{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Principal{}, err
	}
	return p, nil
}

func (s *Service) CreateUser(ctx context.Context, login, password string) (Principal, error) {
	login, err := NormalizeLogin(login)
	if err != nil {
		return Principal{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return Principal{}, ErrInvalidInput
	}
	var p Principal
	err = s.DB.QueryRow(ctx, `INSERT INTO users(login,password_hash) VALUES($1,$2) RETURNING id::text,login,role`, login, hash).Scan(&p.UserID, &p.Login, &p.Role)
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			return Principal{}, ErrConflict
		}
		return Principal{}, err
	}
	return p, nil
}

func (s *Service) Login(ctx context.Context, login, password string) (string, Principal, error) {
	login, err := NormalizeLogin(login)
	if err != nil {
		return "", Principal{}, ErrInvalidCredentials
	}
	var p Principal
	var hash string
	err = s.DB.QueryRow(ctx, `SELECT id::text,login,role,password_hash,display_name,
		CASE WHEN avatar_key IS NULL THEN '' ELSE '/api/v1/users/'||id::text||'/avatar' END
		FROM users WHERE login=$1 AND NOT disabled`, login).Scan(&p.UserID, &p.Login, &p.Role, &hash, &p.DisplayName, &p.AvatarURL)
	if err != nil {
		return "", Principal{}, ErrInvalidCredentials
	}
	if !VerifyPassword(hash, password) {
		return "", Principal{}, ErrInvalidCredentials
	}
	token, tokenHash, err := NewToken()
	if err != nil {
		return "", Principal{}, err
	}
	ttl := s.TTL
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	err = s.DB.QueryRow(ctx, `INSERT INTO sessions(user_id,token_hash,expires_at) VALUES($1,$2,now()+$3::interval) RETURNING id::text`, p.UserID, tokenHash, ttl.String()).Scan(&p.SessionID)
	if err != nil {
		return "", Principal{}, err
	}
	return token, p, nil
}

func (s *Service) RevokeSession(ctx context.Context, sessionID string) error {
	_, err := s.DB.Exec(ctx, `UPDATE sessions SET revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`, sessionID)
	return err
}

func (s *Service) RevokeUser(ctx context.Context, userID string) error {
	_, err := s.DB.Exec(ctx, `UPDATE sessions SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, userID)
	return err
}

func (s *Service) ChangePassword(ctx context.Context, p Principal, current, newPassword string) error {
	var hash string
	if err := s.DB.QueryRow(ctx, `SELECT password_hash FROM users WHERE id=$1 AND NOT disabled`, p.UserID).Scan(&hash); err != nil {
		return ErrInvalidCredentials
	}
	if !VerifyPassword(hash, current) {
		return ErrInvalidCredentials
	}
	newHash, err := HashPassword(newPassword)
	if err != nil {
		return ErrInvalidInput
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE users SET password_hash=$2 WHERE id=$1 AND password_hash=$3`, p.UserID, newHash, hash)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrInvalidCredentials
	}
	if _, err = tx.Exec(ctx, `UPDATE sessions SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, p.UserID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) CreateWebSocketTicket(ctx context.Context, p Principal) (string, error) {
	token, hash, err := NewToken()
	if err != nil {
		return "", err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO ws_tickets(session_id,token_hash,expires_at) VALUES($1,$2,now()+interval '30 seconds')`, p.SessionID, hash)
	if err != nil {
		return "", err
	}
	return token, nil
}

func (s *Service) ConsumeWebSocketTicket(ctx context.Context, ticket string) (Principal, error) {
	if len(ticket) != 43 {
		return Principal{}, ErrUnauthenticated
	}
	hash := sha256.Sum256([]byte(ticket))
	var p Principal
	err := s.DB.QueryRow(ctx, `WITH consumed AS (
		UPDATE ws_tickets SET consumed_at=now() WHERE token_hash=$1 AND consumed_at IS NULL AND expires_at>now()
		RETURNING session_id
	) SELECT u.id::text,s.id::text,u.login,u.role FROM consumed t
		JOIN sessions s ON s.id=t.session_id JOIN users u ON u.id=s.user_id
		WHERE s.revoked_at IS NULL AND s.expires_at>now() AND NOT u.disabled`, hash[:]).Scan(&p.UserID, &p.SessionID, &p.Login, &p.Role)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	return p, nil
}
