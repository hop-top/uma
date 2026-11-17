package postgres

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"hop.top/uma/model"
)

func invalid(msg string) error  { return model.NewError(model.Validation, msg) }
func conflict(msg string) error { return model.NewError(model.Conflict, msg) }
func notFound() error           { return model.NewError(model.NotFound, "resource not found") }

// classify maps a pgx or database error to a *model.Error. Context errors
// stay standard context errors. Safe messages carry the SQLSTATE and, for
// constraint failures, the constraint name; never values or server text.
func classify(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if cerr := ctx.Err(); cerr != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err)) {
		return cerr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var me *model.Error
	if errors.As(err, &me) {
		return me
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return model.NewError(model.Unavailable, "database unavailable")
	}
	msg := "database error " + pe.Code
	if pe.ConstraintName != "" {
		msg += " (" + pe.ConstraintName + ")"
	}
	switch {
	case pe.Code == "40001", pe.Code == "40P01":
		return model.NewError(model.Transient, "unit could not be serialized; retry the unit: "+msg)
	case pe.Code == "55P03":
		return model.NewError(model.Transient, "lock not available; retry the unit: "+msg)
	case pe.Code == "23505":
		return model.NewError(model.Conflict, "uniqueness violation: "+msg)
	case pe.Code == "23503":
		// A parent the record points at does not exist in the unit's tenant.
		return model.NewError(model.NotFound, "resource not found: "+msg)
	case pe.Code == "57014":
		return model.NewError(model.Transient, "statement canceled: "+msg)
	case strings.HasPrefix(pe.Code, "22"), strings.HasPrefix(pe.Code, "23"):
		// Data exceptions and remaining integrity violations: input the core
		// checks did not catch, such as text holding NUL bytes.
		return model.NewError(model.Validation, "invalid value: "+msg)
	case strings.HasPrefix(pe.Code, "08"), strings.HasPrefix(pe.Code, "53"), strings.HasPrefix(pe.Code, "57"):
		return model.NewError(model.Unavailable, "database unavailable: "+msg)
	case pe.Code == "3F000", pe.Code == "42P01", pe.Code == "42703":
		// Missing schema, table or column: the database does not hold the
		// schema this package was opened against.
		return model.NewError(model.SchemaMismatch, "database schema does not match: "+msg)
	}
	return model.NewError(model.Permanent, msg)
}

// commitError classifies a failed COMMIT. A server-reported failure means the
// unit rolled back. A connection failure during commit leaves the outcome
// unknown: the caller recovers through its persisted ingress or operation
// key, never by assuming either outcome.
func commitError(ctx context.Context, err error) error {
	var pe *pgconn.PgError
	switch {
	case errors.As(err, &pe):
		return classify(ctx, err)
	case errors.Is(err, pgx.ErrTxCommitRollback):
		return model.NewError(model.Transient, "unit rolled back at commit; retry the unit")
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return model.NewError(model.OutcomeUnknown, "commit outcome unknown")
}

// firstErr returns the first non-nil error.
func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
