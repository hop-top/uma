package recipients_test

import (
	"fmt"
	"reflect"
	"testing"

	"hop.top/uma/adapter/mail/recipients"
	"hop.top/uma/model"
)

func TestPlanWithoutRecipientsIsTheRoute(t *testing.T) {
	got, err := recipients.Plan(nil, "jane@example.org", []string{"me@example.com"}, []string{"x@example.org"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (recipients.Set{To: []string{"jane@example.org"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("Plan = %+v, want %+v", got, want)
	}
}

func TestPlanReplyAll(t *testing.T) {
	req := &model.ReplyRecipients{ReplyAll: true, Cc: []string{"extra@example.org"}, Bcc: []string{"audit@example.com"}}
	got, err := recipients.Plan(req, "jane@example.org",
		[]string{"Me@Example.com"},
		[]string{"me@example.com", "bob@example.org", "JANE@example.org"},
		[]string{"carol@example.org", "bob@example.org", "me@EXAMPLE.com"})
	if err != nil {
		t.Fatal(err)
	}
	want := recipients.Set{
		To:  []string{"jane@example.org", "bob@example.org"},
		Cc:  []string{"carol@example.org", "extra@example.org"},
		Bcc: []string{"audit@example.com"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Plan = %+v, want %+v", got, want)
	}
	if all := got.All(); !reflect.DeepEqual(all, []string{"jane@example.org", "bob@example.org", "carol@example.org", "extra@example.org", "audit@example.com"}) {
		t.Errorf("All = %v", all)
	}
}

// Explicit recipients are the caller's: the channel's own address stays when
// named, and an address already in To or Cc is not repeated in Bcc.
func TestPlanExplicitRecipients(t *testing.T) {
	req := &model.ReplyRecipients{Cc: []string{"me@example.com", "jane@example.org"}, Bcc: []string{"me@example.com", "z@example.org"}}
	got, err := recipients.Plan(req, "jane@example.org", []string{"me@example.com"}, []string{"ignored@example.org"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := recipients.Set{To: []string{"jane@example.org"}, Cc: []string{"me@example.com"}, Bcc: []string{"z@example.org"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Plan = %+v, want %+v", got, want)
	}
}

func TestPlanRefusesTooMany(t *testing.T) {
	var parentTo []string
	for i := 0; i < recipients.Max; i++ {
		parentTo = append(parentTo, fmt.Sprintf("u%d@example.org", i))
	}
	_, err := recipients.Plan(&model.ReplyRecipients{ReplyAll: true}, "jane@example.org", nil, parentTo, nil)
	if !model.IsCode(err, model.Validation) {
		t.Fatalf("err = %v, want validation", err)
	}
	if _, err := recipients.Plan(&model.ReplyRecipients{ReplyAll: true}, "jane@example.org", nil, parentTo[:recipients.Max-1], nil); err != nil {
		t.Fatalf("exactly the bound: %v", err)
	}
}

func TestPlanRefusesAnUnsupportedRequest(t *testing.T) {
	_, err := recipients.Plan(&model.ReplyRecipients{Cc: []string{"nobody"}}, "jane@example.org", nil, nil, nil)
	if !model.IsCode(err, model.Validation) {
		t.Fatalf("err = %v, want validation", err)
	}
}

func TestParseList(t *testing.T) {
	got := recipients.ParseList(`"Doe, Jane" <jane@example.org>, bob@Example.org, <carol@example.org>`)
	if want := []string{"jane@example.org", "bob@Example.org", "carol@example.org"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ParseList = %v, want %v", got, want)
	}
	for _, bad := range []string{"", "not an address", "a@example.org, nobody"} {
		if got := recipients.ParseList(bad); got != nil {
			t.Errorf("ParseList(%q) = %v, want none", bad, got)
		}
	}
}

func TestLimitsAreReadable(t *testing.T) {
	l, err := model.ParseRecipientLimits(recipients.Limits().Extension())
	if err != nil || !l.ReplyAll || !l.Cc || !l.Bcc || l.MaxRecipients != recipients.Max {
		t.Fatalf("limits = %+v, %v", l, err)
	}
}
