package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"marketplace/internal/api"
	"marketplace/internal/config"
)

func testConfig() config.Config {
	return config.Config{JWTSecret: strings.Repeat("s", 32), AccessTTL: 20 * time.Minute, RefreshTTL: 14 * 24 * time.Hour, CreateInterval: time.Minute, UpdateInterval: time.Minute}
}
func signedToken(t *testing.T, cfg config.Config, kind string, role api.Role, id int64, expiry time.Time) string {
	t.Helper()
	c := claims{Role: role, TokenType: kind, RegisteredClaims: jwt.RegisteredClaims{Issuer: "marketplace", Subject: decimal.NewFromInt(id).String(), Audience: jwt.ClaimStrings{"marketplace-" + kind}, ExpiresAt: jwt.NewNumericDate(expiry), IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)), ID: uuid.NewString()}}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(cfg.JWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestDiscountMoney(t *testing.T) {
	cases := []struct {
		kind               api.DiscountType
		value, total, want string
	}{
		{api.PERCENTAGE, "10", "0.30", "0.03"}, {api.PERCENTAGE, "99", "100", "70"},
		{api.PERCENTAGE, "12.5", "10.01", "1.25"}, {api.FIXEDAMOUNT, "500", "125.12", "125.12"},
		{api.FIXEDAMOUNT, "10.11", "100", "10.11"},
	}
	for _, tc := range cases {
		got := discountFor(api.PromoCodeResponse{DiscountType: tc.kind, DiscountValue: decimal.RequireFromString(tc.value)}, decimal.RequireFromString(tc.total))
		if !got.Equal(decimal.RequireFromString(tc.want)) {
			t.Errorf("%s %s of %s: got %s, want %s", tc.kind, tc.value, tc.total, got, tc.want)
		}
	}
}
func TestOrderStateMachine(t *testing.T) {
	statuses := []api.OrderStatus{api.CREATED, api.PAYMENTPENDING, api.PAID, api.SHIPPED, api.COMPLETED, api.CANCELED}
	for i, from := range statuses {
		for j, to := range statuses {
			want := i < 4 && j == i+1
			if got := canTransition(from, to); got != want {
				t.Errorf("%s -> %s: got %v", from, to, got)
			}
		}
	}
}
func TestPromoAtUsageLimitCanStayOnExistingOrder(t *testing.T) {
	now := time.Now()
	p := api.PromoCodeResponse{Active: true, CurrentUses: 1, MaxUses: 1, ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)}
	if promoUsable(p, now, false) {
		t.Fatal("exhausted promo was accepted for a new order")
	}
	if !promoUsable(p, now, true) {
		t.Fatal("existing promo was removed solely because its own use reached the limit")
	}
	p.Active = false
	if promoUsable(p, now, true) {
		t.Fatal("inactive promo was accepted")
	}
}
func TestTokenSeparationAndExpiry(t *testing.T) {
	cfg := testConfig()
	a := &App{cfg: cfg, now: time.Now}
	access := signedToken(t, cfg, "access", api.RoleUSER, 1, time.Now().Add(time.Minute))
	if _, err := a.parseToken(access, "access"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.parseToken(access, "refresh"); err == nil {
		t.Fatal("access accepted as refresh")
	}
	expired := signedToken(t, cfg, "access", api.RoleUSER, 1, time.Now().Add(-time.Second))
	if _, err := a.parseToken(expired, "access"); err == nil {
		t.Fatal("expired access accepted")
	}
	if _, err := a.parseToken(access+"broken", "access"); err == nil {
		t.Fatal("tampered signature accepted")
	}
}
func TestContractValidationBeforeBusinessLogic(t *testing.T) {
	// No database is provided: every case must be rejected by the contract middleware.
	cfg := testConfig()
	a, err := New(nil, cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	handler := a.Handler()
	token := signedToken(t, cfg, "access", api.RoleADMIN, 1, time.Now().Add(time.Minute))
	valid := map[string]interface{}{"name": "Товар", "price": 12.50, "stock": 1, "category": "Books", "status": "ACTIVE"}
	type validationCase struct {
		name, method, path string
		body               interface{}
	}
	cases := []validationCase{}
	for _, bad := range []struct {
		field string
		value interface{}
	}{
		{"name", ""}, {"name", strings.Repeat("a", 256)}, {"price", 0}, {"price", -1}, {"price", 0.001}, {"price", 1.001}, {"price", "12.50"}, {"stock", -1}, {"stock", 1.5}, {"category", ""}, {"category", strings.Repeat("a", 101)}, {"status", "UNKNOWN"}, {"description", strings.Repeat("a", 4001)},
	} {
		m := map[string]interface{}{}
		for k, v := range valid {
			m[k] = v
		}
		m[bad.field] = bad.value
		cases = append(cases, validationCase{fmt.Sprintf("%s_case_%d", bad.field, len(cases)), "POST", "/products", m})
	}
	cases = append(cases,
		validationCase{"missing_required", "POST", "/products", map[string]interface{}{"name": "x"}},
		validationCase{"extra_field", "POST", "/products", map[string]interface{}{"unexpected": true}},
		validationCase{"invalid_page", "GET", "/products?page=-1", nil},
		validationCase{"invalid_size", "GET", "/products?size=0", nil},
		validationCase{"invalid_id", "GET", "/products/0", nil},
		validationCase{"empty_order", "POST", "/orders", map[string]interface{}{"items": []interface{}{}}},
		validationCase{"quantity_zero", "POST", "/orders", map[string]interface{}{"items": []interface{}{map[string]interface{}{"product_id": 1, "quantity": 0}}}},
		validationCase{"quantity_1000", "POST", "/orders", map[string]interface{}{"items": []interface{}{map[string]interface{}{"product_id": 1, "quantity": 1000}}}},
		validationCase{"bad_promo", "POST", "/orders", map[string]interface{}{"items": []interface{}{map[string]interface{}{"product_id": 1, "quantity": 1}}, "promo_code": "lower"}},
		validationCase{"bad_email", "POST", "/auth/register", map[string]interface{}{"email": "no-email", "password": "password123"}},
		validationCase{"register_admin", "POST", "/auth/register", map[string]interface{}{"email": "test@example.com", "password": "password123", "role": "ADMIN"}},
	)
	tooMany := []api.OrderItemInput{}
	for i := int64(1); i <= 51; i++ {
		tooMany = append(tooMany, api.OrderItemInput{ProductId: i, Quantity: 1})
	}
	cases = append(cases, validationCase{"51_items", "POST", "/orders", map[string]interface{}{"items": tooMany}})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(data))
			if tc.body == nil {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			} else {
				req.Header.Set("Content-Type", "application/json")
			}
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != 400 {
				t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
			}
			var out api.ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out.ErrorCode != api.VALIDATIONERROR || out.Details == nil || len(*out.Details) == 0 {
				t.Fatalf("invalid error contract: %s", rec.Body.String())
			}
			if _, err := uuid.Parse(rec.Header().Get("X-Request-Id")); err != nil {
				t.Fatal("missing request ID")
			}
		})
	}
}
func TestLoggingRedactsSecrets(t *testing.T) {
	var logs bytes.Buffer
	cfg := testConfig()
	a, err := New(nil, cfg, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	handler := a.logging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) }))
	req := httptest.NewRequest("POST", "/test", strings.NewReader(`{"password":"secret-pass","nested":{"refresh_token":"secret-token"},"items":[{"password":"other-secret"}],"name":"visible"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	for _, secret := range []string{"secret-pass", "secret-token", "other-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("secret leaked: %s", secret)
		}
	}
	if !strings.Contains(logs.String(), "visible") {
		t.Fatal("non-sensitive fields were lost")
	}
	var event map[string]interface{}
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"request_id", "method", "endpoint", "status_code", "duration_ms", "user_id", "timestamp", "request_body"} {
		if _, ok := event[field]; !ok {
			t.Errorf("missing %s", field)
		}
	}
}

func TestValidationCollectsAllMissingFields(t *testing.T) {
	cfg := testConfig()
	a, err := New(nil, cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/products", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+signedToken(t, cfg, "access", api.RoleADMIN, 1, time.Now().Add(time.Minute)))
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	for _, field := range []string{"name", "price", "stock", "category", "status"} {
		if !strings.Contains(rec.Body.String(), field) {
			t.Errorf("missing violation for %s: %s", field, rec.Body.String())
		}
	}
}

func TestMalformedBodyNeverReachesBusinessLogic(t *testing.T) {
	cfg := testConfig()
	a, err := New(nil, cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"name":"x"} {}`, `{"password":"secret",`, strings.Repeat(" ", 1<<20) + "{}"} {
		req := httptest.NewRequest("POST", "/products", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+signedToken(t, cfg, "access", api.RoleADMIN, 1, time.Now().Add(time.Minute)))
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
		}
	}
}
