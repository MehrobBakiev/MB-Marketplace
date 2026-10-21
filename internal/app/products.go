package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"marketplace/internal/api"
)

const productColumns = `id,name,description,price,stock,category,status,seller_id,created_at,updated_at`

func scanProduct(row scanner) (api.ProductResponse, error) {
	var p api.ProductResponse
	err := row.Scan(&p.Id, &p.Name, &p.Description, &p.Price, &p.Stock, &p.Category, &p.Status, &p.SellerId, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}
func productByID(ctx context.Context, q querier, id int64, lock bool) (api.ProductResponse, error) {
	query := `SELECT ` + productColumns + ` FROM products WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	p, err := scanProduct(q.QueryRowContext(ctx, query, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = problem(404, "PRODUCT_NOT_FOUND", "Товар не найден")
	}
	return p, err
}
func (a *App) GetProduct(w http.ResponseWriter, r *http.Request, id int64) {
	p, err := productByID(r.Context(), a.db, id, false)
	a.respond(w, r, 200, p, err)
}
func (a *App) ListProducts(w http.ResponseWriter, r *http.Request, params api.ListProductsParams) {
	page, size := int64(0), int64(20)
	if params.Page != nil {
		page = *params.Page
	}
	if params.Size != nil {
		size = *params.Size
	}
	out := api.ProductPage{Items: []api.ProductResponse{}, Page: page, Size: size}
	tx, err := a.db.BeginTx(r.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer tx.Rollback()
	const filter = ` FROM products WHERE ($1::text IS NULL OR status=$1) AND ($2::text IS NULL OR category=$2)`
	err = tx.QueryRowContext(r.Context(), `SELECT count(*)`+filter, params.Status, params.Category).Scan(&out.TotalElements)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	rows, err := tx.QueryContext(r.Context(), `SELECT `+productColumns+filter+` ORDER BY id LIMIT $3 OFFSET $4`, params.Status, params.Category, size, page*size)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		p, scanErr := scanProduct(rows)
		if scanErr != nil {
			a.fail(w, r, scanErr)
			return
		}
		out.Items = append(out.Items, p)
	}
	if err = rows.Err(); err == nil {
		err = tx.Commit()
	}
	a.respond(w, r, 200, out, err)
}
func (a *App) CreateProduct(w http.ResponseWriter, r *http.Request) {
	if err := permitted(r, api.RoleSELLER, api.RoleADMIN); err != nil {
		a.fail(w, r, err)
		return
	}
	var in api.ProductCreate
	if err := decode(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	owner := current(r).ID
	if in.SellerId != nil {
		if current(r).Role != api.RoleADMIN && *in.SellerId != owner {
			a.fail(w, r, problem(403, "ACCESS_DENIED", "Продавец может создавать только свои товары"))
			return
		}
		owner = *in.SellerId
	}
	var exists bool
	err := a.db.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND role IN ('SELLER','ADMIN'))`, owner).Scan(&exists)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if !exists {
		a.fail(w, r, invalid("seller_id", "Владелец должен быть существующим продавцом или администратором"))
		return
	}
	p, err := scanProduct(a.db.QueryRowContext(r.Context(), `INSERT INTO products(name,description,price,stock,category,status,seller_id) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+productColumns, in.Name, in.Description, in.Price, in.Stock, in.Category, in.Status, owner))
	a.respond(w, r, 201, p, err)
}
func (a *App) UpdateProduct(w http.ResponseWriter, r *http.Request, id int64) {
	if err := permitted(r, api.RoleSELLER, api.RoleADMIN); err != nil {
		a.fail(w, r, err)
		return
	}
	var in api.ProductUpdate
	if err := decode(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	var out api.ProductResponse
	err := a.tx(r.Context(), func(tx *sql.Tx) error {
		p, err := productByID(r.Context(), tx, id, true)
		if err != nil {
			return err
		}
		if current(r).Role != api.RoleADMIN && p.SellerId != current(r).ID {
			return problem(403, "ACCESS_DENIED", "Продавец может изменять только свои товары")
		}
		out, err = scanProduct(tx.QueryRowContext(r.Context(), `UPDATE products SET name=$2,description=$3,price=$4,stock=$5,category=$6,status=$7 WHERE id=$1 RETURNING `+productColumns, id, in.Name, in.Description, in.Price, in.Stock, in.Category, in.Status))
		return err
	})
	a.respond(w, r, 200, out, err)
}
func (a *App) ArchiveProduct(w http.ResponseWriter, r *http.Request, id int64) {
	if err := permitted(r, api.RoleSELLER, api.RoleADMIN); err != nil {
		a.fail(w, r, err)
		return
	}
	err := a.tx(r.Context(), func(tx *sql.Tx) error {
		p, err := productByID(r.Context(), tx, id, true)
		if err != nil {
			return err
		}
		if current(r).Role != api.RoleADMIN && p.SellerId != current(r).ID {
			return problem(403, "ACCESS_DENIED", "Продавец может архивировать только свои товары")
		}
		_, err = tx.ExecContext(r.Context(), `UPDATE products SET status='ARCHIVED' WHERE id=$1 AND status<>'ARCHIVED'`, id)
		return err
	})
	a.respond(w, r, 204, nil, err)
}
