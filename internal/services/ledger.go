package services

import (
	"context"
	"encoding/json"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// localCommand is one local domain mutation entering the operation ledger.
// Unlike submission it queues no provider work: the operation claim, the
// mutation and its events commit in one unit with state model.Committed.
//
// The operation key is (tenant, caller, kind, target, operation ID), the
// caller being the client's principal. For a new resource, target is a
// stable parent or configuration namespace, so a retried create replays
// instead of allocating again.
type localCommand struct {
	kind        string
	target      model.Ref
	resource    model.Ref
	owner       model.Ref
	command     model.Command
	audience    model.Audience
	destination *model.Ref
	fingerprint []byte
	// result is the resource the operation produces or changes.
	result        model.Ref
	resultMessage *model.MessageRef
	resultTask    *model.TaskRef
	// authorize, when set, replaces the authorization of kind on resource.
	// It runs where that would, before the unit, and may keep the decision
	// for apply to record.
	authorize func(ctx context.Context) error
	// link is the replay context a Link or Unlink stores with its intent.
	link *linkIntent
	// apply checks mutable preconditions and writes the mutation inside the
	// claiming unit. It runs only for a new claim, never for a replay: a
	// completed command does not fail because its own expected revision has
	// since advanced. It must be free of effects outside the unit.
	apply func(ctx context.Context, tx store.Tx, op model.OperationRef) error
}

// commit authorizes c on its resource, then claims its operation and applies
// it in one unit. The same key with the same fingerprint returns the stored
// operation with Replayed set; with a different fingerprint it is
// model.Conflict. Authorization runs before any stored result is revealed.
// IDs and revisions are allocated before the unit and reused across its
// bounded retries.
func commit(ctx context.Context, cl *engine.Client, c localCommand, opID model.ID, rev model.Revision) (model.Acceptance, error) {
	authorize := c.authorize
	if authorize == nil {
		authorize = func(ctx context.Context) error {
			return cl.Authorize(ctx, policy.Authorization{
				Action: c.kind, Resource: c.resource, Audience: c.audience,
				Destination: c.destination, Revision: c.command.ExpectedRevision,
			}, c.owner)
		}
	}
	if err := authorize(ctx); err != nil {
		return model.Acceptance{}, err
	}
	if err := operations.RefuseHold(c.command); err != nil {
		return model.Acceptance{}, err
	}
	done, err := cl.Engine().Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	tenant := cl.Principal().Tenant
	opRef := model.OperationRef{Tenant: tenant, ID: opID}
	result := c.result
	intent, err := json.Marshal(localIntent{Kind: c.kind, Result: result, Link: c.link})
	if err != nil {
		return model.Acceptance{}, model.NewError(model.Permanent, "operation intent cannot be stored")
	}
	rec := store.OperationRecord{
		Operation: model.Operation{
			Ref: opRef, State: model.Committed, Result: &result, ResultMessage: c.resultMessage, ResultTask: c.resultTask,
			Revision: rev,
		},
		Key: store.OperationKey{
			Tenant: tenant, Caller: cl.Principal().Actor, Kind: c.kind, Target: c.target, ID: c.command.Operation,
		},
		FingerprintVersion: operations.FingerprintVersion1,
		Fingerprint:        c.fingerprint,
		Command:            model.Extension{Name: intentLocal, Version: 1, Data: intent},
	}
	var got store.OperationRecord
	var created bool
	err = operations.Transact(ctx, cl.Engine(), tenant, func(tx store.Tx) error {
		var err error
		got, created, err = tx.ClaimOperation(ctx, rec)
		if err != nil || !created {
			return err
		}
		return c.apply(ctx, tx, opRef)
	})
	if err != nil {
		if model.IsCode(err, model.Conflict) && !created {
			return model.Acceptance{}, model.NewError(model.Conflict,
				"operation ID already used with a different payload")
		}
		return model.Acceptance{}, err
	}
	return model.Acceptance{
		Operation: operations.Shown(got),
		Durable:   cl.Engine().Store().Properties().Durable,
		Replayed:  !created,
	}, nil
}

// intentLocal names the stored intent of a local command: its kind and
// result, plus the replay context of a Link or Unlink. Local commands
// complete in their claiming unit; no worker reads the intent.
const intentLocal = "uma.intent.local"

type localIntent struct {
	Kind   string      `json:"kind"`
	Result model.Ref   `json:"result"`
	Link   *linkIntent `json:"link,omitempty"`
}

// allocate returns n fresh IDs, allocated before any unit of work.
func allocate(ctx context.Context, e *engine.Engine, n int) ([]model.ID, error) {
	out := make([]model.ID, n)
	for i := range out {
		id, err := operations.NewID(ctx, e)
		if err != nil {
			return nil, err
		}
		out[i] = id
	}
	return out, nil
}

// commandEvent builds one committed event caused by a local command.
func commandEvent(cl *engine.Client, id model.ID, kind, schema string, res model.Ref, rev model.Revision,
	audience model.Audience, correlation model.CorrelationID, payload any,
) (model.Event, error) {
	return commandEventAs(cl.Engine(), cl.Principal().Actor, id, kind, schema, res, rev, audience, correlation, payload)
}

// commandEventAs builds one committed event caused by a local command that
// actor made, for a command applied after its caller returned: an approved
// held operation.
func commandEventAs(e *engine.Engine, actor model.ActorID, id model.ID, kind, schema string, res model.Ref, rev model.Revision,
	audience model.Audience, correlation model.CorrelationID, payload any,
) (model.Event, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return model.Event{}, model.NewError(model.Permanent, "event payload encoding failed")
	}
	now := e.Clock().Now().UTC()
	return model.Event{
		Version: model.EventEnvelopeVersion, ID: id, Kind: kind,
		Tenant: res.Tenant, Inbox: res.Inbox, Resource: res,
		Actor: actor, OccurredAt: now, RecordedAt: now,
		Correlation: correlation, Audience: audience, Revision: rev,
		Payload: model.Extension{Name: schema, Version: model.PayloadVersion1, Data: data},
	}, nil
}

// Command-triggered payloads (version 1). They mirror the provider-triggered
// ones and add the originating operation.

// statusChange is the status summary of a conversation.changed payload.
type statusChange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type commandConversationChanged struct {
	Conversation     model.ConversationRef `json:"conversation"`
	Changes          []string              `json:"changes"`
	Trigger          string                `json:"trigger"`
	Status           *statusChange         `json:"status,omitempty"`
	PreviousRevision model.Revision        `json:"previous_revision,omitempty"`
	Operation        model.OperationRef    `json:"operation"`
}

type commandMessageCreated struct {
	Message   model.MessageRef   `json:"message"`
	Trigger   string             `json:"trigger"`
	Origin    string             `json:"origin"`
	Operation model.OperationRef `json:"operation"`
	Source    model.TypeKey      `json:"source,omitempty"`
	InReplyTo *model.MessageRef  `json:"in_reply_to,omitempty"`
}
