package app

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/shopspring/decimal"
	"marketplace/internal/api"
)

const orderColumns = `id,user_id,status,promo_code_id,total_amount,discount_amount,created_at,updated_at`

var maxMoney = decimal.RequireFromString("9999999999.99")

func readOrder(ctx context.Context, q querier, id int64, lock bool) (api.OrderResponse, error) {
	var o api.OrderResponse
	query := `SELECT ` + orderColumns + ` FROM orders WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	err := q.QueryRowContext(ctx, query, id).Scan(&o.Id, &o.UserId, &o.Status, &o.PromoCodeId, &o.TotalAmount, &o.DiscountAmount, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return o, problem(404, "ORDER_NOT_FOUND", "Заказ не найден")
	}
	if err != nil {
		return o, err
	}
	rows, err := q.QueryContext(ctx, `SELECT id,product_id,quantity,price_at_order FROM order_items WHERE order_id=$1 ORDER BY id`, id)
	if err != nil {
		return o, err
	}
	defer rows.Close()
	o.Items = []api.OrderItemResponse{}
	for rows.Next() {
		var item api.OrderItemResponse
		if err = rows.Scan(&item.Id, &item.ProductId, &item.Quantity, &item.PriceAtOrder); err != nil {
			return o, err
		}
		o.Items = append(o.Items, item)
	}
	return o, rows.Err()
}
func owns(p principal, o api.OrderResponse) error {
	if p.Role != api.RoleADMIN && p.ID != o.UserId {
		return problem(403, "ORDER_OWNERSHIP_VIOLATION", "Заказ принадлежит другому пользователю")
	}
	return nil
}
func validateItems(items []api.OrderItemInput) error {
	seen := map[int64]bool{}
	for _, item := range items {
		if seen[item.ProductId] {
			return invalid("items", "product_id не должен повторяться")
		}
		seen[item.ProductId] = true
	}
	return nil
}
func lockUser(ctx context.Context, tx *sql.Tx, id int64) error {
	var found int64
	return tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, id).Scan(&found)
}

// All order mutations lock the owner before the order, including admin actions.
func lockOrder(ctx context.Context, tx *sql.Tx, id int64, p principal) (api.OrderResponse, error) {
	var owner int64
	err := tx.QueryRowContext(ctx, `SELECT user_id FROM orders WHERE id=$1`, id).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return api.OrderResponse{}, problem(404, "ORDER_NOT_FOUND", "Заказ не найден")
	}
	if err != nil {
		return api.OrderResponse{}, err
	}
	if p.Role != api.RoleADMIN && p.ID != owner {
		return api.OrderResponse{}, problem(403, "ORDER_OWNERSHIP_VIOLATION", "Заказ принадлежит другому пользователю")
	}
	if err = lockUser(ctx, tx, owner); err != nil {
		return api.OrderResponse{}, err
	}
	return readOrder(ctx, tx, id, true)
}
func (a *App) rateLimit(ctx context.Context, tx *sql.Tx, userID int64, operation string, interval time.Duration) error {
	var last time.Time
	err := tx.QueryRowContext(ctx, `SELECT created_at FROM user_operations WHERE user_id=$1 AND operation_type=$2 ORDER BY created_at DESC LIMIT 1`, userID, operation).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	remaining := interval - a.now().Sub(last)
	if remaining > 0 {
		return &apiError{status: 429, code: "ORDER_LIMIT_EXCEEDED", message: "Превышена частота операций с заказами", details: map[string]interface{}{"retry_after_seconds": int64(math.Ceil(remaining.Seconds()))}}
	}
	return nil
}
func lockProducts(ctx context.Context, tx *sql.Tx, items []api.OrderItemInput, old []api.OrderItemResponse) (map[int64]api.ProductResponse, error) {
	ids := map[int64]bool{}
	for _, item := range items {
		ids[item.ProductId] = true
	}
	for _, item := range old {
		ids[item.ProductId] = true
	}
	sorted := make([]int64, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	products := make(map[int64]api.ProductResponse, len(ids))
	for _, id := range sorted {
		p, err := productByID(ctx, tx, id, true)
		if err != nil {
			return nil, err
		}
		products[id] = p
	}
	return products, nil
}
func reserve(ctx context.Context, tx *sql.Tx, items []api.OrderItemInput, products map[int64]api.ProductResponse) (decimal.Decimal, error) {
	for _, item := range items {
		if products[item.ProductId].Status != api.ACTIVE {
			return decimal.Zero, problem(409, "PRODUCT_INACTIVE", "Нельзя заказать неактивный товар")
		}
	}
	missing := []map[string]interface{}{}
	for _, item := range items {
		p := products[item.ProductId]
		if p.Stock < item.Quantity {
			missing = append(missing, map[string]interface{}{"product_id": p.Id, "requested_quantity": item.Quantity, "available_stock": p.Stock})
		}
	}
	if len(missing) > 0 {
		return decimal.Zero, &apiError{status: 409, code: "INSUFFICIENT_STOCK", message: "Недостаточно товара на складе", details: map[string]interface{}{"products": missing}}
	}
	subtotal := decimal.Zero
	for _, item := range items {
		subtotal = subtotal.Add(products[item.ProductId].Price.Mul(decimal.NewFromInt(item.Quantity)))
	}
	if subtotal.GreaterThan(maxMoney) {
		return decimal.Zero, invalid("items", "Сумма заказа превышает допустимую точность DECIMAL(12,2)")
	}
	for _, item := range items {
		if _, err := tx.ExecContext(ctx, `UPDATE products SET stock=stock-$2 WHERE id=$1`, item.ProductId, item.Quantity); err != nil {
			return decimal.Zero, err
		}
	}
	return subtotal, nil
}
func restore(ctx context.Context, tx *sql.Tx, items []api.OrderItemResponse, products map[int64]api.ProductResponse) error {
	for _, item := range items {
		if _, err := tx.ExecContext(ctx, `UPDATE products SET stock=stock+$2 WHERE id=$1`, item.ProductId, item.Quantity); err != nil {
			return err
		}
		p := products[item.ProductId]
		p.Stock += item.Quantity
		products[item.ProductId] = p
	}
	return nil
}
func saveItems(ctx context.Context, tx *sql.Tx, orderID int64, items []api.OrderItemInput, products map[int64]api.ProductResponse) error {
	for _, item := range items {
		if _, err := tx.ExecContext(ctx, `INSERT INTO order_items(order_id,product_id,quantity,price_at_order) VALUES($1,$2,$3,$4)`, orderID, item.ProductId, item.Quantity, products[item.ProductId].Price); err != nil {
			return err
		}
	}
	return nil
}
func recordOperation(ctx context.Context, tx *sql.Tx, userID int64, kind string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO user_operations(user_id,operation_type,created_at) VALUES($1,$2,$3)`, userID, kind, now)
	return err
}

func (a *App) CreateOrder(w http.ResponseWriter, r *http.Request) {
	if err := permitted(r, api.RoleUSER, api.RoleADMIN); err != nil {
		a.fail(w, r, err)
		return
	}
	var in api.OrderCreate
	if err := decode(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	if err := validateItems(in.Items); err != nil {
		a.fail(w, r, err)
		return
	}
	var out api.OrderResponse
	err := a.tx(r.Context(), func(tx *sql.Tx) error {
		ctx := r.Context()
		userID := current(r).ID
		if err := lockUser(ctx, tx, userID); err != nil {
			return err
		}
		if err := a.rateLimit(ctx, tx, userID, "CREATE_ORDER", a.cfg.CreateInterval); err != nil {
			return err
		}
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM orders WHERE user_id=$1 AND status IN ('CREATED','PAYMENT_PENDING'))`, userID).Scan(&active); err != nil {
			return err
		}
		if active {
			return problem(409, "ORDER_HAS_ACTIVE", "У пользователя уже есть активный заказ")
		}
		products, err := lockProducts(ctx, tx, in.Items, nil)
		if err != nil {
			return err
		}
		subtotal, err := reserve(ctx, tx, in.Items, products)
		if err != nil {
			return err
		}
		var promoID *int64
		discount := decimal.Zero
		if in.PromoCode != nil {
			p, value, err := applyPromo(ctx, tx, *in.PromoCode, subtotal, a.now())
			if err != nil {
				return err
			}
			promoID = &p.Id
			discount = value
		}
		var id int64
		err = tx.QueryRowContext(ctx, `INSERT INTO orders(user_id,status,promo_code_id,total_amount,discount_amount) VALUES($1,'CREATED',$2,$3,$4) RETURNING id`, userID, promoID, subtotal.Sub(discount), discount).Scan(&id)
		if isUnique(err) {
			return problem(409, "ORDER_HAS_ACTIVE", "У пользователя уже есть активный заказ")
		}
		if err != nil {
			return err
		}
		if err = saveItems(ctx, tx, id, in.Items, products); err != nil {
			return err
		}
		if err = recordOperation(ctx, tx, userID, "CREATE_ORDER", a.now()); err != nil {
			return err
		}
		out, err = readOrder(ctx, tx, id, false)
		return err
	})
	a.respond(w, r, 201, out, err)
}
func (a *App) GetOrder(w http.ResponseWriter, r *http.Request, id int64) {
	if err := permitted(r, api.RoleUSER, api.RoleADMIN); err != nil {
		a.fail(w, r, err)
		return
	}
	tx, err := a.db.BeginTx(r.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer tx.Rollback()
	out, err := readOrder(r.Context(), tx, id, false)
	if err == nil {
		err = owns(current(r), out)
	}
	if err == nil {
		err = tx.Commit()
	}
	a.respond(w, r, 200, out, err)
}
func (a *App) UpdateOrder(w http.ResponseWriter, r *http.Request, id int64) {
	if err := permitted(r, api.RoleUSER, api.RoleADMIN); err != nil {
		a.fail(w, r, err)
		return
	}
	var in api.OrderUpdate
	if err := decode(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	if err := validateItems(in.Items); err != nil {
		a.fail(w, r, err)
		return
	}
	var out api.OrderResponse
	err := a.tx(r.Context(), func(tx *sql.Tx) error {
		ctx := r.Context()
		order, err := lockOrder(ctx, tx, id, current(r))
		if err != nil {
			return err
		}
		if order.Status != api.CREATED {
			return problem(409, "INVALID_STATE_TRANSITION", "Изменение позиций разрешено только в статусе CREATED")
		}
		if err = a.rateLimit(ctx, tx, order.UserId, "UPDATE_ORDER", a.cfg.UpdateInterval); err != nil {
			return err
		}
		products, err := lockProducts(ctx, tx, in.Items, order.Items)
		if err != nil {
			return err
		}
		if err = restore(ctx, tx, order.Items, products); err != nil {
			return err
		}
		subtotal, err := reserve(ctx, tx, in.Items, products)
		if err != nil {
			return err
		}
		discount := decimal.Zero
		if order.PromoCodeId != nil {
			p, err := scanPromo(tx.QueryRowContext(ctx, `SELECT `+promoColumns+` FROM promo_codes WHERE id=$1 FOR UPDATE`, *order.PromoCodeId))
			if err != nil {
				return err
			}
			if promoUsable(p, a.now(), true) && !subtotal.LessThan(p.MinOrderAmount) {
				discount = discountFor(p, subtotal)
			} else {
				if _, err = tx.ExecContext(ctx, `UPDATE promo_codes SET current_uses=current_uses-1 WHERE id=$1`, p.Id); err != nil {
					return err
				}
				order.PromoCodeId = nil
			}
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM order_items WHERE order_id=$1`, id); err != nil {
			return err
		}
		if err = saveItems(ctx, tx, id, in.Items, products); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE orders SET promo_code_id=$2,total_amount=$3,discount_amount=$4 WHERE id=$1`, id, order.PromoCodeId, subtotal.Sub(discount), discount); err != nil {
			return err
		}
		if err = recordOperation(ctx, tx, order.UserId, "UPDATE_ORDER", a.now()); err != nil {
			return err
		}
		out, err = readOrder(ctx, tx, id, false)
		return err
	})
	a.respond(w, r, 200, out, err)
}
func (a *App) CancelOrder(w http.ResponseWriter, r *http.Request, id int64) {
	if err := permitted(r, api.RoleUSER, api.RoleADMIN); err != nil {
		a.fail(w, r, err)
		return
	}
	var out api.OrderResponse
	err := a.tx(r.Context(), func(tx *sql.Tx) error {
		ctx := r.Context()
		order, err := lockOrder(ctx, tx, id, current(r))
		if err != nil {
			return err
		}
		if order.Status != api.CREATED && order.Status != api.PAYMENTPENDING {
			return problem(409, "INVALID_STATE_TRANSITION", "Отмена разрешена только из CREATED или PAYMENT_PENDING")
		}
		products, err := lockProducts(ctx, tx, nil, order.Items)
		if err != nil {
			return err
		}
		if err = restore(ctx, tx, order.Items, products); err != nil {
			return err
		}
		if order.PromoCodeId != nil {
			if _, err = tx.ExecContext(ctx, `UPDATE promo_codes SET current_uses=current_uses-1 WHERE id=$1`, *order.PromoCodeId); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE orders SET status='CANCELED' WHERE id=$1`, id); err != nil {
			return err
		}
		out, err = readOrder(ctx, tx, id, false)
		return err
	})
	a.respond(w, r, 200, out, err)
}
func canTransition(from, to api.OrderStatus) bool {
	next := map[api.OrderStatus]api.OrderStatus{api.CREATED: api.PAYMENTPENDING, api.PAYMENTPENDING: api.PAID, api.PAID: api.SHIPPED, api.SHIPPED: api.COMPLETED}
	expected, ok := next[from]
	return ok && expected == to
}
func (a *App) TransitionOrder(w http.ResponseWriter, r *http.Request, id int64) {
	if err := permitted(r, api.RoleADMIN); err != nil {
		a.fail(w, r, err)
		return
	}
	var in api.OrderStatusUpdate
	if err := decode(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	var out api.OrderResponse
	err := a.tx(r.Context(), func(tx *sql.Tx) error {
		order, err := lockOrder(r.Context(), tx, id, current(r))
		if err != nil {
			return err
		}
		if !canTransition(order.Status, in.Status) {
			return problem(409, "INVALID_STATE_TRANSITION", "Недопустимый переход состояния заказа")
		}
		if _, err = tx.ExecContext(r.Context(), `UPDATE orders SET status=$2 WHERE id=$1`, id, in.Status); err != nil {
			return err
		}
		out, err = readOrder(r.Context(), tx, id, false)
		return err
	})
	a.respond(w, r, 200, out, err)
}
