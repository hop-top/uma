package model

import (
	"fmt"
	"reflect"
	"testing"
)

func TestReplyRecipientsNormalize(t *testing.T) {
	in := ReplyRecipients{
		ReplyAll: true,
		Cc:       []string{"a@Example.COM", "b@example.com", "a@example.com"},
		Bcc:      []string{"B@example.com", "c@example.com", "c@EXAMPLE.com"},
	}
	got, err := in.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	want := ReplyRecipients{ReplyAll: true, Cc: []string{"a@example.com", "b@example.com"}, Bcc: []string{"c@example.com"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Normalize = %+v, want %+v", got, want)
	}
	// An address in both lists keeps its Cc place only.
	got, err = ReplyRecipients{Cc: []string{"a@example.com"}, Bcc: []string{"a@example.com", "z@example.com"}}.Normalize()
	if err != nil || !reflect.DeepEqual(got.Bcc, []string{"z@example.com"}) {
		t.Errorf("cc/bcc overlap: %+v, %v", got, err)
	}
}

func TestReplyRecipientsValidate(t *testing.T) {
	many := make([]string, MaxReplyRecipients+1)
	for i := range many {
		many[i] = fmt.Sprintf("u%d@example.com", i)
	}
	for name, tc := range map[string]struct {
		r  ReplyRecipients
		ok bool
	}{
		"zero":              {ReplyRecipients{}, true},
		"reply all only":    {ReplyRecipients{ReplyAll: true}, true},
		"cc and bcc":        {ReplyRecipients{Cc: []string{"a@example.com"}, Bcc: []string{"b@example.com"}}, true},
		"display name":      {ReplyRecipients{Cc: []string{"A <a@example.com>"}}, false},
		"not an address":    {ReplyRecipients{Cc: []string{"nope"}}, false},
		"bad bcc":           {ReplyRecipients{Bcc: []string{" a@example.com"}}, false},
		"header injection":  {ReplyRecipients{Cc: []string{"a@example.com\r\nBcc: x@example.com"}}, false},
		"exactly the bound": {ReplyRecipients{Cc: many[:MaxReplyRecipients]}, true},
		"over the bound":    {ReplyRecipients{Cc: many[:MaxReplyRecipients], Bcc: many[MaxReplyRecipients:]}, false},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.r.Validate()
			if (err == nil) != tc.ok {
				t.Fatalf("Validate = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && !IsCode(err, Validation) {
				t.Errorf("code = %v, want validation", err)
			}
		})
	}
}

func TestRecipientLimitsRoundTrip(t *testing.T) {
	l := RecipientLimits{ReplyAll: true, Cc: true, Bcc: true, MaxRecipients: 50}
	got, err := ParseRecipientLimits(l.Extension())
	if err != nil || got != l {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	for name, e := range map[string]Extension{
		"wrong name":    {Name: StateLimitsName, Version: 1, Data: []byte(`{}`)},
		"wrong version": {Name: RecipientLimitsName, Version: 2, Data: []byte(`{}`)},
		"unreadable":    {Name: RecipientLimitsName, Version: 1, Data: []byte(`[`)},
		"negative max":  {Name: RecipientLimitsName, Version: 1, Data: []byte(`{"max_recipients":-1}`)},
	} {
		if _, err := ParseRecipientLimits(e); !IsCode(err, Validation) {
			t.Errorf("%s: err = %v, want validation", name, err)
		}
	}
}
