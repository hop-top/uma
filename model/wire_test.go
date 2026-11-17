package model

import (
	"bytes"
	"encoding/json"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/wire golden files")

// wireTypes is every exported struct type that crosses a wire or storage
// boundary, keyed by its declared name. TestWireTagsComplete fails when a
// declared struct type is in neither wireTypes nor notWire.
var wireTypes = map[string]any{
	"Acceptance":                 Acceptance{},
	"Acknowledge":                Acknowledge{},
	"Attachment":                 Attachment{},
	"AttachmentFilter":           AttachmentFilter{},
	"AttachmentMetadata":         AttachmentMetadata{},
	"AttachmentRef":              AttachmentRef{},
	"Attribute":                  Attribute{},
	"Audience":                   Audience{},
	"Author":                     Author{},
	"Call":                       Call{},
	"CapabilityStatus":           CapabilityStatus{},
	"Channel":                    Channel{},
	"ChannelRef":                 ChannelRef{},
	"Command":                    Command{},
	"ConfigureChannel":           ConfigureChannel{},
	"ConfigureInbox":             ConfigureInbox{},
	"ConfigureLabel":             ConfigureLabel{},
	"Contact":                    Contact{},
	"ContactRef":                 ContactRef{},
	"ContactMergedPayload":       ContactMergedPayload{},
	"MergeContacts":              MergeContacts{},
	"MergePreview":               MergePreview{},
	"PreviewMerge":               PreviewMerge{},
	"Content":                    Content{},
	"Conversation":               Conversation{},
	"ConversationRef":            ConversationRef{},
	"ConversationChangedPayload": ConversationChangedPayload{},
	"ConversationLabelPayload":   ConversationLabelPayload{},
	"ConversationLabels":         ConversationLabels{},
	"DeleteMessage":              DeleteMessage{},
	"DeleteLabel":                DeleteLabel{},
	"DeliveryFailure":            DeliveryFailure{},
	"DeliveryReport":             DeliveryReport{},
	"DeliveryUpdatedPayload":     DeliveryUpdatedPayload{},
	"EditMessage":                EditMessage{},
	"EmailHeaders":               EmailHeaders{},
	"EndStream":                  EndStream{},
	"Envelope":                   Envelope{},
	"Ephemeral":                  Ephemeral{},
	"Error":                      Error{},
	"Event":                      Event{},
	"EventBatch":                 EventBatch{},
	"Evidence":                   Evidence{},
	"ExecuteExtension":           ExecuteExtension{},
	"ExportRequest":              ExportRequest{},
	"Export":                     Export{},
	"EventRef":                   EventRef{},
	"ExportRef":                  ExportRef{},
	"ExportLine":                 ExportLine{},
	"Extension":                  Extension{},
	"ExternalRef":                ExternalRef{},
	"Filter":                     Filter{},
	"Identity":                   Identity{},
	"IdentityKey":                IdentityKey{},
	"IdentityLink":               IdentityLink{},
	"IdentityLinkedPayload":      IdentityLinkedPayload{},
	"IdentityRef":                IdentityRef{},
	"Inbox":                      Inbox{},
	"InboxRef":                   InboxRef{},
	"Ingress":                    Ingress{},
	"IngressReceipt":             IngressReceipt{},
	"Interaction":                Interaction{},
	"InteractionDecision":        InteractionDecision{},
	"InteractionInput":           InteractionInput{},
	"InteractionRef":             InteractionRef{},
	"InteractionResponse":        InteractionResponse{},
	"InteractionReceivedPayload": InteractionReceivedPayload{},
	"InteractionUpdatedPayload":  InteractionUpdatedPayload{},
	"Respond":                    Respond{},
	"InternalNote":               InternalNote{},
	"Label":                      Label{},
	"LabelRef":                   LabelRef{},
	"LabelSelector":              LabelSelector{},
	"LinkIdentity":               LinkIdentity{},
	"ListRequest":                ListRequest{},
	"ListEvents":                 ListEvents{},
	"Lookup":                     Lookup[MessageRef]{},
	"LookupKey":                  LookupKey{},
	"Message":                    Message{},
	"MessageCreatedPayload":      MessageCreatedPayload{},
	"MessageRef":                 MessageRef{},
	"MessageReceivedPayload":     MessageReceivedPayload{},
	"MessageUpdatedPayload":      MessageUpdatedPayload{},
	"NormalizedEmail":            NormalizedEmail{},
	"OpenSubscription":           OpenSubscription{},
	"HeldBlock":                  HeldBlock{},
	"HeldEdit":                   HeldEdit{},
	"DecideHeld":                 DecideHeld{},
	"EditHeld":                   EditHeld{},
	"Decided":                    Decided{},
	"HoldProposal":               HoldProposal{},
	"TrustEntry":                 TrustEntry{},
	"TrustLookup":                TrustLookup{},
	"TrustFilter":                TrustFilter{},
	"ResetTrust":                 ResetTrust{},
	"ResetTrustSet":              ResetTrustSet{},
	"ImportTrust":                ImportTrust{},
	"Operation":                  Operation{},
	"OperationLookup":            OperationLookup{},
	"OperationKey":               OperationKey{},
	"OperationProgress":          OperationProgress{},
	"OperationRef":               OperationRef{},
	"Origin":                     Origin{},
	"Page":                       Page[MessageRef]{},
	"PageRequest":                PageRequest{},
	"Part":                       Part{},
	"Participant":                Participant{},
	"PayloadReaction":            PayloadReaction{},
	"Poll":                       Poll{},
	"PrepareAttachment":          PrepareAttachment{},
	"Principal":                  Principal{},
	"QueryExtension":             QueryExtension{},
	"Reaction":                   Reaction{},
	"Receipt":                    Receipt{},
	"ParticipantChange":          ParticipantChange{},
	"ArchiveConversation":        ArchiveConversation{},
	"MoveConversation":           MoveConversation{},
	"MoveDestination":            MoveDestination{},
	"SetConversationRead":        SetConversationRead{},
	"FlagConversation":           FlagConversation{},
	"TrashConversation":          TrashConversation{},
	"Folder":                     Folder{},
	"ProviderLabel":              ProviderLabel{},
	"ProviderConversationState":  ProviderConversationState{},
	"ProviderMailbox":            ProviderMailbox{},
	"ProviderMailboxes":          ProviderMailboxes{},
	"CreateProviderMailbox":      CreateProviderMailbox{},
	"ProviderStateReport":        ProviderStateReport{},
	"RawMessage":                 RawMessage{},
	"Redact":                     Redact{},
	"Ref":                        Ref{},
	"ReplyRecipients":            ReplyRecipients{},
	"RecipientLimits":            RecipientLimits{},
	"Reply":                      Reply{},
	"Selection":                  Selection{},
	"Signal":                     Signal{},
	"StartStream":                StartStream{},
	"StartConversation":          StartConversation{},
	"Stream":                     Stream{},
	"StatusChange":               StatusChange{},
	"StreamRef":                  StreamRef{},
	"StreamUpdate":               StreamUpdate{},
	"Subscription":               Subscription{},
	"SubscriptionRef":            SubscriptionRef{},
	"SubscriptionResult":         SubscriptionResult{},
	"Task":                       Task{},
	"TaskAction":                 TaskAction{},
	"TaskRef":                    TaskRef{},
	"Transition":                 Transition{},
	"ValidationIssue":            ValidationIssue{},
	"WorkItem":                   WorkItem{},
	"WorkItemAction":             WorkItemAction{},
	"WorkItemRef":                WorkItemRef{},
	"WorkItemStatusChange":       WorkItemStatusChange{},
	"WorkItemUpdatedPayload":     WorkItemUpdatedPayload{},
	"WorkItemBindingChange":      WorkItemBindingChange{},
	"WorkItemSource":             WorkItemSource{},
	"WorkItemState":              WorkItemState{},
	"WorkItemUpdate":             WorkItemUpdate{},
	"UnlinkIdentity":             UnlinkIdentity{},
	"CreateContact":              CreateContact{},
	"UpdateContact":              UpdateContact{},
	"ContactCreatedPayload":      ContactCreatedPayload{},
	"ContactUpdatedPayload":      ContactUpdatedPayload{},
	"LinkRequest":                LinkRequest{},
	"EvidenceBinding":            EvidenceBinding{},
	"AttestationFacts":           AttestationFacts{},
	"ImportFacts":                ImportFacts{},
	"VerifiedAttributeFacts":     VerifiedAttributeFacts{},
	"HostSessionFacts":           HostSessionFacts{},
	"ChallengeFacts":             ChallengeFacts{},
	"MessageReaction":            MessageReaction{},
	"Edit":                       Edit{},
	"PendingMutation":            PendingMutation{},
	"PendingReaction":            PendingReaction{},
	"PendingState":               PendingState{},
	"StateLimits":                StateLimits{},
	"ReactionSummary":            ReactionSummary{},
	"ReactionCount":              ReactionCount{},
	"Artifact":                   Artifact{},
	"ArtifactUpdate":             ArtifactUpdate{},
	"TaskBinding":                TaskBinding{},
	"TaskOpen":                   TaskOpen{},
	"TaskUpdate":                 TaskUpdate{},
	"TaskUpdatedPayload":         TaskUpdatedPayload{},

	// Conversation workflow state.
	"Snooze":                        Snooze{},
	"AssigneeFilter":                AssigneeFilter{},
	"ConversationAssignment":        ConversationAssignment{},
	"ConversationSnooze":            ConversationSnooze{},
	"ConversationAssignmentPayload": ConversationAssignmentPayload{},
	"ConversationSnoozePayload":     ConversationSnoozePayload{},
	"ConversationLink":              ConversationLink{},
	"ConversationLinkChange":        ConversationLinkChange{},
	"ConversationLinkPayload":       ConversationLinkPayload{},
	"ReadMarker":                    ReadMarker{},
	"ReadMarkerPayload":             ReadMarkerPayload{},
	"ReadMark":                      ReadMark{},
	"UnreadFilter":                  UnreadFilter{},
	"HeldOperationPayload":          HeldOperationPayload{},
}

// notWire lists exported struct types that are intentionally not wire types,
// each with the reason.
var notWire = map[string]string{
	"ContentLimits":     "engine configuration passed in Go; never encoded",
	"DecodedTaskAction": "decoder result passed in Go; the action itself crosses the wire as an Extension",
	"ReactionStep":      "a change computed in Go to apply to a message; the stored result is the reaction set",
}

var snakeCase = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

// TestWireTagsComplete parses the package source and requires a snake_case
// json tag on every exported field of every exported struct type, omitempty
// exactly on pointer, slice, map and raw JSON fields, an _ms key on every
// duration, and a golden fixture for every wire type. Embedded structs stay untagged so their fields promote.
func TestWireTagsComplete(t *testing.T) {
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok || !spec.Name.IsExported() {
				return true
			}
			st, ok := spec.Type.(*ast.StructType)
			if !ok {
				return true
			}
			name := spec.Name.Name
			declared[name] = true
			if _, skip := notWire[name]; skip {
				return true
			}
			for _, field := range st.Fields.List {
				checkFieldTag(t, fset, name, field)
			}
			return true
		})
	}
	for name := range declared {
		_, wire := wireTypes[name]
		_, skip := notWire[name]
		switch {
		case wire && skip:
			t.Errorf("%s is both a wire type and allow-listed", name)
		case !wire && !skip:
			t.Errorf("%s has no golden fixture; add it to wireTypes or notWire", name)
		}
	}
	for name := range wireTypes {
		if !declared[name] {
			t.Errorf("wireTypes lists %s, which the package does not declare", name)
		}
	}
	for name := range notWire {
		if !declared[name] {
			t.Errorf("notWire lists %s, which the package does not declare", name)
		}
	}
}

func checkFieldTag(t *testing.T, fset *token.FileSet, typeName string, field *ast.Field) {
	t.Helper()
	at := fset.Position(field.Pos())
	if len(field.Names) == 0 {
		if field.Tag != nil {
			t.Errorf("%s: %s: embedded field must stay untagged", at, typeName)
		}
		return
	}
	for _, id := range field.Names {
		if !id.IsExported() {
			continue
		}
		where := typeName + "." + id.Name
		if field.Tag == nil {
			t.Errorf("%s: %s has no json tag", at, where)
			continue
		}
		tag, ok := reflect.StructTag(strings.Trim(field.Tag.Value, "`")).Lookup("json")
		if !ok {
			t.Errorf("%s: %s has no json tag", at, where)
			continue
		}
		key, opts, _ := strings.Cut(tag, ",")
		if !snakeCase.MatchString(key) {
			t.Errorf("%s: %s json key %q is not snake_case", at, where, key)
		}
		want := ""
		if optional(field.Type) {
			want = "omitempty"
		}
		if opts != want {
			t.Errorf("%s: %s json options %q, want %q", at, where, opts, want)
		}
		if isDuration(field.Type) && !strings.HasSuffix(key, "_ms") {
			t.Errorf("%s: %s is a duration; its json key %q needs the _ms suffix", at, where, key)
		}
	}
}

// isDuration reports whether a field type is time.Duration, which crosses the
// wire as integer milliseconds.
func isDuration(expr ast.Expr) bool {
	e, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := e.X.(*ast.Ident)
	return ok && pkg.Name == "time" && e.Sel.Name == "Duration"
}

// optional reports whether a field type is a pointer, slice, map or raw JSON:
// the kinds whose nil or empty value means absent.
func optional(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.StarExpr, *ast.MapType:
		return true
	case *ast.ArrayType:
		return e.Len == nil
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		return ok && pkg.Name == "json" && e.Sel.Name == "RawMessage"
	}
	return false
}

// TestWireGolden encodes a fully populated value of every wire type and
// compares it byte for byte with testdata/wire/<Type>.json, then decodes the
// encoding and requires the original value back. Run with -update to rewrite
// the golden files.
func TestWireGolden(t *testing.T) {
	names := make([]string, 0, len(wireTypes))
	for name := range wireTypes {
		names = append(names, name)
	}
	slices.Sort(names)
	testdata := openTestdata(t)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			typ := reflect.TypeOf(wireTypes[name])
			v := reflect.New(typ)
			populate(t, v.Elem(), typ.Name())
			compact, err := json.Marshal(v.Interface())
			if err != nil {
				t.Fatal(err)
			}
			var got bytes.Buffer
			if err := json.Indent(&got, compact, "", "  "); err != nil {
				t.Fatal(err)
			}
			got.WriteByte('\n')
			path := filepath.Join("testdata", "wire", name+".json")
			file := filepath.Join("wire", name+".json")
			if *update {
				if err := testdata.MkdirAll("wire", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := testdata.WriteFile(file, got.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := testdata.ReadFile(file)
			if err != nil {
				t.Fatalf("%v (run go test -update to create it)", err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Errorf("encoding of %s differs from %s:\n got: %s\nwant: %s", name, path, got.Bytes(), want)
			}
			back := reflect.New(typ)
			if err := json.Unmarshal(compact, back.Interface()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(back.Elem().Interface(), v.Elem().Interface()) {
				t.Errorf("%s does not round-trip:\n got: %#v\nwant: %#v", name, back.Elem().Interface(), v.Elem().Interface())
			}
		})
	}
}

// openTestdata opens the testdata directory as a root, so golden reads and
// writes cannot leave it.
func openTestdata(t *testing.T) *os.Root {
	t.Helper()
	root, err := os.OpenRoot("testdata")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

var (
	durationType = reflect.TypeFor[time.Duration]()
	rawType      = reflect.TypeFor[json.RawMessage]()
	goldenAt     = time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
)

// populate sets every exported field reachable from v to a non-zero value:
// strings to their field name, numbers to 1, durations to 1500ms, bools to
// true, times to a fixed UTC instant, pointers to a populated value and
// slices and maps to one populated element. Unsupported kinds fail the test, so no field can stay
// zero and drop out of the golden file.
func populate(t *testing.T, v reflect.Value, name string) {
	t.Helper()
	switch {
	case v.Type() == timeType:
		v.Set(reflect.ValueOf(goldenAt))
		return
	case v.Type() == durationType:
		v.SetInt(int64(1500 * time.Millisecond))
		return
	case v.Type() == rawType:
		v.SetBytes([]byte(`{"field":"` + name + `"}`))
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(name)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		populate(t, v.Elem(), name)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		populate(t, v.Index(0), name)
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		key := reflect.New(v.Type().Key()).Elem()
		populate(t, key, name+"Key")
		elem := reflect.New(v.Type().Elem()).Elem()
		populate(t, elem, name)
		v.SetMapIndex(key, elem)
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if f.IsExported() {
				populate(t, v.Field(i), f.Name)
			}
		}
	default:
		t.Fatalf("populate: %s has unsupported kind %s", name, v.Kind())
	}
}

// TestWireDurations: durations encode as integer milliseconds, rounded toward
// zero, and negative values are rejected on decode.
func TestWireDurations(t *testing.T) {
	type wire interface{ json.Marshaler }
	cases := map[string]struct {
		in  func(time.Duration) wire
		key string
		out func() (json.Unmarshaler, func() time.Duration)
	}{
		"error": {
			in:  func(d time.Duration) wire { return Error{Code: RateLimited, RetryAfter: d} },
			key: "retry_after_ms",
			out: func() (json.Unmarshaler, func() time.Duration) {
				var e Error
				return &e, func() time.Duration { return e.RetryAfter }
			},
		},
		"start stream": {
			in:  func(d time.Duration) wire { return StartStream{Mode: StreamNative, Deadline: d} },
			key: "deadline_ms",
			out: func() (json.Unmarshaler, func() time.Duration) {
				var s StartStream
				return &s, func() time.Duration { return s.Deadline }
			},
		},
		"poll": {
			in:  func(d time.Duration) wire { return Poll{Limit: 1, Wait: d} },
			key: "wait_ms",
			out: func() (json.Unmarshaler, func() time.Duration) {
				var p Poll
				return &p, func() time.Duration { return p.Wait }
			},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				in   time.Duration
				ms   int64
				back time.Duration
			}{
				{1500 * time.Millisecond, 1500, 1500 * time.Millisecond},
				{1999999 * time.Nanosecond, 1, time.Millisecond},
				{999 * time.Microsecond, 0, 0},
			} {
				b, err := json.Marshal(c.in(tc.in))
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(b, &fields); err != nil {
					t.Fatal(err)
				}
				if got, want := string(fields[c.key]), strconv.FormatInt(tc.ms, 10); got != want {
					t.Errorf("%v: %s = %s, want %s (%s)", tc.in, c.key, got, want, b)
				}
				dst, get := c.out()
				if err := json.Unmarshal(b, dst); err != nil {
					t.Fatal(err)
				}
				if got := get(); got != tc.back {
					t.Errorf("%v: decoded %v, want %v", tc.in, got, tc.back)
				}
			}
			dst, _ := c.out()
			err := json.Unmarshal([]byte(`{"`+c.key+`":-1}`), dst)
			requireError(t, err, Validation, c.key)
		})
	}
}
