package app

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/shopspring/decimal"
	contract "marketplace/api"
	"marketplace/internal/api"
	"marketplace/internal/config"
)

type App struct {
	db     *sql.DB
	cfg    config.Config
	log    *slog.Logger
	router routers.Router
	now    func() time.Time
}

func New(db *sql.DB, cfg config.Config, logger *slog.Logger) (*App, error) {
	decimal.MarshalJSONWithoutQuotes = true
	spec, err := api.GetSwagger()
	if err != nil {
		return nil, err
	}
	if err = spec.Validate(context.Background()); err != nil {
		return nil, err
	}
	spec.Servers = nil // Match routes independently of the deployment host.
	router, err := legacy.NewRouter(spec)
	if err != nil {
		return nil, err
	}
	return &App{db: db, cfg: cfg, log: logger, router: router, now: time.Now}, nil
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	api.HandlerWithOptions(a, api.StdHTTPServerOptions{BaseRouter: mux, ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
		a.fail(w, r, invalid("parameters", err.Error()))
	}})
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		_, _ = w.Write(contract.Specification)
	})
	return a.logging(a.authenticate(a.validate(mux)))
}

func (a *App) Health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.db.PingContext(ctx); err != nil {
		writeJSON(w, 503, api.ErrorResponse{ErrorCode: "INTERNAL_ERROR", Message: "База данных недоступна"})
		return
	}
	writeJSON(w, 200, api.HealthResponse{Status: "ok"})
}

// tx executes all business mutations atomically. Rollback also runs on panic.
func (a *App) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

type querier interface {
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

type scanner interface{ Scan(...interface{}) error }

// Compile-time check: the implementation uses the generated contract handlers.
var _ api.ServerInterface = (*App)(nil)
