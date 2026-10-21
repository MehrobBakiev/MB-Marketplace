package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"marketplace/internal/api"
)

type claims struct {
	Role      api.Role `json:"role"`
	TokenType string   `json:"token_type"`
	jwt.RegisteredClaims
}
type principal struct {
	ID   int64
	Role api.Role
}
type principalKey struct{}

func current(r *http.Request) principal {
	p, _ := r.Context().Value(principalKey{}).(principal)
	return p
}
func permitted(r *http.Request, roles ...api.Role) error {
	for _, role := range roles {
		if current(r).Role == role {
			return nil
		}
	}
	return problem(403, "ACCESS_DENIED", "Недостаточно прав")
}

func (a *App) EnsureAdmin(ctx context.Context) error {
	if a.cfg.AdminEmail == "" {
		return nil
	}
	email := strings.ToLower(strings.TrimSpace(a.cfg.AdminEmail))
	hash, err := bcrypt.GenerateFromPassword([]byte(a.cfg.AdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, `INSERT INTO users(email,password_hash,role) VALUES($1,$2,'ADMIN') ON CONFLICT(email) DO NOTHING`, email, string(hash))
	if err != nil {
		return err
	}
	var role string
	if err = a.db.QueryRowContext(ctx, `SELECT role FROM users WHERE email=$1`, email).Scan(&role); err != nil {
		return err
	}
	if role != "ADMIN" {
		return fmt.Errorf("configured ADMIN_EMAIL belongs to a non-admin user")
	}
	return nil
}

func (a *App) Register(w http.ResponseWriter, r *http.Request) {
	var in api.RegisterRequest
	if err := decode(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	if in.Password == nil || len(*in.Password) > 72 {
		a.fail(w, r, invalid("password", "Пароль должен содержать 8–72 байта"))
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(*in.Password), bcrypt.DefaultCost)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	role := api.RoleUSER
	if in.Role != nil {
		role = api.Role(*in.Role)
	}
	var user api.UserResponse
	err = a.db.QueryRowContext(r.Context(), `INSERT INTO users(email,password_hash,role) VALUES($1,$2,$3) RETURNING id,email,role`, strings.ToLower(strings.TrimSpace(string(in.Email))), string(hash), role).Scan(&user.Id, &user.Email, &user.Role)
	if isUnique(err) {
		err = problem(409, "USER_ALREADY_EXISTS", "Пользователь с таким email уже существует")
	}
	a.respond(w, r, 201, user, err)
}

func (a *App) Login(w http.ResponseWriter, r *http.Request) {
	var in api.LoginRequest
	if err := decode(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	var user api.UserResponse
	var hash string
	err := a.db.QueryRowContext(r.Context(), `SELECT id,email,role,password_hash FROM users WHERE email=$1`, strings.ToLower(strings.TrimSpace(string(in.Email)))).Scan(&user.Id, &user.Email, &user.Role, &hash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		a.fail(w, r, err)
		return
	}
	if in.Password == nil || errors.Is(err, sql.ErrNoRows) || bcrypt.CompareHashAndPassword([]byte(hash), []byte(*in.Password)) != nil {
		a.fail(w, r, problem(401, "INVALID_CREDENTIALS", "Неверный email или пароль"))
		return
	}
	tokens, err := a.issue(r.Context(), a.db, user)
	a.respond(w, r, 200, tokens, err)
}

func (a *App) issue(ctx context.Context, q querier, user api.UserResponse) (api.AuthResponse, error) {
	now := a.now().UTC()
	makeClaims := func(kind string, ttl time.Duration) claims {
		return claims{Role: user.Role, TokenType: kind, RegisteredClaims: jwt.RegisteredClaims{Issuer: "marketplace", Subject: strconv.FormatInt(user.Id, 10), Audience: jwt.ClaimStrings{"marketplace-" + kind}, ExpiresAt: jwt.NewNumericDate(now.Add(ttl)), IssuedAt: jwt.NewNumericDate(now), ID: uuid.NewString()}}
	}
	accessClaims := makeClaims("access", a.cfg.AccessTTL)
	refreshClaims := makeClaims("refresh", a.cfg.RefreshTTL)
	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims).SignedString([]byte(a.cfg.JWTSecret))
	if err != nil {
		return api.AuthResponse{}, err
	}
	refresh, err := jwt.NewWithClaims(jwt.SigningMethodHS256, refreshClaims).SignedString([]byte(a.cfg.JWTSecret))
	if err != nil {
		return api.AuthResponse{}, err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO refresh_tokens(id,user_id,expires_at) VALUES($1,$2,$3)`, refreshClaims.ID, user.Id, refreshClaims.ExpiresAt.Time)
	return api.AuthResponse{AccessToken: access, RefreshToken: refresh, TokenType: api.Bearer, ExpiresIn: int64(a.cfg.AccessTTL.Seconds()), User: user}, err
}

func (a *App) parseToken(raw, kind string) (*claims, error) {
	c := new(claims)
	_, err := jwt.ParseWithClaims(raw, c, func(token *jwt.Token) (interface{}, error) { return []byte(a.cfg.JWTSecret), nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("marketplace"), jwt.WithAudience("marketplace-"+kind), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(a.now))
	if err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(c.Subject, 10, 64)
	if err != nil || id < 1 || c.TokenType != kind || !c.Role.Valid() || c.IssuedAt == nil || c.ID == "" {
		return nil, jwt.ErrTokenInvalidClaims
	}
	if _, err = uuid.Parse(c.ID); err != nil {
		return nil, jwt.ErrTokenInvalidClaims
	}
	return c, nil
}

func (a *App) Refresh(w http.ResponseWriter, r *http.Request) {
	var in api.RefreshRequest
	if err := decode(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	c, err := a.parseToken(in.RefreshToken, "refresh")
	if err != nil {
		a.fail(w, r, problem(401, "REFRESH_TOKEN_INVALID", "Refresh token недействителен"))
		return
	}
	var out api.AuthResponse
	err = a.tx(r.Context(), func(tx *sql.Tx) error {
		var user api.UserResponse
		err := tx.QueryRowContext(r.Context(), `SELECT u.id,u.email,u.role FROM refresh_tokens t JOIN users u ON u.id=t.user_id WHERE t.id=$1 AND t.user_id=$2 AND t.revoked_at IS NULL AND t.expires_at>$3 FOR UPDATE OF t`, c.ID, c.Subject, a.now()).Scan(&user.Id, &user.Email, &user.Role)
		if errors.Is(err, sql.ErrNoRows) {
			return problem(401, "REFRESH_TOKEN_INVALID", "Refresh token недействителен или уже использован")
		}
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(r.Context(), `UPDATE refresh_tokens SET revoked_at=$2 WHERE id=$1`, c.ID, a.now()); err != nil {
			return err
		}
		out, err = a.issue(r.Context(), tx, user)
		return err
	})
	a.respond(w, r, 200, out, err)
}
