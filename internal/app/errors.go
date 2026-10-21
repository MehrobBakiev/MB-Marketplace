package app

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"
	"marketplace/internal/api"
)

type apiError struct {
	status  int
	code    api.ErrorCode
	message string
	details map[string]interface{}
}

func (e *apiError) Error() string { return e.message }
func problem(status int, code api.ErrorCode, message string) *apiError {
	return &apiError{status: status, code: code, message: message}
}
func invalid(field, message string) *apiError {
	return &apiError{status: 400, code: api.ErrorCode("VALIDATION_ERROR"), message: "Некорректные входные данные", details: map[string]interface{}{field: message}}
}
func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if data != nil {
		_ = json.NewEncoder(w).Encode(data)
	}
}
func (a *App) fail(w http.ResponseWriter, r *http.Request, err error) {
	var e *apiError
	if !errors.As(err, &e) {
		a.log.ErrorContext(r.Context(), "request failed", "request_id", w.Header().Get("X-Request-Id"), "error", err.Error())
		e = problem(500, "INTERNAL_ERROR", "Внутренняя ошибка сервера")
	}
	response := api.ErrorResponse{ErrorCode: e.code, Message: e.message}
	if e.details != nil {
		response.Details = &e.details
	}
	writeJSON(w, e.status, response)
}
func (a *App) respond(w http.ResponseWriter, r *http.Request, status int, data interface{}, err error) {
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, status, data)
}
func decode(r *http.Request, target interface{}) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return invalid("body", "Некорректный JSON")
	}
	return nil
}
