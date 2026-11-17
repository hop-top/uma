package model

// Receipt kinds (Receipt.Kind): what a receipt tells the sender of a
// received message.
const (
	// ReceiptDelivered tells the sender the message reached its recipient.
	ReceiptDelivered = "delivered"
	// ReceiptRead tells the sender the message was read.
	ReceiptRead = "read"
)

// IsReceiptKind reports whether k is a receipt kind: ReceiptDelivered or
// ReceiptRead.
func IsReceiptKind(k string) bool { return k == ReceiptDelivered || k == ReceiptRead }

// Receipt sends a delivery or read receipt for a received message to the
// provider (Messages.Receipt): Kind is ReceiptDelivered or ReceiptRead and
// Message a message that arrived from the provider. ExpectedRevision must
// be empty: a receipt changes nothing in uma and does not depend on the
// message's revision.
type Receipt struct {
	Command
	Message MessageRef `json:"message"`
	Kind    string     `json:"kind"`
}

// RevisionRule is RevisionAbsent: a receipt is about a message, not a
// version of it.
func (Receipt) RevisionRule() RevisionRule { return RevisionAbsent }

// Validate checks the command, the message and that Kind is a receipt
// kind.
func (r Receipt) Validate() error {
	var kind error
	if !IsReceiptKind(r.Kind) {
		kind = invalidField("kind", "must be delivered or read")
	}
	return firstError(r.Command.Validate(r.RevisionRule()), r.Message.validate("message", false), kind)
}
