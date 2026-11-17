package model

import (
	"slices"
	"strconv"
)

// MaxMergeIdentities bounds the identities one Contacts.Merge moves. A
// contact holding more is model.Validation: relink the surplus with Link
// first.
const MaxMergeIdentities = 500

// MaxMergePreviewConversations bounds the conversations a MergePreview
// lists; MergePreview.MoreConversations reports a longer list.
const MaxMergePreviewConversations = 1000

// PreviewMerge asks what MergeContacts of From into Into would move. It
// changes nothing.
type PreviewMerge struct {
	Call
	From ContactRef `json:"from"`
	Into ContactRef `json:"into"`
}

// Validate checks the call and that From and Into are two full contact
// refs.
func (r PreviewMerge) Validate() error {
	return firstError(r.Call.Validate(), checkMergePair(r.From, r.Into))
}

// MergePreview is what a merge of From into Into would move, as of the
// contacts' revisions it carries. Pass them to MergeContacts as
// ExpectedRevision and ExpectedIntoRevision, and Identities as its
// Identities, to merge exactly what was previewed.
//
// Identities lists every identity on From, in order of recording.
// Conversations lists the conversations those identities own
// (Conversation.Owner) that the principal may read. They keep their owner
// and their reply route: a merge changes the contact the owner belongs to,
// not the conversation.
type MergePreview struct {
	From              ContactRef        `json:"from"`
	Into              ContactRef        `json:"into"`
	FromRevision      Revision          `json:"from_revision"`
	IntoRevision      Revision          `json:"into_revision"`
	Identities        []IdentityRef     `json:"identities,omitempty"`
	Conversations     []ConversationRef `json:"conversations,omitempty"`
	MoreConversations bool              `json:"more_conversations"`
}

// Clone returns a deep copy.
func (p MergePreview) Clone() MergePreview {
	p.Identities = slices.Clone(p.Identities)
	p.Conversations = slices.Clone(p.Conversations)
	return p
}

// MergeContacts merges contact From into contact Into: it moves every
// identity on From to Into, appends one link history record of class merge
// for each, leaves From in place with MergedInto set to Into, and appends
// one contact.merged event. ExpectedRevision is From's revision and
// ExpectedIntoRevision Into's; both are required. Identities is the set
// the caller reviewed through PreviewMerge, and must be exactly the
// identities on From at commit. Reason is the actor's audit reason code,
// one lowercase name segment, recorded on every link record; it outlives
// redaction, so it must carry no personal data.
type MergeContacts struct {
	Command
	From                 ContactRef    `json:"from"`
	Into                 ContactRef    `json:"into"`
	ExpectedIntoRevision Revision      `json:"expected_into_revision"`
	Identities           []IdentityRef `json:"identities,omitempty"`
	Reason               string        `json:"reason"`
}

// RevisionRule is RevisionRequired: compared with From's revision.
func (MergeContacts) RevisionRule() RevisionRule { return RevisionRequired }

// Validate checks the command, the two contacts, Into's expected revision,
// the reviewed identities (at most MaxMergeIdentities, no duplicates) and
// the reason code.
func (r MergeContacts) Validate() error {
	var ids error
	for i, id := range r.Identities {
		if ids = id.validate(fieldPath("identities", strconv.Itoa(i)), false); ids != nil {
			break
		}
	}
	switch {
	case ids != nil:
	case len(r.Identities) > MaxMergeIdentities:
		ids = invalidField("identities", "exceeds maximum merge size")
	case hasDuplicate(r.Identities):
		ids = invalidField("identities", "must not repeat an identity")
	}
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		checkMergePair(r.From, r.Into),
		checkToken("expected_into_revision", string(r.ExpectedIntoRevision)),
		ids,
		checkReasonCode("reason", r.Reason),
	)
}

func checkMergePair(from, into ContactRef) error {
	if err := firstError(from.validate("from", false), into.validate("into", false)); err != nil {
		return err
	}
	if from == into {
		return invalidField("into", "must differ from the contact merged")
	}
	return nil
}

func hasDuplicate(ids []IdentityRef) bool {
	seen := make(map[IdentityRef]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			return true
		}
		seen[id] = struct{}{}
	}
	return false
}

// ContactMergedPayload is the contact.merged payload, one per merge:
// references only. Link names the link history records, one per moved
// identity and in the order of Identities.
type ContactMergedPayload struct {
	From       ContactRef    `json:"from"`
	Into       ContactRef    `json:"into"`
	Identities []IdentityRef `json:"identities,omitempty"`
	Links      []ID          `json:"links,omitempty"`
	Operation  OperationRef  `json:"operation"`
}
