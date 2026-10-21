package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"marketplace/internal/api"
)

func public(path string) bool {
	switch path {
	case "/health", "/openapi.yaml", "/auth/register", "/auth/login", "/auth/refresh":
		return true
	}
	return false
}
func (a *App) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if public(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			a.fail(w, r, problem(401, "TOKEN_INVALID", "Требуется access token"))
			return
		}
		c, err := a.parseToken(parts[1], "access")
		if err != nil {
			code := api.TOKENINVALID
			if errors.Is(err, jwt.ErrTokenExpired) {
				code = api.TOKENEXPIRED
			}
			a.fail(w, r, problem(401, code, "Access token недействителен"))
			return
		}
		id, _ := strconv.ParseInt(c.Subject, 10, 64)
		if info, ok := r.Context().Value(requestInfoKey{}).(*requestInfo); ok {
			info.userID = id
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{ID: id, Role: c.Role})))
	})
}

func (a *App) validate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi.yaml" {
			next.ServeHTTP(w, r)
			return
		}
		route, params, err := a.router.FindRoute(r)
		if err != nil {
			a.fail(w, r, problem(404, "RESOURCE_NOT_FOUND", "Маршрут не найден"))
			return
		}
		input := &openapi3filter.RequestValidationInput{Request: r, PathParams: params, Route: route, Options: &openapi3filter.Options{MultiError: true, SchemaValidationOptions: []openapi3.SchemaValidationOption{
			openapi3.DisableSchemaErrorDetails(),
			openapi3.WithStringFormatValidator("email", openapi3.NewCallbackValidator(func(value string) error {
				address, err := mail.ParseAddress(value)
				if err != nil || address.Address != value {
					return errors.New("invalid email address")
				}
				return nil
			})),
			openapi3.WithStringFormatValidator("date-time", openapi3.NewCallbackValidator(func(value string) error {
				if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
					return errors.New("invalid RFC3339 date-time")
				}
				return nil
			})),
		}, AuthenticationFunc: func(ctx context.Context, _ *openapi3filter.AuthenticationInput) error {
			if p, _ := ctx.Value(principalKey{}).(principal); p.ID > 0 {
				return nil
			}
			return errors.New("authentication required")
		}}}
		if err = openapi3filter.ValidateRequest(r.Context(), input); err != nil {
			details := map[string]interface{}{}
			validationDetails(err, "body", details)
			a.fail(w, r, &apiError{status: 400, code: "VALIDATION_ERROR", message: "Нарушены ограничения OpenAPI", details: details})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func validationDetails(err error, field string, out map[string]interface{}) {
	switch e := err.(type) {
	case openapi3.MultiError:
		for _, child := range e {
			validationDetails(child, field, out)
		}
	case *openapi3filter.RequestError:
		if e.Parameter != nil {
			field = e.Parameter.Name
		}
		if e.Err != nil {
			validationDetails(e.Err, field, out)
		} else {
			addValidationDetail(out, field, e.Reason)
		}
	case *openapi3.SchemaError:
		if path := strings.Join(e.JSONPointer(), "."); path != "" {
			field = path
		}
		addValidationDetail(out, field, e.Reason)
	case interface{ Unwrap() []error }:
		for _, child := range e.Unwrap() {
			validationDetails(child, field, out)
		}
	case interface{ Unwrap() error }:
		if child := e.Unwrap(); child != nil {
			validationDetails(child, field, out)
		} else {
			addValidationDetail(out, field, "Некорректное значение")
		}
	default:
		addValidationDetail(out, field, "Некорректное значение или формат")
	}
}

func addValidationDetail(out map[string]interface{}, field, message string) {
	previous, _ := out[field].([]string)
	out[field] = append(previous, message)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type requestInfo struct{ userID interface{} }
type requestInfoKey struct{}

func (a *App) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := uuid.NewString()
		w.Header().Set("X-Request-Id", requestID)
		sw := &statusWriter{ResponseWriter: w}
		info := &requestInfo{}
		r = r.WithContext(context.WithValue(r.Context(), requestInfoKey{}, info))
		var body interface{}
		defer func() {
			if recovered := recover(); recovered != nil {
				a.log.Error("panic", "request_id", requestID)
				if sw.status == 0 {
					a.fail(sw, r, problem(500, "INTERNAL_ERROR", "Внутренняя ошибка сервера"))
				}
			}
			status := sw.status
			if status == 0 {
				status = 200
			}
			attrs := []interface{}{"request_id", requestID, "method", r.Method, "endpoint", r.URL.Path, "status_code", status, "duration_ms", float64(time.Since(start).Microseconds()) / 1000, "user_id", info.userID, "timestamp", start.UTC().Format(time.RFC3339Nano)}
			if r.Method == "POST" || r.Method == "PUT" || r.Method == "DELETE" {
				attrs = append(attrs, "request_body", body)
			}
			a.log.Info("api_request", attrs...)
		}()
		if r.Body != nil {
			raw, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
			_ = r.Body.Close()
			if err != nil || len(raw) > 1<<20 {
				body = "[body rejected]"
				a.fail(sw, r, invalid("body", "Тело запроса превышает 1 MiB или не может быть прочитано"))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			if len(raw) > 0 {
				if json.Unmarshal(raw, &body) == nil {
					body = redact(body)
				} else {
					body = "[invalid JSON]"
					a.fail(sw, r, invalid("body", "Тело должно содержать один корректный JSON-документ"))
					return
				}
			}
		}
		next.ServeHTTP(sw, r)
	})
}
func redact(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		for k, item := range v {
			key := strings.ToLower(k)
			if strings.Contains(key, "password") || strings.Contains(key, "token") || strings.Contains(key, "secret") {
				v[k] = "[REDACTED]"
			} else {
				v[k] = redact(item)
			}
		}
	case []interface{}:
		for i, item := range v {
			v[i] = redact(item)
		}
	}
	return value
}
