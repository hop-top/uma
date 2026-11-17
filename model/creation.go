package model

// Creation targets: a resource being created has no ID until its operation
// allocates one, so the operation targets a fixed per-kind namespace ref in
// the tenant instead. The IDs are part of stored operation keys and never
// change.
const (
	channelCreationID ID = "00000000-0000-8000-8000-00000000c4a1"
	labelCreationID   ID = "00000000-0000-8000-8000-000000001abe"
	contactCreationID ID = "00000000-0000-8000-8000-0000000c0a7c"
)

// CreationTarget returns the operation target of creating a resource of
// kind in tenant t: Configuration.ConfigureChannel with an empty channel ID
// (KindChannel), Labels.Configure with an empty label ID (KindLabel) and
// Contacts.Create (KindContact).
// It is the zero Ref for every other kind; those creations target an
// existing parent resource instead.
func CreationTarget(t TenantID, kind ResourceKind) Ref {
	var id ID
	switch kind {
	case resourceChannel:
		id = channelCreationID
	case resourceLabel:
		id = labelCreationID
	case resourceContact:
		id = contactCreationID
	default:
		return Ref{}
	}
	return Ref{Tenant: t, Kind: kind, ID: id}
}
