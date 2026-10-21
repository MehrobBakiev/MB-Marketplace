package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"marketplace/internal/api"
)

type integrationSuite struct {
	t                    *testing.T
	a                    *App
	db                   *sql.DB
	handler              http.Handler
	prefix               string
	admin, seller, buyer api.AuthResponse
}

func (s *integrationSuite) raw(token, method, path string, body interface{}) (*http.Request, *httptest.ResponseRecorder) {
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return req, rec
}
func (s *integrationSuite) check(req *http.Request, rec *httptest.ResponseRecorder, status int, code string, out interface{}) {
	s.t.Helper()
	if rec.Code != status {
		s.t.Fatalf("%s %s: got %d want %d: %s", req.Method, req.URL.Path, rec.Code, status, rec.Body.String())
	}
	if _, err := uuid.Parse(rec.Header().Get("X-Request-Id")); err != nil {
		s.t.Fatal("missing X-Request-Id")
	}
	route, params, err := s.a.router.FindRoute(req)
	if err != nil {
		s.t.Fatal(err)
	}
	input := &openapi3filter.ResponseValidationInput{RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route}, Status: rec.Code, Header: rec.Header(), Body: io.NopCloser(bytes.NewReader(rec.Body.Bytes())), Options: &openapi3filter.Options{MultiError: true}}
	if err = openapi3filter.ValidateResponse(context.Background(), input); err != nil {
		s.t.Fatalf("response violates OpenAPI: %v; body=%s", err, rec.Body.String())
	}
	if code != "" {
		var e api.ErrorResponse
		if err = json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			s.t.Fatal(err)
		}
		if string(e.ErrorCode) != code {
			s.t.Fatalf("got %s want %s: %s", e.ErrorCode, code, rec.Body.String())
		}
	}
	if out != nil {
		if err = json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			s.t.Fatal(err)
		}
	}
}
func (s *integrationSuite) call(token, method, path string, body interface{}, status int, code string, out interface{}) {
	s.t.Helper()
	req, rec := s.raw(token, method, path, body)
	s.check(req, rec, status, code, out)
}
func (s *integrationSuite) account(role string) api.AuthResponse {
	s.t.Helper()
	email := s.prefix + uuid.NewString() + "@example.com"
	s.call("", "POST", "/auth/register", map[string]interface{}{"email": email, "password": "Password123!", "role": role}, 201, "", nil)
	var auth api.AuthResponse
	s.call("", "POST", "/auth/login", map[string]interface{}{"email": email, "password": "Password123!"}, 200, "", &auth)
	return auth
}
func (s *integrationSuite) product(price float64, stock int64, status string) api.ProductResponse {
	s.t.Helper()
	var p api.ProductResponse
	s.call(s.seller.AccessToken, "POST", "/products", map[string]interface{}{"name": "Товар", "price": price, "stock": stock, "category": s.prefix, "status": status}, 201, "", &p)
	return p
}
func items(id, quantity int64) map[string]interface{} {
	return map[string]interface{}{"items": []map[string]interface{}{{"product_id": id, "quantity": quantity}}}
}
func (s *integrationSuite) order(user api.AuthResponse, p api.ProductResponse, quantity int64, promo string) api.OrderResponse {
	s.t.Helper()
	body := items(p.Id, quantity)
	if promo != "" {
		body["promo_code"] = promo
	}
	var o api.OrderResponse
	s.call(user.AccessToken, "POST", "/orders", body, 201, "", &o)
	return o
}
func (s *integrationSuite) stock(id, want int64) {
	s.t.Helper()
	var got int64
	if err := s.db.QueryRow(`SELECT stock FROM products WHERE id=$1`, id).Scan(&got); err != nil {
		s.t.Fatal(err)
	}
	if got != want {
		s.t.Fatalf("stock product %d: got %d want %d", id, got, want)
	}
}
func (s *integrationSuite) uses(id, want int64) {
	s.t.Helper()
	var got int64
	if err := s.db.QueryRow(`SELECT current_uses FROM promo_codes WHERE id=$1`, id).Scan(&got); err != nil {
		s.t.Fatal(err)
	}
	if got != want {
		s.t.Fatalf("uses promo %d: got %d want %d", id, got, want)
	}
}
func (s *integrationSuite) age(userID int64, kind string) {
	s.t.Helper()
	if _, err := s.db.Exec(`UPDATE user_operations SET created_at=created_at-interval '2 minutes' WHERE user_id=$1 AND operation_type=$2`, userID, kind); err != nil {
		s.t.Fatal(err)
	}
}
func (s *integrationSuite) promo(kind string, value, minimum float64, max int64, active bool, expired bool) api.PromoCodeResponse {
	s.t.Helper()
	now := time.Now().UTC()
	until := now.Add(time.Hour)
	if expired {
		until = now.Add(-time.Minute)
	}
	var p api.PromoCodeResponse
	s.call(s.seller.AccessToken, "POST", "/promo-codes", map[string]interface{}{"code": "P" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))[:15], "discount_type": kind, "discount_value": value, "min_order_amount": minimum, "max_uses": max, "valid_from": now.Add(-time.Hour), "valid_until": until, "active": active}, 201, "", &p)
	return p
}
func orderPath(id int64) string { return fmt.Sprintf("/orders/%d", id) }

func TestIntegration(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to a PostgreSQL database migrated by Flyway")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(20)
	cfg := testConfig()
	prefix := "it-" + uuid.NewString()
	cfg.AdminEmail = prefix + "-admin@example.com"
	cfg.AdminPassword = "Admin-test-123!"
	a, err := New(db, cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.EnsureAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := &integrationSuite{t: t, a: a, db: db, handler: a.Handler(), prefix: prefix}
	s.call("", "POST", "/auth/login", map[string]interface{}{"email": cfg.AdminEmail, "password": cfg.AdminPassword}, 200, "", &s.admin)
	s.seller = s.account("SELLER")
	s.buyer = s.account("USER")
	t.Run("auth_rotation_and_token_errors", func(t *testing.T) {
		s.t = t
		s.call("", "GET", "/products", nil, 401, "TOKEN_INVALID", nil)
		s.call(s.buyer.RefreshToken, "GET", "/products", nil, 401, "TOKEN_INVALID", nil)
		expired := signedToken(t, cfg, "access", api.RoleUSER, s.buyer.User.Id, time.Now().Add(-time.Second))
		s.call(expired, "GET", "/products", nil, 401, "TOKEN_EXPIRED", nil)
		s.call("", "POST", "/auth/login", map[string]interface{}{"email": cfg.AdminEmail, "password": "wrong"}, 401, "INVALID_CREDENTIALS", nil)
		s.call("", "POST", "/auth/register", map[string]interface{}{"email": cfg.AdminEmail, "password": "Password123!"}, 409, "USER_ALREADY_EXISTS", nil)
		s.call("", "POST", "/auth/refresh", map[string]interface{}{"refresh_token": s.buyer.AccessToken}, 401, "REFRESH_TOKEN_INVALID", nil)
		var rotated api.AuthResponse
		s.call("", "POST", "/auth/refresh", map[string]interface{}{"refresh_token": s.buyer.RefreshToken}, 200, "", &rotated)
		s.call("", "POST", "/auth/refresh", map[string]interface{}{"refresh_token": s.buyer.RefreshToken}, 401, "REFRESH_TOKEN_INVALID", nil)
		s.call(rotated.AccessToken, "GET", "/products", nil, 200, "", nil)
	})
	t.Run("product_crud_pagination_and_roles", func(t *testing.T) {
		s.t = t
		other := s.account("SELLER")
		p := s.product(100.25, 10, "ACTIVE")
		p2 := s.product(25, 0, "INACTIVE")
		s.call(s.buyer.AccessToken, "POST", "/products", map[string]interface{}{"name": "x", "price": 1, "stock": 0, "category": "x", "status": "ACTIVE"}, 403, "ACCESS_DENIED", nil)
		update := map[string]interface{}{"name": "Обновлённый", "description": nil, "price": 120.50, "stock": 9, "category": s.prefix, "status": "ACTIVE"}
		s.call(other.AccessToken, "PUT", fmt.Sprintf("/products/%d", p.Id), update, 403, "ACCESS_DENIED", nil)
		s.call(other.AccessToken, "DELETE", fmt.Sprintf("/products/%d", p.Id), nil, 403, "ACCESS_DENIED", nil)
		var changed api.ProductResponse
		s.call(s.seller.AccessToken, "PUT", fmt.Sprintf("/products/%d", p.Id), update, 200, "", &changed)
		if !changed.UpdatedAt.After(p.UpdatedAt) {
			t.Fatal("updated_at did not change")
		}
		for _, auth := range []api.AuthResponse{s.admin, s.buyer, s.seller} {
			s.call(auth.AccessToken, "GET", fmt.Sprintf("/products/%d", p.Id), nil, 200, "", nil)
		}
		var page api.ProductPage
		s.call(s.buyer.AccessToken, "GET", "/products?category="+s.prefix+"&page=1&size=1", nil, 200, "", &page)
		if page.TotalElements != 2 || len(page.Items) != 1 || page.Items[0].Id != p2.Id {
			t.Fatalf("bad page: %+v", page)
		}
		s.call(s.buyer.AccessToken, "GET", "/products?category="+s.prefix+"&status=INACTIVE", nil, 200, "", &page)
		if page.TotalElements != 1 {
			t.Fatal("status filter failed")
		}
		s.call(s.buyer.AccessToken, "GET", "/products?category="+s.prefix+"-different", nil, 200, "", &page)
		if page.TotalElements != 0 || len(page.Items) != 0 {
			t.Fatal("category must match exactly")
		}
		s.call(s.seller.AccessToken, "DELETE", fmt.Sprintf("/products/%d", p.Id), nil, 204, "", nil)
		s.call(s.seller.AccessToken, "DELETE", fmt.Sprintf("/products/%d", p.Id), nil, 204, "", nil)
		var archived api.ProductResponse
		s.call(s.buyer.AccessToken, "GET", fmt.Sprintf("/products/%d", p.Id), nil, 200, "", &archived)
		if archived.Status != api.ARCHIVED {
			t.Fatal("DELETE was not soft")
		}
		s.call(s.buyer.AccessToken, "GET", "/products/9223372036854775807", nil, 404, "PRODUCT_NOT_FOUND", nil)
		s.call(s.seller.AccessToken, "POST", "/products", map[string]interface{}{"name": strings.Repeat("я", 255), "description": strings.Repeat("a", 4000), "price": 0.01, "stock": 0, "category": strings.Repeat("x", 100), "status": "ACTIVE"}, 201, "", nil)
	})
	t.Run("create_errors_and_transaction_rollback", func(t *testing.T) {
		s.t = t
		u := s.account("USER")
		p := s.product(100, 5, "ACTIVE")
		inactive := s.product(20, 4, "INACTIVE")
		s.call(u.AccessToken, "POST", "/orders", items(9223372036854775807, 1), 404, "PRODUCT_NOT_FOUND", nil)
		s.call(u.AccessToken, "POST", "/orders", items(inactive.Id, 1), 409, "PRODUCT_INACTIVE", nil)
		s.call(u.AccessToken, "POST", "/orders", items(p.Id, 6), 409, "INSUFFICIENT_STOCK", nil)
		body := items(p.Id, 1)
		body["promo_code"] = "MISSING_PROMO"
		s.call(u.AccessToken, "POST", "/orders", body, 422, "PROMO_CODE_INVALID", nil)
		s.stock(p.Id, 5)
		min := s.promo("PERCENTAGE", 10, 500, 1, true, false)
		body["promo_code"] = min.Code
		s.call(u.AccessToken, "POST", "/orders", body, 422, "PROMO_CODE_MIN_AMOUNT", nil)
		s.stock(p.Id, 5)
		s.uses(min.Id, 0)
		for _, promo := range []api.PromoCodeResponse{s.promo("PERCENTAGE", 10, 0, 1, false, false), s.promo("PERCENTAGE", 10, 0, 1, true, true)} {
			body["promo_code"] = promo.Code
			s.call(u.AccessToken, "POST", "/orders", body, 422, "PROMO_CODE_INVALID", nil)
			s.stock(p.Id, 5)
		}
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM user_operations WHERE user_id=$1`, u.User.Id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatal("failed orders recorded operations")
		}
		o := s.order(u, p, 2, "")
		s.stock(p.Id, 3)
		s.call(u.AccessToken, "POST", "/orders", items(p.Id, 1), 429, "ORDER_LIMIT_EXCEEDED", nil)
		s.age(u.User.Id, "CREATE_ORDER")
		s.call(u.AccessToken, "POST", "/orders", items(p.Id, 1), 409, "ORDER_HAS_ACTIVE", nil)
		stranger := s.account("USER")
		s.call(stranger.AccessToken, "GET", orderPath(o.Id), nil, 403, "ORDER_OWNERSHIP_VIOLATION", nil)
		s.call(stranger.AccessToken, "PUT", orderPath(o.Id), items(p.Id, 1), 403, "ORDER_OWNERSHIP_VIOLATION", nil)
		s.call(stranger.AccessToken, "POST", orderPath(o.Id)+"/cancel", nil, 403, "ORDER_OWNERSHIP_VIOLATION", nil)
		s.call(s.seller.AccessToken, "GET", orderPath(o.Id), nil, 403, "ACCESS_DENIED", nil)
		s.call(s.seller.AccessToken, "POST", "/orders", items(p.Id, 1), 403, "ACCESS_DENIED", nil)
		s.call(s.admin.AccessToken, "GET", orderPath(o.Id), nil, 200, "", nil)
		s.call(u.AccessToken, "GET", "/orders/9223372036854775807", nil, 404, "ORDER_NOT_FOUND", nil)
	})
	t.Run("snapshot_update_rollback_and_promo_removal", func(t *testing.T) {
		s.t = t
		u := s.account("USER")
		p := s.product(100, 10, "ACTIVE")
		promo := s.promo("PERCENTAGE", 10, 150, 1, true, false)
		o := s.order(u, p, 2, promo.Code)
		s.stock(p.Id, 8)
		s.uses(promo.Id, 1)
		if o.TotalAmount.String() != "180" || o.DiscountAmount.String() != "20" {
			t.Fatalf("incorrect discount %+v", o)
		}
		// Updating the product price must not alter an existing order snapshot.
		if _, err := db.Exec(`UPDATE products SET price=120 WHERE id=$1`, p.Id); err != nil {
			t.Fatal(err)
		}
		var read api.OrderResponse
		s.call(u.AccessToken, "GET", orderPath(o.Id), nil, 200, "", &read)
		if read.Items[0].PriceAtOrder.String() != "100" || read.TotalAmount.String() != "180" {
			t.Fatal("snapshot changed")
		}
		s.call(u.AccessToken, "PUT", orderPath(o.Id), items(p.Id, 11), 409, "INSUFFICIENT_STOCK", nil)
		s.stock(p.Id, 8)
		s.uses(promo.Id, 1)
		s.call(u.AccessToken, "GET", orderPath(o.Id), nil, 200, "", &read)
		if read.Items[0].Quantity != 2 || read.TotalAmount.String() != "180" {
			t.Fatal("failed update was not rolled back")
		}
		s.call(u.AccessToken, "PUT", orderPath(o.Id), items(p.Id, 3), 200, "", &read)
		s.stock(p.Id, 7)
		s.uses(promo.Id, 1)
		if read.PromoCodeId == nil || read.TotalAmount.String() != "324" {
			t.Fatal("already applied promo was lost at its usage limit")
		}
		s.call(u.AccessToken, "PUT", orderPath(o.Id), items(p.Id, 1), 429, "ORDER_LIMIT_EXCEEDED", nil)
		s.age(u.User.Id, "UPDATE_ORDER")
		s.call(u.AccessToken, "PUT", orderPath(o.Id), items(p.Id, 1), 200, "", &read)
		s.stock(p.Id, 9)
		s.uses(promo.Id, 0)
		if read.PromoCodeId != nil || !read.DiscountAmount.IsZero() || read.TotalAmount.String() != "120" {
			t.Fatal("promo was not removed below minimum")
		}
		s.call(u.AccessToken, "POST", orderPath(o.Id)+"/cancel", nil, 200, "", &read)
		s.stock(p.Id, 10)
		s.uses(promo.Id, 0)
		s.call(u.AccessToken, "POST", orderPath(o.Id)+"/cancel", nil, 409, "INVALID_STATE_TRANSITION", nil)
		s.stock(p.Id, 10)
	})
	t.Run("cancel_returns_promo_and_state_machine", func(t *testing.T) {
		s.t = t
		u := s.account("USER")
		p := s.product(100, 10, "ACTIVE")
		promo := s.promo("FIXED_AMOUNT", 500, 0, 1, true, false)
		o := s.order(u, p, 2, promo.Code)
		if !o.TotalAmount.IsZero() || o.DiscountAmount.String() != "200" {
			t.Fatal("fixed discount exceeded subtotal")
		}
		s.call(s.admin.AccessToken, "POST", orderPath(o.Id)+"/status", map[string]string{"status": "PAYMENT_PENDING"}, 200, "", nil)
		s.call(u.AccessToken, "PUT", orderPath(o.Id), items(p.Id, 1), 409, "INVALID_STATE_TRANSITION", nil)
		s.call(u.AccessToken, "POST", orderPath(o.Id)+"/cancel", nil, 200, "", nil)
		s.stock(p.Id, 10)
		s.uses(promo.Id, 0)
		u2 := s.account("USER")
		o2 := s.order(u2, p, 1, "")
		s.call(u2.AccessToken, "POST", orderPath(o2.Id)+"/status", map[string]string{"status": "PAYMENT_PENDING"}, 403, "ACCESS_DENIED", nil)
		s.call(s.admin.AccessToken, "POST", orderPath(o2.Id)+"/status", map[string]string{"status": "SHIPPED"}, 409, "INVALID_STATE_TRANSITION", nil)
		for _, status := range []string{"PAYMENT_PENDING", "PAID", "SHIPPED", "COMPLETED"} {
			s.call(s.admin.AccessToken, "POST", orderPath(o2.Id)+"/status", map[string]string{"status": status}, 200, "", nil)
			if status == "PAID" {
				s.call(u2.AccessToken, "POST", orderPath(o2.Id)+"/cancel", nil, 409, "INVALID_STATE_TRANSITION", nil)
			}
		}
		s.call(s.admin.AccessToken, "POST", orderPath(o2.Id)+"/status", map[string]string{"status": "CREATED"}, 409, "INVALID_STATE_TRANSITION", nil)
		s.stock(p.Id, 9)
	})
	t.Run("concurrent_stock_reservation", func(t *testing.T) {
		s.t = t
		p := s.product(1, 1, "ACTIVE")
		users := []api.AuthResponse{s.account("USER"), s.account("USER")}
		type result struct {
			req *http.Request
			rec *httptest.ResponseRecorder
		}
		results := make(chan result, 2)
		var wg sync.WaitGroup
		for _, user := range users {
			wg.Add(1)
			go func(token string) {
				defer wg.Done()
				req, rec := s.raw(token, "POST", "/orders", items(p.Id, 1))
				results <- result{req, rec}
			}(user.AccessToken)
		}
		wg.Wait()
		close(results)
		success, conflict := 0, 0
		for result := range results {
			if result.rec.Code == 201 {
				success++
				s.check(result.req, result.rec, 201, "", nil)
			} else {
				conflict++
				s.check(result.req, result.rec, 409, "INSUFFICIENT_STOCK", nil)
			}
		}
		if success != 1 || conflict != 1 {
			t.Fatalf("success=%d conflict=%d", success, conflict)
		}
		s.stock(p.Id, 0)
	})
	t.Run("concurrent_same_user_and_promo_limit", func(t *testing.T) {
		s.t = t
		p := s.product(10, 10, "ACTIVE")
		u := s.account("USER")
		type result struct {
			req *http.Request
			rec *httptest.ResponseRecorder
		}
		results := make(chan result, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, rec := s.raw(u.AccessToken, "POST", "/orders", items(p.Id, 1))
				results <- result{req, rec}
			}()
		}
		wg.Wait()
		close(results)
		success := 0
		for result := range results {
			if result.rec.Code == 201 {
				success++
				s.check(result.req, result.rec, 201, "", nil)
			} else {
				s.check(result.req, result.rec, 429, "ORDER_LIMIT_EXCEEDED", nil)
			}
		}
		if success != 1 {
			t.Fatal("more than one order created")
		}
		s.stock(p.Id, 9)
		promo := s.promo("PERCENTAGE", 99, 0, 1, true, false)
		u1, u2 := s.account("USER"), s.account("USER")
		results = make(chan result, 2)
		for _, user := range []api.AuthResponse{u1, u2} {
			wg.Add(1)
			go func(token string) {
				defer wg.Done()
				body := items(p.Id, 1)
				body["promo_code"] = promo.Code
				req, rec := s.raw(token, "POST", "/orders", body)
				results <- result{req, rec}
			}(user.AccessToken)
		}
		wg.Wait()
		close(results)
		success = 0
		for result := range results {
			if result.rec.Code == 201 {
				success++
				var order api.OrderResponse
				s.check(result.req, result.rec, 201, "", &order)
				if order.DiscountAmount.String() != "7" {
					t.Fatal("percentage discount was not capped at 70%")
				}
			} else {
				s.check(result.req, result.rec, 422, "PROMO_CODE_INVALID", nil)
			}
		}
		if success != 1 {
			t.Fatal("promo limit exceeded")
		}
		s.uses(promo.Id, 1)
		s.stock(p.Id, 8)
	})
}
