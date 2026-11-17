package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"hop.top/uma/model"
	"hop.top/uma/store/internal/search"
	"hop.top/uma/store/postgres/internal/db"
)

// searchVersion is the schema version that added the message search
// columns (migrations/00014_message_search.sql).
const searchVersion = 14

// messageSearch is the support Messages declares: the message search
// fields are evaluated in SQL under the shared rules of
// store/internal/search.
var messageSearch = filterSupport{inboxes: true, conversation: true, labels: true, task: true, text: true, since: true, before: true, attachments: true}

// searchColumns are the indexed columns a message row stores beside its
// body.
type searchColumns struct {
	tokens []string
	unix   int64
	nanos  int32
	// recorded is RecordedAt, for the snooze wake rule (workflow.go).
	recUnix  int64
	recNanos int32
}

func searchColumnsOf(m model.Message) searchColumns {
	recUnix, recNanos := instant(m.RecordedAt)
	return searchColumns{
		tokens:   strs(search.Tokens(m)),
		unix:     m.OccurredAt.Unix(),
		nanos:    int32(m.OccurredAt.Nanosecond()), // #nosec G115 -- below 1e9
		recUnix:  recUnix,
		recNanos: recNanos,
	}
}

// listMessagesParams returns the ListMessages arguments of f. Filter
// fields arrive validated.
func (h *handle) listMessagesParams(f model.Filter, after int64, lim int32) db.ListMessagesParams {
	inbox, conv := conversationArgs(f)
	anyOf, allOf, noneOf := selectorArgs(f)
	p := db.ListMessagesParams{
		TenantID: h.tenantID, After: after, Inboxes: inboxIDs(f), ConversationInbox: inbox, ConversationID: conv,
		AnyLabels: anyOf, AllLabels: allOf, NoneLabels: noneOf,
		Tokens: strs(model.SearchTokens(f.Text)), MediaExact: []string{}, MediaTypes: []string{}, Lim: lim,
	}
	if tk := f.Task; tk != nil {
		p.TaskID, p.TaskInbox, p.TaskConversation = optID(tk.ID), optID(tk.Conversation.Inbox.ID), optID(tk.Conversation.ID)
	}
	if f.Since != nil {
		c := searchColumnsOf(model.Message{OccurredAt: *f.Since})
		p.SinceUnix, p.SinceNanos = &c.unix, &c.nanos
	}
	if f.Before != nil {
		c := searchColumnsOf(model.Message{OccurredAt: *f.Before})
		p.BeforeUnix, p.BeforeNanos = &c.unix, &c.nanos
	}
	if a := f.Attachments; a != nil {
		p.HasAttachments = a.Has
		p.MediaExact, p.MediaTypes = search.Patterns(a.MediaTypes)
	}
	return p
}

// fillBatch bounds the rows one fill transaction tokenizes.
const fillBatch = 500

// fillSearchTokens tokenizes every message row written before the search
// migration, in (tenant, id) order, one transaction per batch. A row
// written meanwhile already carries its tokens and is skipped, so
// concurrent fills and writers agree; a failed fill leaves the rest NULL
// for the next Migrate.
func fillSearchTokens(ctx context.Context, pool *pgxpool.Pool) error {
	afterTenant, afterID := "", "00000000-0000-0000-0000-000000000000"
	for {
		n, err := fillSearchBatch(ctx, pool, &afterTenant, &afterID)
		if err != nil {
			return migrationError(ctx, err)
		}
		if n < fillBatch {
			return nil
		}
	}
}

func fillSearchBatch(ctx context.Context, pool *pgxpool.Pool, afterTenant, afterID *string) (n int, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	q := db.New(tx)
	rows, err := q.ListUnindexedMessages(ctx, db.ListUnindexedMessagesParams{AfterTenant: *afterTenant, AfterID: *afterID, Lim: fillBatch})
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		m, err := decode[model.Message](row.Body)
		if err != nil {
			return 0, err
		}
		if err := q.SetMessageTokens(ctx, db.SetMessageTokensParams{
			TenantID: row.TenantID, ID: row.ID, SearchTokens: searchColumnsOf(m).tokens,
		}); err != nil {
			return 0, err
		}
		*afterTenant, *afterID = row.TenantID, row.ID
	}
	return len(rows), tx.Commit(ctx)
}

// checkSearchFilled requires every message row to carry its tokens.
func checkSearchFilled(ctx context.Context, pool *pgxpool.Pool) error {
	pending, err := db.New(pool).UnindexedMessagesExist(ctx)
	if err != nil {
		return migrationError(ctx, err)
	}
	if pending {
		return model.NewError(model.SchemaMismatch,
			"message search index is incomplete; run Migrate before Open")
	}
	return nil
}
