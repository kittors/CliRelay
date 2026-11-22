package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const orderSelectColumns = `
	id, order_no, tenant_id, binding_id, target_kind, target_id, target_label,
	end_user_id, plan_id, plan_name, plan_kind, plan_snapshot_json,
	provider, pay_channel, amount_cents, currency, status,
	provider_order_id, provider_trade_no, qrcode_url, pay_url,
	fulfillment_summary, fulfillment_error, source, created_by,
	expires_at, paid_at, fulfilled_at, created_at, updated_at`

func scanOrder(row interface{ Scan(dest ...any) error }) (Order, error) {
	var o Order
	var targetKind, planKind, snapshotJSON, status string
	var rawExpires, rawPaid, rawFulfilled, rawCreated, rawUpdated any
	if err := row.Scan(
		&o.ID, &o.OrderNo, &o.TenantID, &o.BindingID, &targetKind, &o.TargetID, &o.TargetLabel,
		&o.EndUserID, &o.PlanID, &o.PlanName, &planKind, &snapshotJSON,
		&o.Provider, &o.PayChannel, &o.AmountCents, &o.Currency, &status,
		&o.ProviderOrderID, &o.ProviderTradeNo, &o.QRCodeURL, &o.PayURL,
		&o.FulfillmentSummary, &o.FulfillmentError, &o.Source, &o.CreatedBy,
		&rawExpires, &rawPaid, &rawFulfilled, &rawCreated, &rawUpdated,
	); err != nil {
		return Order{}, err
	}
	o.TargetKind = TargetKind(targetKind)
	o.PlanKind = PlanKind(planKind)
	o.Status = OrderStatus(status)
	if strings.TrimSpace(snapshotJSON) != "" {
		_ = json.Unmarshal([]byte(snapshotJSON), &o.PlanSnapshot)
	}
	if ts, ok := parseAnyTime(rawExpires); ok {
		o.ExpiresAt = ts
	}
	if ts, ok := parseAnyTime(rawPaid); ok {
		o.PaidAt = &ts
	}
	if ts, ok := parseAnyTime(rawFulfilled); ok {
		o.FulfilledAt = &ts
	}
	if ts, ok := parseAnyTime(rawCreated); ok {
		o.CreatedAt = ts
	}
	if ts, ok := parseAnyTime(rawUpdated); ok {
		o.UpdatedAt = ts
	}
	return o, nil
}

// GenerateOrderNo produces a <= 26 char ASCII merchant trade_order_id compatible with XunhuPay.
func GenerateOrderNo(now time.Time) (string, error) {
	suffix, err := randomHex(4)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("CP%s%s", now.UTC().Format("20060102150405"), strings.ToUpper(suffix)), nil
}

func (s *Store) CreateOrder(ctx context.Context, o Order) (Order, error) {
	if !s.usable() {
		return Order{}, ErrUnavailable
	}
	snapshotBytes, err := json.Marshal(o.PlanSnapshot)
	if err != nil {
		return Order{}, err
	}
	var paidVal, fulfilledVal any
	if o.PaidAt != nil {
		paidVal = o.PaidAt.UTC()
	}
	if o.FulfilledAt != nil {
		fulfilledVal = o.FulfilledAt.UTC()
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO billing_orders (
			id, order_no, tenant_id, binding_id, target_kind, target_id, target_label,
			end_user_id, plan_id, plan_name, plan_kind, plan_snapshot_json,
			provider, pay_channel, amount_cents, currency, status,
			provider_order_id, provider_trade_no, qrcode_url, pay_url,
			fulfillment_summary, fulfillment_error, source, created_by,
			expires_at, paid_at, fulfilled_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		o.ID, o.OrderNo, o.TenantID, o.BindingID, string(o.TargetKind), o.TargetID, o.TargetLabel,
		o.EndUserID, o.PlanID, o.PlanName, string(o.PlanKind), string(snapshotBytes),
		o.Provider, o.PayChannel, o.AmountCents, o.Currency, string(o.Status),
		o.ProviderOrderID, o.ProviderTradeNo, o.QRCodeURL, o.PayURL,
		o.FulfillmentSummary, o.FulfillmentError, o.Source, o.CreatedBy,
		o.ExpiresAt.UTC(), paidVal, fulfilledVal, o.CreatedAt.UTC(), o.UpdatedAt.UTC(),
	); err != nil {
		return Order{}, err
	}
	return s.GetOrderByNo(ctx, o.TenantID, o.OrderNo)
}

func (s *Store) GetOrderByNo(ctx context.Context, tenantID, orderNo string) (Order, error) {
	if !s.usable() {
		return Order{}, ErrUnavailable
	}
	query := `SELECT ` + orderSelectColumns + ` FROM billing_orders WHERE order_no = ?`
	args := []any{strings.TrimSpace(orderNo)}
	if strings.TrimSpace(tenantID) != "" {
		query += ` AND tenant_id = ?`
		args = append(args, strings.TrimSpace(tenantID))
	}
	o, err := scanOrder(s.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	return o, err
}

func (s *Store) UpdateOrderPaymentDetails(ctx context.Context, tenantID, orderNo, providerOrderID, qrURL, payURL string) error {
	if !s.usable() {
		return ErrUnavailable
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE billing_orders
		   SET provider_order_id = ?, qrcode_url = ?, pay_url = ?, updated_at = ?
		 WHERE tenant_id = ? AND order_no = ?
	`, providerOrderID, qrURL, payURL, time.Now().UTC(), tenantID, orderNo)
	return err
}

func (s *Store) ExpireStalePendingOrders(ctx context.Context, tenantID string, now time.Time) {
	if !s.usable() {
		return
	}
	_, _ = s.db.ExecContext(ctx, `
		UPDATE billing_orders
		   SET status = ?, updated_at = ?
		 WHERE tenant_id = ? AND status = ? AND expires_at <= ?
	`, string(OrderStatusExpired), now.UTC(), tenantID, string(OrderStatusPending), now.UTC())
}

// ClaimOrderForFulfillment atomically transitions an order into the in-progress
// "paid" lock state so concurrent webhooks and status polls can never double-fulfill.
// When forceRetry is false (webhooks / auto-sync), only pending or expired orders
// are claimed; paid (in-progress) and fulfilled orders return alreadyHandled=true.
// When forceRetry is true (explicit admin manual retry), failed and paid orders may
// also be reclaimed.
func (s *Store) ClaimOrderForFulfillment(ctx context.Context, tenantID, orderNo, providerOrderID, providerTradeNo string, paidAt time.Time, forceRetry bool) (Order, bool, error) {
	if !s.usable() {
		return Order{}, false, ErrUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Order{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	selectSQL := `SELECT ` + orderSelectColumns + ` FROM billing_orders WHERE tenant_id = ? AND order_no = ?`
	o, err := scanOrder(tx.QueryRowContext(ctx, selectSQL+" FOR UPDATE", tenantID, orderNo))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		o, err = scanOrder(tx.QueryRowContext(ctx, selectSQL, tenantID, orderNo))
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Order{}, false, ErrNotFound
	}
	if err != nil {
		return Order{}, false, err
	}
	if o.Status == OrderStatusFulfilled {
		_ = tx.Commit()
		return o, true, nil
	}
	if o.Status == OrderStatusClosed {
		return Order{}, false, ErrInvalidTransition
	}
	if !forceRetry && o.Status == OrderStatusPaid {
		// Another worker is actively fulfilling this order right now.
		_ = tx.Commit()
		return o, true, nil
	}
	if providerOrderID == "" {
		providerOrderID = o.ProviderOrderID
	}
	if providerTradeNo == "" {
		providerTradeNo = o.ProviderTradeNo
	}
	effPaidAt := paidAt.UTC()
	if o.PaidAt != nil {
		effPaidAt = o.PaidAt.UTC()
	}
	allowed := `status IN ('pending', 'expired')`
	if forceRetry {
		allowed = `status IN ('pending', 'expired', 'failed', 'paid')`
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE billing_orders
		   SET status = ?, provider_order_id = ?, provider_trade_no = ?,
		       paid_at = ?, fulfillment_error = '', updated_at = ?
		 WHERE tenant_id = ? AND order_no = ? AND `+allowed,
		string(OrderStatusPaid), providerOrderID, providerTradeNo, effPaidAt, time.Now().UTC(),
		tenantID, orderNo)
	if err != nil {
		return Order{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_ = tx.Commit()
		return o, true, nil
	}
	if err := tx.Commit(); err != nil {
		return Order{}, false, err
	}
	o.Status = OrderStatusPaid
	o.ProviderOrderID = providerOrderID
	o.ProviderTradeNo = providerTradeNo
	o.PaidAt = &effPaidAt
	return o, false, nil
}

func (s *Store) MarkOrderFulfilled(ctx context.Context, tenantID, orderNo, summary string, fulfilledAt time.Time) (Order, error) {
	if !s.usable() {
		return Order{}, ErrUnavailable
	}
	now := fulfilledAt.UTC()
	if _, err := s.db.ExecContext(ctx, `
		UPDATE billing_orders
		   SET status = ?, fulfillment_summary = ?, fulfillment_error = '',
		       fulfilled_at = ?, updated_at = ?
		 WHERE tenant_id = ? AND order_no = ?
	`, string(OrderStatusFulfilled), strings.TrimSpace(summary), now, now, tenantID, orderNo); err != nil {
		return Order{}, err
	}
	return s.GetOrderByNo(ctx, tenantID, orderNo)
}

func (s *Store) MarkOrderFulfillmentFailed(ctx context.Context, tenantID, orderNo, errMsg string) {
	if !s.usable() {
		return
	}
	_, _ = s.db.ExecContext(ctx, `
		UPDATE billing_orders
		   SET status = ?, fulfillment_error = ?, updated_at = ?
		 WHERE tenant_id = ? AND order_no = ? AND status <> ?
	`, string(OrderStatusFailed), strings.TrimSpace(errMsg), time.Now().UTC(), tenantID, orderNo, string(OrderStatusFulfilled))
}

func (s *Store) ListOrders(ctx context.Context, tenantID string, filter OrderFilter) (OrderListResult, error) {
	if !s.usable() {
		return OrderListResult{}, ErrUnavailable
	}
	s.ExpireStalePendingOrders(ctx, tenantID, time.Now())

	page := filter.Page
	if page < 1 {
		page = 1
	}
	size := filter.Size
	if size < 1 {
		size = 20
	}
	if size > 100 {
		size = 100
	}

	where := []string{"tenant_id = ?"}
	args := []any{tenantID}
	if st := strings.TrimSpace(filter.Status); st != "" && st != "all" {
		where = append(where, "status = ?")
		args = append(args, st)
	}
	if filter.TargetKind != "" {
		where = append(where, "target_kind = ?")
		args = append(args, string(filter.TargetKind))
	}
	if id := strings.TrimSpace(filter.TargetID); id != "" {
		where = append(where, "target_id = ?")
		args = append(args, id)
	}
	if eu := strings.TrimSpace(filter.EndUserID); eu != "" {
		where = append(where, "end_user_id = ?")
		args = append(args, eu)
	}
	if q := strings.TrimSpace(filter.Search); q != "" {
		like := "%" + strings.ToLower(q) + "%"
		where = append(where, "(LOWER(order_no) LIKE ? OR LOWER(target_label) LIKE ? OR LOWER(plan_name) LIKE ? OR LOWER(provider_order_id) LIKE ?)")
		args = append(args, like, like, like, like)
	}
	whereClause := strings.Join(where, " AND ")

	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM billing_orders WHERE `+whereClause, args...).Scan(&total); err != nil {
		return OrderListResult{}, err
	}

	listArgs := append(append([]any(nil), args...), size, (page-1)*size)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+orderSelectColumns+` FROM billing_orders WHERE `+whereClause+` ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		listArgs...,
	)
	if err != nil {
		return OrderListResult{}, err
	}
	defer rows.Close()
	items := make([]Order, 0)
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return OrderListResult{}, err
		}
		items = append(items, o)
	}
	if err := rows.Err(); err != nil {
		return OrderListResult{}, err
	}

	summary, err := s.OrderStats(ctx, tenantID, filter.EndUserID)
	if err != nil {
		return OrderListResult{}, err
	}
	return OrderListResult{
		Items:   items,
		Total:   total,
		Page:    page,
		Size:    size,
		Summary: summary,
	}, nil
}

func (s *Store) OrderStats(ctx context.Context, tenantID, endUserID string) (OrderSummary, error) {
	var summary OrderSummary
	if !s.usable() {
		return summary, ErrUnavailable
	}
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).UTC()

	query := `SELECT status, amount_cents, paid_at, created_at FROM billing_orders WHERE tenant_id = ?`
	args := []any{tenantID}
	if strings.TrimSpace(endUserID) != "" {
		query += ` AND end_user_id = ?`
		args = append(args, strings.TrimSpace(endUserID))
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return summary, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var amount int64
		var rawPaid, rawCreated any
		if err := rows.Scan(&status, &amount, &rawPaid, &rawCreated); err != nil {
			return summary, err
		}
		switch OrderStatus(status) {
		case OrderStatusFulfilled, OrderStatusPaid:
			summary.PaidCount++
			summary.TotalAmountCents += amount
			ts, ok := parseAnyTime(rawPaid)
			if !ok {
				ts, _ = parseAnyTime(rawCreated)
			}
			if !ts.Before(monthStart) {
				summary.MonthAmountCents += amount
			}
			if !ts.Before(todayStart) {
				summary.TodayAmountCents += amount
			}
		case OrderStatusPending:
			summary.PendingCount++
		case OrderStatusFailed:
			summary.FailedCount++
		}
	}
	return summary, rows.Err()
}
