package widget

// InboundMessageID is the provider message ID the receiver gives an
// anonymous visitor's message with clientID.
func InboundMessageID(visitor, clientID string) string {
	return inboundID("message", claims{Visitor: visitor, Assurance: AssuranceAnonymous}, clientID)
}
