package model

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

var (
	labelA = LabelRef{Tenant: tenantA, ID: idA}
	labelB = LabelRef{Tenant: tenantA, ID: idB}
	labelC = LabelRef{Tenant: tenantA, ID: idC}
)

// labelRefs returns n distinct valid label refs in tenant t, sorted by ID.
func labelRefs(t TenantID, n int) []LabelRef {
	out := make([]LabelRef, n)
	for i := range out {
		out[i] = LabelRef{Tenant: t, ID: ID("00000000-0000-7000-8000-" + leftPad(strconv.Itoa(i+1), 12))}
	}
	return out
}

func leftPad(s string, n int) string { return strings.Repeat("0", n-len(s)) + s }

func TestLabelValidate(t *testing.T) {
	ok := Label{Ref: labelA, Name: "Urgent", Description: "needs a reply today", Color: "#a1b2c3", Revision: "r1"}
	requireOK(t, ok.Validate())
	requireOK(t, Label{Ref: labelA, Name: strings.Repeat("é", MaxLabelNameRunes)}.Validate())
	cases := map[string]struct {
		mut   func(*Label)
		field string
	}{
		"no tenant":          {func(l *Label) { l.Ref.Tenant = "" }, "ref.tenant"},
		"no id":              {func(l *Label) { l.Ref.ID = "" }, "ref.id"},
		"no name":            {func(l *Label) { l.Name = "" }, "name"},
		"leading space":      {func(l *Label) { l.Name = " urgent" }, "name"},
		"trailing space":     {func(l *Label) { l.Name = "urgent " }, "name"},
		"control char":       {func(l *Label) { l.Name = "ur\x07gent" }, "name"},
		"invalid utf8":       {func(l *Label) { l.Name = "ur\xffgent" }, "name"},
		"too many runes":     {func(l *Label) { l.Name = strings.Repeat("a", MaxLabelNameRunes+1) }, "name"},
		"too many bytes":     {func(l *Label) { l.Name = strings.Repeat("😀", 64) }, "name"},
		"long description":   {func(l *Label) { l.Description = strings.Repeat("d", MaxLabelDescriptionBytes+1) }, "description"},
		"nul description":    {func(l *Label) { l.Description = "a\x00b" }, "description"},
		"bad utf8 desc":      {func(l *Label) { l.Description = "\xff" }, "description"},
		"color no hash":      {func(l *Label) { l.Color = "a1b2c3" }, "color"},
		"color uppercase":    {func(l *Label) { l.Color = "#A1B2C3" }, "color"},
		"color short":        {func(l *Label) { l.Color = "#abc" }, "color"},
		"color not hex":      {func(l *Label) { l.Color = "#a1b2cg" }, "color"},
		"color with alpha":   {func(l *Label) { l.Color = "#a1b2c3d4" }, "color"},
		"color named":        {func(l *Label) { l.Color = "red" }, "color"},
		"color space":        {func(l *Label) { l.Color = " #a1b2c3" }, "color"},
		"name byte limit ok": {func(l *Label) { l.Name = strings.Repeat("€", 64) }, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			l := ok
			tc.mut(&l)
			if tc.field == "" {
				requireOK(t, l.Validate())
				return
			}
			requireError(t, l.Validate(), Validation, tc.field)
		})
	}
	requireOK(t, Label{Ref: labelA, Name: "x", Description: strings.Repeat("d", MaxLabelDescriptionBytes)}.Validate())
}

// Names compare under simple case folding: the fold key is the minimum of
// each rune's SimpleFold orbit, so every case variant shares one key.
func TestLabelNameKey(t *testing.T) {
	same := [][]string{
		{"Urgent", "urgent", "URGENT", "uRgEnT"},
		{"Straße", "STRAßE", "straße"},
		{"Ǆemal", "ǅemal", "ǆemal"},
		{"K", "k", "K"}, // Kelvin sign folds with k
		{"Σίσυφος", "σίσυφος", "ΣΊΣΥΦΟΣ"},
	}
	for _, group := range same {
		want := LabelNameKey(group[0])
		for _, n := range group[1:] {
			if got := LabelNameKey(n); got != want {
				t.Errorf("LabelNameKey(%q) = %q, want %q (as %q)", n, got, want, group[0])
			}
		}
	}
	for _, pair := range [][2]string{{"urgent", "urgent!"}, {"é", "é"}, {"a", "b"}} {
		if LabelNameKey(pair[0]) == LabelNameKey(pair[1]) {
			t.Errorf("%q and %q share a fold key", pair[0], pair[1])
		}
	}
	// The key is stored by both stores, so its exact form is the contract:
	// each rune becomes the smallest rune of its orbit.
	for name, want := range map[string]string{"Urgent": "URGENT", "straße": "STRAßE", "\u212a": "K", "σς": "ΣΣ"} {
		if got := LabelNameKey(name); got != want {
			t.Errorf("LabelNameKey(%q) = %q, want %q", name, got, want)
		}
	}
	if got := LabelNameKey("Urgent"); got != LabelNameKey(LabelNameKey("Urgent")) {
		t.Errorf("fold key %q is not a fixed point", got)
	}
}

func TestLabelSelectorValidate(t *testing.T) {
	many := labelRefs(tenantA, 70)
	cases := map[string]struct {
		sel   LabelSelector
		field string
	}{
		"any":                  {LabelSelector{Any: []LabelRef{labelA, labelB}}, ""},
		"all":                  {LabelSelector{All: []LabelRef{labelA}}, ""},
		"none":                 {LabelSelector{None: []LabelRef{labelC}}, ""},
		"any and all":          {LabelSelector{Any: []LabelRef{labelA}, All: []LabelRef{labelA, labelB}}, ""},
		"every part":           {LabelSelector{Any: []LabelRef{labelA}, All: []LabelRef{labelB}, None: []LabelRef{labelC}}, ""},
		"part at cap":          {LabelSelector{Any: many[:MaxLabelSelectorPart]}, ""},
		"union at cap":         {LabelSelector{Any: many[:32], None: many[32:64]}, ""},
		"empty":                {LabelSelector{}, "labels"},
		"empty slices":         {LabelSelector{Any: []LabelRef{}, All: []LabelRef{}, None: []LabelRef{}}, "labels"},
		"bad ref":              {LabelSelector{Any: []LabelRef{{Tenant: tenantA}}}, "labels.any[0].id"},
		"no tenant":            {LabelSelector{All: []LabelRef{labelA, {ID: idB}}}, "labels.all[1].tenant"},
		"duplicate in any":     {LabelSelector{Any: []LabelRef{labelA, labelA}}, "labels.any[1]"},
		"duplicate in all":     {LabelSelector{All: []LabelRef{labelB, labelA, labelB}}, "labels.all[2]"},
		"duplicate in none":    {LabelSelector{None: []LabelRef{labelC, labelC}}, "labels.none[1]"},
		"part over cap":        {LabelSelector{All: many[:MaxLabelSelectorPart+1]}, "labels.all"},
		"union over cap":       {LabelSelector{Any: many[:32], All: many[32:64], None: many[64:65]}, "labels"},
		"none contradicts any": {LabelSelector{Any: []LabelRef{labelA}, None: []LabelRef{labelB, labelA}}, "labels.none[1]"},
		"none contradicts all": {LabelSelector{All: []LabelRef{labelC}, None: []LabelRef{labelC}}, "labels.none[0]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := Filter{Labels: &tc.sel}
			if tc.field == "" {
				requireOK(t, f.Validate())
				return
			}
			requireError(t, f.Validate(), Validation, tc.field)
		})
	}
	requireOK(t, Filter{}.Validate())
}

// A selector naming a label of another tenant is not found, never a hint
// that the label exists.
func TestLabelSelectorCheckScope(t *testing.T) {
	foreign := LabelRef{Tenant: tenantB, ID: idA}
	requireOK(t, LabelSelector{Any: []LabelRef{labelA}, All: []LabelRef{labelB}, None: []LabelRef{labelC}}.CheckScope(tenantA))
	for name, sel := range map[string]LabelSelector{
		"any":  {Any: []LabelRef{labelA, foreign}},
		"all":  {All: []LabelRef{foreign}},
		"none": {Any: []LabelRef{labelA}, None: []LabelRef{foreign}},
	} {
		t.Run(name, func(t *testing.T) { requireError(t, sel.CheckScope(tenantA), NotFound, "") })
	}
}

func TestFilterLabelsClone(t *testing.T) {
	f := Filter{Labels: &LabelSelector{Any: []LabelRef{labelA}, All: []LabelRef{labelB}, None: []LabelRef{labelC}}}
	c := f.Clone()
	c.Labels.Any[0] = labelC
	c.Labels.None = append(c.Labels.None[:0], labelA)
	if f.Labels.Any[0] != labelA || f.Labels.None[0] != labelC {
		t.Fatalf("original mutated through clone: %+v", f.Labels)
	}
	if c.Labels == f.Labels {
		t.Fatal("clone shares the selector")
	}
	b, err := json.Marshal(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "labels") {
		t.Fatalf("unset selector encoded: %s", b)
	}
}

func TestConversationLabelsCloneDeep(t *testing.T) {
	orig := Conversation{Ref: convA, Labels: []LabelRef{labelA, labelB}}
	c := orig.Clone()
	c.Labels[0] = labelC
	if orig.Labels[0] != labelA {
		t.Fatalf("original mutated through clone: %v", orig.Labels)
	}
	if got := (Conversation{}).Clone(); got.Labels != nil {
		t.Fatalf("nil labels became %#v", got.Labels)
	}
}

func TestConversationValidateLabels(t *testing.T) {
	over := labelRefs(tenantA, MaxConversationLabels+1)
	cases := map[string]struct {
		labels []LabelRef
		field  string
	}{
		"none":         {nil, ""},
		"sorted":       {[]LabelRef{labelA, labelB, labelC}, ""},
		"at cap":       {over[:MaxConversationLabels], ""},
		"over cap":     {over, "labels"},
		"unsorted":     {[]LabelRef{labelB, labelA}, "labels[1]"},
		"duplicate":    {[]LabelRef{labelA, labelA}, "labels[1]"},
		"bad ref":      {[]LabelRef{{Tenant: tenantA, ID: badID}}, "labels[0].id"},
		"other tenant": {[]LabelRef{labelA, {Tenant: tenantB, ID: idB}}, "labels[1]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := Conversation{Ref: convA, Labels: tc.labels}.ValidateLabels()
			if tc.field == "" {
				requireOK(t, err)
				return
			}
			requireError(t, err, Validation, tc.field)
		})
	}
}

func TestLabelRequests(t *testing.T) {
	newLabel := Label{Ref: LabelRef{Tenant: tenantA}, Name: "Urgent"}
	cap64 := labelRefs(tenantA, MaxConversationLabels+1)
	runRequestCases(t, map[string]requestCase{
		"configure create":          {ConfigureLabel{Command: cmd, Label: newLabel}, ""},
		"configure create revision": {ConfigureLabel{Command: cmdRev, Label: newLabel}, "expected_revision"},
		"configure update":          {ConfigureLabel{Command: cmdRev, Label: Label{Ref: labelA, Name: "Urgent"}}, ""},
		"configure update no rev":   {ConfigureLabel{Command: cmd, Label: Label{Ref: labelA, Name: "Urgent"}}, "expected_revision"},
		"configure no tenant":       {ConfigureLabel{Command: cmd, Label: Label{Name: "Urgent"}}, "label.ref.tenant"},
		"configure bad id":          {ConfigureLabel{Command: cmdRev, Label: Label{Ref: LabelRef{Tenant: tenantA, ID: badID}, Name: "x"}}, "label.ref.id"},
		"configure no name":         {ConfigureLabel{Command: cmd, Label: Label{Ref: LabelRef{Tenant: tenantA}}}, "label.name"},
		"configure bad color":       {ConfigureLabel{Command: cmd, Label: Label{Ref: LabelRef{Tenant: tenantA}, Name: "x", Color: "#FFF"}}, "label.color"},
		"configure no correlation":  {ConfigureLabel{Command: noCall, Label: newLabel}, "correlation"},
		"delete":                    {DeleteLabel{Command: cmdRev, Label: labelA}, ""},
		"delete no revision":        {DeleteLabel{Command: cmd, Label: labelA}, "expected_revision"},
		"delete no id":              {DeleteLabel{Command: cmdRev, Label: LabelRef{Tenant: tenantA}}, "label.id"},
		"add":                       {ConversationLabels{Command: cmd, Conversation: convA, Labels: []LabelRef{labelB, labelA}}, ""},
		"add at revision":           {ConversationLabels{Command: cmdRev, Conversation: convA, Labels: []LabelRef{labelA}}, ""},
		"add at cap":                {ConversationLabels{Command: cmd, Conversation: convA, Labels: cap64[:MaxConversationLabels]}, ""},
		"add no labels":             {ConversationLabels{Command: cmd, Conversation: convA}, "labels"},
		"add empty labels":          {ConversationLabels{Command: cmd, Conversation: convA, Labels: []LabelRef{}}, "labels"},
		"add over cap":              {ConversationLabels{Command: cmd, Conversation: convA, Labels: cap64}, "labels"},
		"add duplicate":             {ConversationLabels{Command: cmd, Conversation: convA, Labels: []LabelRef{labelA, labelB, labelA}}, "labels[2]"},
		"add bad label":             {ConversationLabels{Command: cmd, Conversation: convA, Labels: []LabelRef{{Tenant: tenantA}}}, "labels[0].id"},
		"add other tenant":          {ConversationLabels{Command: cmd, Conversation: convA, Labels: []LabelRef{{Tenant: tenantB, ID: idA}}}, "labels[0]"},
		"add bad conversation":      {ConversationLabels{Command: cmd, Labels: []LabelRef{labelA}}, "conversation.inbox.tenant"},
		"add no operation":          {ConversationLabels{Command: Command{Call: call}, Conversation: convA, Labels: []LabelRef{labelA}}, "operation"},
	})
	for name, tc := range map[string]struct{ got, want RevisionRule }{
		"configure create": {ConfigureLabel{}.RevisionRule(), RevisionAbsent},
		"configure update": {ConfigureLabel{Label: Label{Ref: labelA}}.RevisionRule(), RevisionRequired},
		"delete":           {DeleteLabel{}.RevisionRule(), RevisionRequired},
		"conversation":     {ConversationLabels{}.RevisionRule(), RevisionOptional},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: rule %d, want %d", name, tc.got, tc.want)
		}
	}
	requireOK(t, Lookup[LabelRef]{Call: call, Ref: labelA}.Validate())
	requireError(t, Lookup[LabelRef]{Call: call, Ref: LabelRef{Tenant: tenantA}}.Validate(), Validation, "ref.id")
}

// Label events report one label change on a conversation as references
// only: no name, description or color.
func TestLabelPayloadCarriesRefsOnly(t *testing.T) {
	for _, kind := range []string{EventConversationLabelAdded, EventConversationLabelRemoved} {
		if _, ok := builtinPayloads[kind].(ConversationLabelPayload); !ok {
			t.Errorf("%s payload = %T, want ConversationLabelPayload", kind, builtinPayloads[kind])
		}
	}
	op := OperationRef{Tenant: tenantA, ID: idD}
	b, err := json.Marshal(ConversationLabelPayload{
		Conversation: convA, Label: labelA, Trigger: TriggerCommand, PreviousRevision: "r1", Operation: &op,
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{"conversation", "label", "trigger", "previous_revision", "operation"}
	if len(fields) != len(want) {
		t.Errorf("fields %s, want exactly %v", b, want)
	}
	for _, k := range want {
		if _, ok := fields[k]; !ok {
			t.Errorf("%s missing from %s", k, b)
		}
	}
	if string(fields["label"]) != `{"tenant":"tenant-a","id":"`+string(idA)+`"}` {
		t.Errorf("label = %s, want the ref only", fields["label"])
	}
	b, err = json.Marshal(ConversationLabelPayload{Trigger: TriggerProvider})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "operation") {
		t.Errorf("unset operation encoded: %s", b)
	}
}

// Label names follow the reserved-name grammar: a tenant-scoped resource
// kind, <area>.<verb> actions and <area>.<past-tense verb> events whose
// schema is the uma-prefixed kind.
func TestLabelNamesFollowGrammar(t *testing.T) {
	if KindLabel != "label" || KindLabel.scope() != scopeTenant {
		t.Fatalf("KindLabel = %q scope %d, want tenant-scoped label", KindLabel, KindLabel.scope())
	}
	for got, want := range map[string]string{
		ActionLabelRead:                "label.read",
		ActionLabelConfigure:           "label.configure",
		ActionConversationLabel:        "conversation.label",
		EventConversationLabelAdded:    "conversation.label_added",
		EventConversationLabelRemoved:  "conversation.label_removed",
		SchemaConversationLabelAdded:   "uma.conversation.label_added",
		SchemaConversationLabelRemoved: "uma.conversation.label_removed",
	} {
		if got != want {
			t.Errorf("name %q, want %q", got, want)
		}
		if ValidateExtensionName(got) == nil {
			t.Errorf("core name %q accepted as an extension name", got)
		}
	}
}

func TestCreationTarget(t *testing.T) {
	for _, kind := range []ResourceKind{KindChannel, KindLabel, KindContact} {
		r := CreationTarget(tenantA, kind)
		requireOK(t, r.Validate())
		if r.Tenant != tenantA || r.Kind != kind || r.Inbox != "" {
			t.Errorf("CreationTarget(%s) = %+v", kind, r)
		}
		if other := CreationTarget(tenantB, kind); other.ID != r.ID || other.Tenant != tenantB {
			t.Errorf("CreationTarget(%s) not stable across tenants: %+v vs %+v", kind, other, r)
		}
	}
	if CreationTarget(tenantA, KindChannel).ID == CreationTarget(tenantA, KindLabel).ID {
		t.Error("channel and label creation share a target ID")
	}
	// The channel target is stored in existing operation keys; it never changes.
	if got := CreationTarget(tenantA, KindChannel).ID; got != "00000000-0000-8000-8000-00000000c4a1" {
		t.Errorf("channel creation target = %s", got)
	}
	if CreationTarget(tenantA, KindContact).ID == CreationTarget(tenantA, KindLabel).ID {
		t.Error("contact and label creation share a target ID")
	}
	for _, kind := range []ResourceKind{KindInbox, KindConversation, KindMessage, "com.example.thing"} {
		if r := CreationTarget(tenantA, kind); !r.IsZero() {
			t.Errorf("CreationTarget(%s) = %+v, want zero", kind, r)
		}
	}
}
