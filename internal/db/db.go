package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func New(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, err
	}
	return pool, nil
}

// MarkEventSeen returns false if this (source, eventID) was already processed —
// use it as the dedupe gate at the top of every webhook handler.
func MarkEventSeen(ctx context.Context, pool *pgxpool.Pool, source, eventID string) (bool, error) {
	tag, err := pool.Exec(ctx,
		`INSERT INTO webhook_events (source, event_id) VALUES ($1, $2)
		 ON CONFLICT (source, event_id) DO NOTHING`,
		source, eventID,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ReleaseEvent undoes MarkEventSeen so the next redelivery of the event is
// processed again. Call it when handling failed with a retryable error.
func ReleaseEvent(ctx context.Context, pool *pgxpool.Pool, source, eventID string) error {
	_, err := pool.Exec(ctx,
		`DELETE FROM webhook_events WHERE source = $1 AND event_id = $2`,
		source, eventID,
	)
	return err
}

// RecordOrderError stores the failing step and error on the order, so stuck
// orders can be found with: SELECT * FROM orders WHERE last_error IS NOT NULL.
func RecordOrderError(ctx context.Context, pool *pgxpool.Pool, orderID int64, step string, cause error) error {
	_, err := pool.Exec(ctx,
		`UPDATE orders
		 SET last_error = $1, last_error_step = $2, last_error_at = now(), updated_at = now()
		 WHERE id = $3`,
		cause.Error(), step, orderID,
	)
	return err
}
