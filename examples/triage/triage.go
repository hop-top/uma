package main

import (
	"context"
	"slices"
	"strings"

	"hop.top/uma"
	"hop.top/uma/model"
)

// Verdict is what a Rule decides for one conversation. Trash outranks the
// rest: a conversation headed for the trash is neither archived nor flagged.
type Verdict struct {
	Archive bool
	Flag    bool
	Trash   bool
}

// Rule decides from the newest inbound message of a conversation: its
// sender address and its text.
type Rule func(sender, text string) Verdict

// DefaultRule is the demo rule. Mail that looks like junk is trashed, mail
// that asks for attention is flagged and stays in the inbox, and bulk mail
// is archived. The first match wins.
func DefaultRule(sender, text string) Verdict {
	sender, text = strings.ToLower(sender), strings.ToLower(text)
	switch {
	case strings.Contains(text, "you have won") || strings.HasSuffix(sender, "@spam.example"):
		return Verdict{Trash: true}
	case strings.Contains(text, "urgent"):
		return Verdict{Flag: true}
	case strings.Contains(text, "unsubscribe") || strings.HasSuffix(sender, "@newsletter.example"):
		return Verdict{Archive: true}
	}
	return Verdict{}
}

// Action is one provider state change a sweep queued.
type Action struct {
	Conversation model.ConversationRef
	Verb         model.StateVerb
	// Kind is the flag kind, set for a flag.
	Kind      string
	Operation model.OperationRef
}

// Sweep reads the inbox's conversations, applies rule to each and queues the
// state changes it asks for through Conversations. Nothing reaches a
// provider until a worker runs the queued operations (Runtime.Run, or
// Runtime.Step as Host.Pass does).
//
// A sweep is repeatable. Operation IDs derive from the conversation and the
// verb, so asking again returns the operation already queued, and a
// conversation whose provider state already shows the change is skipped. Only
// verbs the conversation's channel reports as available are asked for: a
// provider without a trash, or a flag kind the rule wants, is left alone
// rather than refused.
func Sweep(ctx context.Context, c uma.Client, inbox model.InboxRef, rule Rule) ([]Action, error) {
	var out []Action
	var after model.Cursor
	for {
		page, err := c.Conversations().List(ctx, model.ListRequest{
			Call:   model.Call{Correlation: "triage"},
			Filter: model.Filter{Inboxes: []model.InboxRef{inbox}},
			Page:   model.PageRequest{Limit: uma.MaxPageLimit, After: after},
		})
		if err != nil {
			return out, err
		}
		for _, conv := range page.Items {
			acts, err := sweepOne(ctx, c, conv, rule)
			out = append(out, acts...)
			if err != nil {
				return out, err
			}
		}
		if page.Complete || page.Next == "" {
			return out, nil
		}
		after = page.Next
	}
}

func sweepOne(ctx context.Context, c uma.Client, conv model.Conversation, rule Rule) ([]Action, error) {
	msg, ok, err := newestInbound(ctx, c, conv.Ref)
	if err != nil || !ok {
		return nil, err
	}
	verdict := rule(msg.Author.External.ID, text(msg))
	if verdict == (Verdict{}) {
		return nil, nil
	}
	if verdict.Trash {
		verdict = Verdict{Trash: true}
	}
	caps, err := c.Conversations().Capabilities(ctx, model.Lookup[model.ConversationRef]{
		Call: model.Call{Correlation: "triage"}, Ref: conv.Ref,
	})
	if err != nil {
		return nil, err
	}
	var out []Action
	queue := func(a Action, accept func(model.Command) (model.Acceptance, error)) error {
		a.Conversation = conv.Ref
		cmd := model.Command{
			Call:      model.Call{Correlation: "triage"},
			Operation: model.OperationID("triage/" + string(conv.Ref.ID) + "/" + string(a.Verb) + a.Kind),
		}
		acc, err := accept(cmd)
		if err != nil {
			return err
		}
		a.Operation = acc.Operation.Ref
		out = append(out, a)
		return nil
	}

	// What each channel already holds comes from the provider's own reports;
	// uma's Status and Labels play no part.
	var archived, trashed bool
	var flags []string
	for _, s := range conv.ProviderState {
		archived, trashed = archived || s.Archived, trashed || s.Trashed
		flags = append(flags, s.Flags...)
	}
	if trashed {
		return nil, nil
	}

	if verdict.Trash && available(caps, model.CapabilityTrash) {
		err = queue(Action{Verb: model.StateVerbTrash}, func(cmd model.Command) (model.Acceptance, error) {
			return c.Conversations().Trash(ctx, model.TrashConversation{Command: cmd, Conversation: conv.Ref})
		})
		return out, err
	}
	if verdict.Archive && !archived && available(caps, model.CapabilityArchive) {
		err = queue(Action{Verb: model.StateVerbArchive}, func(cmd model.Command) (model.Acceptance, error) {
			return c.Conversations().Archive(ctx, model.ArchiveConversation{Command: cmd, Conversation: conv.Ref})
		})
		if err != nil {
			return out, err
		}
	}
	if kind := flagKind(caps); verdict.Flag && kind != "" && !slices.Contains(flags, kind) {
		err = queue(Action{Verb: model.StateVerbFlag, Kind: kind}, func(cmd model.Command) (model.Acceptance, error) {
			return c.Conversations().Flag(ctx, model.FlagConversation{Command: cmd, Conversation: conv.Ref, Kind: kind})
		})
	}
	return out, err
}

// available reports whether the channel reports c available.
func available(caps []model.CapabilityStatus, c model.Capability) bool {
	for _, s := range caps {
		if s.Name == c {
			return s.Available
		}
	}
	return false
}

// flagKind is the strongest flag kind the channel offers: "important" where
// the provider has it (Gmail), else "star" (IMAP), else none.
func flagKind(caps []model.CapabilityStatus) string {
	for _, s := range caps {
		if s.Name != model.CapabilityFlag || !s.Available {
			continue
		}
		limits, err := model.ParseStateLimits(s.Limits)
		if err != nil {
			return ""
		}
		for _, kind := range []string{model.FlagImportant, model.FlagStar} {
			if slices.Contains(limits.Kinds, kind) {
				return kind
			}
		}
	}
	return ""
}

// newestInbound is the last message of the conversation the provider
// delivered: not one uma sent, not a deleted one.
func newestInbound(ctx context.Context, c uma.Client, ref model.ConversationRef) (model.Message, bool, error) {
	var newest model.Message
	var found bool
	var after model.Cursor
	for {
		page, err := c.Messages().List(ctx, model.ListRequest{
			Call:   model.Call{Correlation: "triage"},
			Filter: model.Filter{Conversation: &ref},
			Page:   model.PageRequest{Limit: uma.MaxPageLimit, After: after},
		})
		if err != nil {
			return model.Message{}, false, err
		}
		for _, m := range page.Items {
			if m.Origin == nil && !m.Deleted {
				newest, found = m, true
			}
		}
		if page.Complete || page.Next == "" {
			return newest, found, nil
		}
		after = page.Next
	}
}

func text(m model.Message) string {
	var b strings.Builder
	for _, p := range m.Content.Parts {
		if p.Kind == model.PartText {
			b.WriteString(p.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
