package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/shopspring/decimal"
	"marketplace/internal/api"
)

const promoColumns = `id,code,discount_type,discount_value,min_order_amount,max_uses,current_uses,valid_from,valid_until,active`

func scanPromo(row scanner) (api.PromoCodeResponse, error) {
	var p api.PromoCodeResponse
	err := row.Scan(&p.Id, &p.Code, &p.DiscountType, &p.DiscountValue, &p.MinOrderAmount, &p.MaxUses, &p.CurrentUses, &p.ValidFrom, &p.ValidUntil, &p.Active)
	return p, err
}
func (a *App) CreatePromoCode(w http.ResponseWriter, r *http.Request) {
	if err := permitted(r, api.RoleSELLER, api.RoleADMIN); err != nil {
		a.fail(w, r, err)
		return
	}
	var in api.PromoCodeCreate
	if err := decode(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	if in.ValidUntil.Before(in.ValidFrom) {
		a.fail(w, r, invalid("valid_until", "Окончание срока не может быть раньше начала"))
		return
	}
	active := true
	if in.Active != nil {
		active = *in.Active
	}
	p, err := scanPromo(a.db.QueryRowContext(r.Context(), `INSERT INTO promo_codes(code,discount_type,discount_value,min_order_amount,max_uses,valid_from,valid_until,active) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+promoColumns, in.Code, in.DiscountType, in.DiscountValue, in.MinOrderAmount, in.MaxUses, in.ValidFrom, in.ValidUntil, active))
	if isUnique(err) {
		err = problem(409, "PROMO_CODE_ALREADY_EXISTS", "Промокод с таким кодом уже существует")
	}
	a.respond(w, r, 201, p, err)
}
func promoUsable(p api.PromoCodeResponse, now time.Time, alreadyApplied bool) bool {
	uses := p.CurrentUses
	if alreadyApplied {
		uses--
	}
	return p.Active && uses < p.MaxUses && !now.Before(p.ValidFrom) && !now.After(p.ValidUntil)
}
func discountFor(p api.PromoCodeResponse, subtotal decimal.Decimal) decimal.Decimal {
	if p.DiscountType == api.PERCENTAGE {
		percent := decimal.Min(p.DiscountValue, decimal.NewFromInt(70))
		return subtotal.Mul(percent).Div(decimal.NewFromInt(100)).Round(2)
	}
	return decimal.Min(p.DiscountValue, subtotal).Round(2)
}
func applyPromo(ctx context.Context, tx *sql.Tx, code string, subtotal decimal.Decimal, now time.Time) (api.PromoCodeResponse, decimal.Decimal, error) {
	p, err := scanPromo(tx.QueryRowContext(ctx, `SELECT `+promoColumns+` FROM promo_codes WHERE code=$1 FOR UPDATE`, code))
	if errors.Is(err, sql.ErrNoRows) {
		return p, decimal.Zero, problem(422, "PROMO_CODE_INVALID", "Промокод недействителен")
	}
	if err != nil {
		return p, decimal.Zero, err
	}
	if !promoUsable(p, now, false) {
		return p, decimal.Zero, problem(422, "PROMO_CODE_INVALID", "Промокод неактивен, истёк или исчерпан")
	}
	if subtotal.LessThan(p.MinOrderAmount) {
		return p, decimal.Zero, problem(422, "PROMO_CODE_MIN_AMOUNT", "Сумма заказа ниже минимальной для промокода")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE promo_codes SET current_uses=current_uses+1 WHERE id=$1`, p.Id); err != nil {
		return p, decimal.Zero, err
	}
	return p, discountFor(p, subtotal), nil
}
