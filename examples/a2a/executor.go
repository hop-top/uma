package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"hop.top/uma"
	"hop.top/uma/companions/bot"
	"hop.top/uma/model"
)

// executor is the host's side of the A2A contract. uma never executes
// anything: it records what remote agents send and what the host reports.
// The executor reads message.received and task.updated events, decides what
// each message means, and answers through the two write paths the host owns:
// Messages.Reply for a message-only exchange, Tasks.Act to open a task and
// report its progress.
//
// What a message means is the host's business; the adapter maps nothing in
// a message's content to a call. This executor reads the first word:
//
//	echo <text>     answer with the text; no task unless the request has one
//	report <topic>  open a task: working, an artifact, completed with one status message
//	late <topic>    like report, after lateDelay: a blocking caller may give up first
//	hold            open a task and keep it working until a cancel request
//	anything else   answer with the help text
//
// Every write carries an operation ID derived from the event, and each step
// of a task is chosen from the task's stored state, so an event redelivered
// after a crash repeats nothing and finishes what is left.
type executor struct {
	client    uma.Client
	sub       model.SubscriptionRef
	lateDelay time.Duration
}

const helpText = "Commands: echo <text>, report <topic>, late <topic>, hold."

// Run executes events until ctx ends.
func (e *executor) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		if err := e.step(ctx); err != nil && ctx.Err() == nil {
			log.Printf("executor: %v", err)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
	return nil
}

// step polls one batch, runs its events in order and acknowledges it. A
// retryable failure leaves the batch unacknowledged, so it is delivered
// again; any other failure drops the event it came from.
func (e *executor) step(ctx context.Context) error {
	batch, err := e.client.Events().Poll(ctx, model.Poll{
		Call: model.Call{Correlation: "desk-poll"}, Subscription: e.sub, Limit: 20, Wait: 500 * time.Millisecond,
	})
	if err != nil || batch.AckToken == "" {
		return err
	}
	for _, ev := range batch.Events {
		if err := e.handle(ctx, ev); err != nil {
			if bot.Classify(err) == bot.Retried {
				return fmt.Errorf("event %s: %w", ev.ID, err)
			}
			log.Printf("executor: dropped event %s: %v", ev.ID, err)
		}
	}
	sum := sha256.Sum256([]byte(batch.AckToken))
	_, err = e.client.Events().Ack(ctx, model.Acknowledge{
		Command:      model.Command{Call: model.Call{Correlation: "desk-ack"}, Operation: model.OperationID("desk.ack." + hex.EncodeToString(sum[:]))},
		Subscription: e.sub, Token: batch.AckToken,
	})
	return err
}

func (e *executor) handle(ctx context.Context, ev model.Event) error {
	switch ev.Kind {
	case model.EventMessageReceived:
		return e.message(ctx, ev)
	case model.EventTaskUpdated:
		return e.taskUpdated(ctx, ev)
	}
	return nil
}

// message executes a message a remote agent sent.
func (e *executor) message(ctx context.Context, ev model.Event) error {
	var p model.MessageReceivedPayload
	if err := decode(ev, model.SchemaMessageReceived, &p); err != nil || p.AuthorKind != model.ContactKindAgent {
		return err
	}
	msg, err := e.client.Messages().Get(ctx, model.Lookup[model.MessageRef]{Call: callOf(ev), Ref: p.Message})
	if err != nil {
		return err
	}
	verb, arg, _ := strings.Cut(strings.TrimSpace(bot.Text(msg.Content)), " ")
	arg = strings.TrimSpace(arg)
	switch strings.ToLower(verb) {
	case "echo":
		return e.answer(ctx, ev, msg, "echo: "+arg)
	case "late":
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(e.lateDelay):
		}
		fallthrough
	case "report":
		return e.work(ctx, ev, msg, plan{status: "Report on " + arg + " is ready.", artifact: &model.ArtifactUpdate{
			ID: "report", Name: "report.txt", Complete: true,
			Content: textContent("Report on " + arg + ": nothing to flag."),
		}})
	case "hold":
		return e.work(ctx, ev, msg, plan{hold: true})
	}
	return e.answer(ctx, ev, msg, helpText)
}

// answer answers a message with text. Without a task that is a plain
// reply: the exchange stays message-only. A message bound to a task (the
// request set returnImmediately) is answered through its task instead.
func (e *executor) answer(ctx context.Context, ev model.Event, msg model.Message, text string) error {
	if msg.Task != nil {
		return e.work(ctx, ev, msg, plan{status: text})
	}
	ref := msg.Ref
	_, err := e.client.Messages().Reply(ctx, model.Reply{
		Command:      model.Command{Call: callOf(ev), Operation: opID(ev, "reply")},
		Conversation: msg.Ref.Conversation, InReplyTo: &ref, Content: textContent(text),
	})
	return err
}

// plan is the work of one task: an artifact, a final status message, or a
// task that is held working.
type plan struct {
	artifact *model.ArtifactUpdate
	status   string
	hold     bool
}

// taskStep is one update of a task, taken only while done reports false.
type taskStep struct {
	name string
	done func(model.Task) bool
	upd  model.TaskUpdate
}

// steps lists the updates of a plan in order: working, the artifact, then
// completed with the status message.
func (pl plan) steps() []taskStep {
	steps := []taskStep{{
		"work", func(t model.Task) bool { return t.State == model.TaskWorking }, model.TaskUpdate{State: model.TaskWorking},
	}}
	if a := pl.artifact; a != nil {
		steps = append(steps, taskStep{
			"artifact",
			func(t model.Task) bool {
				return slices.ContainsFunc(t.Artifacts, func(x model.Artifact) bool { return x.ID == a.ID })
			},
			model.TaskUpdate{Artifacts: []model.ArtifactUpdate{*a}},
		})
	}
	if !pl.hold {
		status := textContent(pl.status)
		steps = append(steps, taskStep{
			"complete", func(model.Task) bool { return false },
			model.TaskUpdate{State: model.TaskCompleted, StatusMessage: &status},
		})
	}
	return steps
}

// work runs a plan on the message's task, opening one if the message has
// none: that is the host's explicit choice, nothing opens a task for it.
// Each step is taken only when the task's stored state says it is still to
// do, and a cancel request ends the work as canceled.
func (e *executor) work(ctx context.Context, ev model.Event, msg model.Message, pl plan) error {
	t, err := e.task(ctx, ev, msg)
	if err != nil {
		return err
	}
	for _, s := range pl.steps() {
		switch {
		case model.IsTerminalTaskState(t.State):
			return nil
		case t.CancelRequested:
			return e.cancel(ctx, ev, t)
		case s.done(t):
			continue
		}
		if t, err = e.update(ctx, ev, s.name, t, s.upd); err != nil {
			return err
		}
	}
	return nil
}

// task returns the task of msg, opening one in the working state when it
// has none. Opening is replay-safe: the operation ID repeats the first
// open.
func (e *executor) task(ctx context.Context, ev model.Event, msg model.Message) (model.Task, error) {
	ref := msg.Task
	if ref == nil {
		data, err := json.Marshal(model.TaskOpen{Message: msg.Ref, State: model.TaskWorking})
		if err != nil {
			return model.Task{}, err
		}
		acc, err := e.client.Tasks().Act(ctx, model.TaskAction{
			Command: model.Command{Call: callOf(ev), Operation: opID(ev, "open")},
			Target:  model.TaskRef{Conversation: msg.Ref.Conversation},
			Action:  model.Extension{Name: model.SchemaTaskOpen, Version: 1, Data: data},
		})
		if err != nil {
			return model.Task{}, err
		}
		ref = acc.Operation.ResultTask
	}
	return e.client.Tasks().Get(ctx, model.Lookup[model.TaskRef]{Call: callOf(ev), Ref: *ref})
}

// update reports progress on t and returns the task as stored afterwards.
func (e *executor) update(ctx context.Context, ev model.Event, step string, t model.Task, u model.TaskUpdate) (model.Task, error) {
	data, err := json.Marshal(u)
	if err != nil {
		return model.Task{}, err
	}
	_, err = e.client.Tasks().Act(ctx, model.TaskAction{
		Command: model.Command{Call: callOf(ev), Operation: opID(ev, step), ExpectedRevision: t.Revision},
		Target:  t.Ref,
		Action:  model.Extension{Name: model.SchemaTaskUpdate, Version: 1, Data: data},
	})
	if err != nil {
		return model.Task{}, err
	}
	return e.client.Tasks().Get(ctx, model.Lookup[model.TaskRef]{Call: callOf(ev), Ref: t.Ref})
}

func (e *executor) cancel(ctx context.Context, ev model.Event, t model.Task) error {
	_, err := e.update(ctx, ev, "cancel", t, model.TaskUpdate{State: model.TaskCanceled})
	return err
}

// taskUpdated honors a cancel request: a remote agent asked, uma recorded
// it, and only the host moves the task to canceled.
func (e *executor) taskUpdated(ctx context.Context, ev model.Event) error {
	var p model.TaskUpdatedPayload
	if err := decode(ev, model.SchemaTaskUpdated, &p); err != nil || !slices.Contains(p.Changes, model.TaskFieldCancelRequested) {
		return err
	}
	t, err := e.client.Tasks().Get(ctx, model.Lookup[model.TaskRef]{Call: callOf(ev), Ref: p.Task})
	if err != nil || !t.CancelRequested || model.IsTerminalTaskState(t.State) {
		return err
	}
	return e.cancel(ctx, ev, t)
}

// decode reads the payload of ev, which must be schema version 1.
func decode(ev model.Event, schema string, into any) error {
	if ev.Payload.Name != schema || ev.Payload.Version != model.PayloadVersion1 {
		return &model.Error{Code: model.SchemaMismatch, SafeMessage: "executor: unsupported " + schema + " payload", Retry: model.NoRetry}
	}
	if err := json.Unmarshal(ev.Payload.Data, into); err != nil {
		return errors.Join(&model.Error{Code: model.SchemaMismatch, SafeMessage: "executor: malformed " + schema + " payload", Retry: model.NoRetry}, err)
	}
	return nil
}

func callOf(ev model.Event) model.Call {
	if c := ev.Correlation; c.Validate() == nil {
		return model.Call{Correlation: c}
	}
	return model.Call{Correlation: "desk"}
}

// opID is the operation ID of one write for an event: the same on every
// delivery of the event, so a repeat replays the first.
func opID(ev model.Event, step string) model.OperationID {
	return model.OperationID("desk." + string(ev.ID) + "." + step)
}

func textContent(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}
