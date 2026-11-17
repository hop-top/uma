package examples

import (
	"context"
	"database/sql"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// SQLBoundary shows transaction wiring, not a conforming storage implementation.
// Bind must supply all repositories, enforce tenant scope using the same *sql.Tx
// and make them fail once the callback returns. The callback runs at most once
// and is never retried; a callback error or panic rolls back.
type SQLBoundary struct {
	DB      *sql.DB
	Bind    func(*sql.Tx, model.TenantID) (store.Tx, error)
	Profile store.Properties
}

func (s *SQLBoundary) Properties() store.Properties { return s.Profile }
func (s *SQLBoundary) View(ctx context.Context, tenant model.TenantID,
	fn func(store.Reader) error) error {
	// Wrap the Tx so a View callback cannot type-assert its Reader to a Tx.
	return s.withTx(ctx, tenant, true, func(tx store.Tx) error { return fn(viewReader{tx}) })
}
func (s *SQLBoundary) Transact(ctx context.Context, tenant model.TenantID,
	fn func(store.Tx) error) error {
	return s.withTx(ctx, tenant, false, fn)
}
func (s *SQLBoundary) withTx(ctx context.Context, tenant model.TenantID, readOnly bool,
	fn func(store.Tx) error) error {
	if s.DB == nil || s.Bind == nil || tenant == "" {
		return model.NewError(model.Unavailable, "storage binding missing")
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: readOnly})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit; runs on panic too
	repos, err := s.Bind(tx, tenant)
	if err != nil {
		return err
	}
	if err := fn(repos); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		// A failed commit may still have applied: recover by ingress or
		// operation key, never by reporting success or re-running fn.
		return model.NewError(model.OutcomeUnknown, "commit outcome unknown")
	}
	return nil
}

// viewReader exposes only the Reader method set of a Tx.
type viewReader struct{ store.Reader }

var _ store.Store = (*SQLBoundary)(nil)
