package google

import (
	"context"
	"slices"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/model"
)

// Inbound conversation state. Gmail history names what changed, not the
// state it left: a labelAdded or labelRemoved record names a message and
// the labels, and a messageDeleted record a message that is gone. The
// receiver turns each such record into the state of the message's thread as
// Gmail holds it when the record is read (users.threads.get), one
// conversation.state envelope per thread per history page, so a burst of
// changes is one report and a report is always the thread as it is now. The
// full synchronization reports the thread of every message it brings in.
//
// The conversation is named by the thread's newest message that is not a
// draft (its Gmail message reference), which the engine resolves to the
// conversation holding that message; a thread whose messages uma never
// stored is a conversation the engine does not hold, and its report records
// nothing. A thread Gmail no longer holds reports nothing.
//
// Gmail dates no label change, so a report is observed when the receiver
// read the thread.

// threadHeaders are the header fields read of a thread's messages, enough
// to find which of the mailbox's addresses a message was sent to.
var threadHeaders = []string{"To", "Cc"}

// stateEventID is the event ID of the report of thread id at history
// position at: the same record read again is the same report.
func stateEventID(id string, at int64s) string { return "state:" + id + ":" + at.String() }

// reportThread reads thread id and hands its state to the sink. at is the
// history position the report is made for.
func (i *instance) reportThread(ctx context.Context, s *session, sink adapter.Sink, id string, at int64s) error {
	t, err := i.getThread(ctx, s, id, "metadata", threadHeaders...)
	if model.IsCode(err, model.NotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	msgs := slices.DeleteFunc(slices.Clone(t.Messages), func(m gMessage) bool { return slices.Contains(m.LabelIDs, labelDraft) })
	if len(msgs) == 0 {
		return nil
	}
	now := i.now().UTC()
	state := threadState(msgs, now)
	i.nameLabels(ctx, s, &state)
	env := model.Envelope{
		EventID:     stateEventID(id, at),
		Destination: i.threadDestination(msgs),
		Kind:        model.EnvelopeConversationState,
		Assurance:   assuranceUnverified,
		Audience:    model.Audience{Visibility: model.Public},
		OccurredAt:  now,
		State: &model.ProviderStateReport{
			Conversation: model.ProviderMessageRef(i.ch.Ref, msgs[len(msgs)-1].ID),
			State:        state,
		},
	}
	_, err = sink.Accept(ctx, model.Ingress{Envelope: env})
	return err
}

// threadDestination is the address of the mailbox a thread's mail was sent
// to: that of the newest inbox message, else the primary address.
func (i *instance) threadDestination(msgs []gMessage) string {
	for _, m := range slices.Backward(msgs) {
		if !slices.Contains(m.LabelIDs, labelInbox) || m.Payload == nil {
			continue
		}
		var block strings.Builder
		for _, h := range m.Payload.Headers {
			if h.Name != "" && !strings.ContainsAny(h.Name, ":\r\n ") {
				block.WriteString(h.Name + ": " + strings.NewReplacer("\r", " ", "\n", " ").Replace(h.Value) + "\r\n")
			}
		}
		block.WriteString("\r\n")
		if msg, err := mime.Parse(strings.NewReader(block.String()), mime.Limits{}); err == nil {
			return i.destination(msg.Header)
		}
	}
	return i.destination(mime.Header{})
}

// handledLabels are the labels that become a field of the state; every
// other label of the thread's messages is a provider label.
var handledLabels = []string{
	labelInbox, labelUnread, labelStarred, labelImportant, labelTrash, labelSpam, labelDraft, labelSent,
}

// threadState is the state of a thread whose non-draft messages are msgs,
// all of them observed at the given time:
//
//   - trashed: every message is in the trash;
//   - archived: no message is in the inbox, as Gmail has it: archiving is
//     removing INBOX. A trashed or spam thread has none either, so it is
//     archived as well as trashed, and a thread that was never in the inbox
//     is archived;
//   - read: no message is unread;
//   - flags: star when any message is starred, important when any is
//     marked important;
//   - labels: every other label of any message, sorted, named by ID.
func threadState(msgs []gMessage, at time.Time) model.ProviderConversationState {
	any := func(l string) bool {
		return slices.ContainsFunc(msgs, func(m gMessage) bool { return slices.Contains(m.LabelIDs, l) })
	}
	trashed := !slices.ContainsFunc(msgs, func(m gMessage) bool { return !slices.Contains(m.LabelIDs, labelTrash) })
	read := !any(labelUnread)
	st := model.ProviderConversationState{
		Trashed:    trashed,
		Archived:   !any(labelInbox),
		Read:       &read,
		ObservedAt: at,
		Complete:   true,
	}
	if any(labelStarred) {
		st.Flags = append(st.Flags, model.FlagStar)
	}
	if any(labelImportant) {
		st.Flags = append(st.Flags, model.FlagImportant)
	}
	var ids []string
	for _, m := range msgs {
		for _, l := range m.LabelIDs {
			if !slices.Contains(handledLabels, l) && !slices.Contains(ids, l) {
				ids = append(ids, l)
			}
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		st.Labels = append(st.Labels, model.ProviderLabel{ID: id, Name: id})
	}
	return st
}

// stateThreads are the threads a history record changed the state of, in
// the record's order, each once: those of its label changes and deletions.
func (h gHistory) stateThreads() []string {
	var out []string
	add := func(m gMessage) {
		if m.ThreadID != "" && !slices.Contains(out, m.ThreadID) {
			out = append(out, m.ThreadID)
		}
	}
	for _, c := range h.LabelsAdded {
		add(c.Message)
	}
	for _, c := range h.LabelsRemoved {
		add(c.Message)
	}
	for _, d := range h.MessagesDeleted {
		add(d.Message)
	}
	return out
}
