package model

import (
	"reflect"
	"testing"
	"time"
)

var timeType = reflect.TypeFor[time.Time]()

// fill sets every exported field reachable from v to a non-zero value, so a
// Clone that forgets a slice, map or pointer field is caught by checkDisjoint.
func fill(v reflect.Value, depth int) {
	if depth > 8 {
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == timeType {
			v.Set(reflect.ValueOf(time.Unix(1700000000, 0).UTC()))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i), depth+1)
			}
		}
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 2, 2)
		fill(s.Index(0), depth+1)
		fill(s.Index(1), depth+1)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		e := reflect.New(v.Type().Elem()).Elem()
		fill(k, depth+1)
		fill(e, depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fill(p.Elem(), depth+1)
		v.Set(p)
	}
}

// checkDisjoint fails when a and b share any slice backing array, map or
// pointer target.
func checkDisjoint(t *testing.T, path string, a, b reflect.Value) {
	t.Helper()
	switch a.Kind() {
	case reflect.Struct:
		if a.Type() == timeType {
			return
		}
		for i := 0; i < a.NumField(); i++ {
			if a.Type().Field(i).IsExported() {
				checkDisjoint(t, path+"."+a.Type().Field(i).Name, a.Field(i), b.Field(i))
			}
		}
	case reflect.Slice:
		if a.Len() > 0 && a.Pointer() == b.Pointer() {
			t.Errorf("%s: slice aliases the original", path)
		}
		for i := 0; i < min(a.Len(), b.Len()); i++ {
			checkDisjoint(t, path+"[]", a.Index(i), b.Index(i))
		}
	case reflect.Map:
		if !a.IsNil() && a.Pointer() == b.Pointer() {
			t.Errorf("%s: map aliases the original", path)
		}
		for _, k := range a.MapKeys() {
			checkDisjoint(t, path+"{}", a.MapIndex(k), b.MapIndex(k))
		}
	case reflect.Pointer:
		if !a.IsNil() && a.Pointer() == b.Pointer() {
			t.Errorf("%s: pointer aliases the original", path)
		}
		if !a.IsNil() && !b.IsNil() {
			checkDisjoint(t, path+"*", a.Elem(), b.Elem())
		}
	}
}

func checkClone[T any](t *testing.T, clone func(T) T) {
	t.Helper()
	var zero T
	if got := clone(zero); !reflect.DeepEqual(got, zero) {
		t.Fatalf("Clone(zero) = %+v; nil must stay nil", got)
	}
	var v T
	fill(reflect.ValueOf(&v).Elem(), 0)
	c := clone(v)
	if !reflect.DeepEqual(v, c) {
		t.Fatalf("Clone differs from original:\n got %+v\nwant %+v", c, v)
	}
	checkDisjoint(t, reflect.TypeFor[T]().Name(), reflect.ValueOf(v), reflect.ValueOf(c))
}

func TestCloneHasNoAliases(t *testing.T) {
	cases := map[string]func(*testing.T){
		"Extension":                 func(t *testing.T) { checkClone(t, Extension.Clone) },
		"Part":                      func(t *testing.T) { checkClone(t, Part.Clone) },
		"Content":                   func(t *testing.T) { checkClone(t, Content.Clone) },
		"Filter":                    func(t *testing.T) { checkClone(t, Filter.Clone) },
		"Export":                    func(t *testing.T) { checkClone(t, Export.Clone) },
		"ExportLine":                func(t *testing.T) { checkClone(t, ExportLine.Clone) },
		"ListRequest":               func(t *testing.T) { checkClone(t, ListRequest.Clone) },
		"Selection":                 func(t *testing.T) { checkClone(t, Selection.Clone) },
		"Identity":                  func(t *testing.T) { checkClone(t, Identity.Clone) },
		"Message":                   func(t *testing.T) { checkClone(t, Message.Clone) },
		"Conversation":              func(t *testing.T) { checkClone(t, Conversation.Clone) },
		"Inbox":                     func(t *testing.T) { checkClone(t, Inbox.Clone) },
		"Channel":                   func(t *testing.T) { checkClone(t, Channel.Clone) },
		"WorkItem":                  func(t *testing.T) { checkClone(t, WorkItem.Clone) },
		"WorkItemState":             func(t *testing.T) { checkClone(t, WorkItemState.Clone) },
		"WorkItemBindingChange":     func(t *testing.T) { checkClone(t, WorkItemBindingChange.Clone) },
		"Task":                      func(t *testing.T) { checkClone(t, Task.Clone) },
		"Artifact":                  func(t *testing.T) { checkClone(t, Artifact.Clone) },
		"IngressReceipt":            func(t *testing.T) { checkClone(t, IngressReceipt.Clone) },
		"Operation":                 func(t *testing.T) { checkClone(t, Operation.Clone) },
		"Acceptance":                func(t *testing.T) { checkClone(t, Acceptance.Clone) },
		"SubscriptionResult":        func(t *testing.T) { checkClone(t, SubscriptionResult.Clone) },
		"CapabilityStatus":          func(t *testing.T) { checkClone(t, CapabilityStatus.Clone) },
		"Event":                     func(t *testing.T) { checkClone(t, Event.Clone) },
		"EventBatch":                func(t *testing.T) { checkClone(t, EventBatch.Clone) },
		"Stream":                    func(t *testing.T) { checkClone(t, Stream.Clone) },
		"Evidence":                  func(t *testing.T) { checkClone(t, Evidence.Clone) },
		"IdentityLink":              func(t *testing.T) { checkClone(t, IdentityLink.Clone) },
		"LinkRequest":               func(t *testing.T) { checkClone(t, LinkRequest.Clone) },
		"Envelope":                  func(t *testing.T) { checkClone(t, Envelope.Clone) },
		"ProviderConversationState": func(t *testing.T) { checkClone(t, ProviderConversationState.Clone) },
		"ProviderStateReport":       func(t *testing.T) { checkClone(t, ProviderStateReport.Clone) },
		"DeliveryReport":            func(t *testing.T) { checkClone(t, DeliveryReport.Clone) },
		"Interaction":               func(t *testing.T) { checkClone(t, Interaction.Clone) },
		"InteractionInput":          func(t *testing.T) { checkClone(t, InteractionInput.Clone) },
		"InteractionDecision":       func(t *testing.T) { checkClone(t, InteractionDecision.Clone) },
		"Page[Message]": func(t *testing.T) {
			checkClone(t, func(p Page[Message]) Page[Message] { return p.Clone(Message.Clone) })
		},
		"Page[ChannelRef] shallow": func(t *testing.T) {
			checkClone(t, func(p Page[ChannelRef]) Page[ChannelRef] { return p.Clone(nil) })
		},
	}
	for name, run := range cases {
		t.Run(name, run)
	}
}

func TestCloneKeepsEmptyNonNil(t *testing.T) {
	c := Content{Parts: []Part{}}.Clone()
	if c.Parts == nil || len(c.Parts) != 0 {
		t.Fatalf("empty slice became %#v", c.Parts)
	}
	d := InteractionDecision{FieldErrors: map[string]string{}}.Clone()
	if d.FieldErrors == nil {
		t.Fatal("empty map became nil")
	}
}

func TestMutatingCloneLeavesOriginal(t *testing.T) {
	orig := Message{
		Content:  Content{Parts: []Part{{Kind: "structured", Structured: &Extension{Name: "a.b", Version: 1, Data: []byte(`{"k":1}`)}}}},
		Metadata: []Extension{{Name: "smtp.headers", Version: 1, Data: []byte(`{}`)}},
	}
	c := orig.Clone()
	c.Content.Parts[0].Structured.Data[2] = 'X'
	c.Content.Parts[0].Kind = "text"
	c.Metadata[0].Data[0] = '['
	if string(orig.Content.Parts[0].Structured.Data) != `{"k":1}` || orig.Content.Parts[0].Kind != "structured" ||
		string(orig.Metadata[0].Data) != `{}` {
		t.Fatalf("original mutated through clone: %+v", orig)
	}
}
