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
