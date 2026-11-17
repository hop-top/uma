package google

import (
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func ext(data string) model.Extension {
	return model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: []byte(data)}
}

func TestValidate(t *testing.T) {
	f := NewFactory(Options{})
	ok := []string{
		``, `{}`, `{"user":"me"}`, `{"user":"desk@example.test","addresses":["support"]}`,
		`{"topic":"projects/my-proj/topics/gmail","push_audience":"https://h.test/x","push_service_account":"p@my-proj.iam.gserviceaccount.com"}`,
		`{"watch_renew_interval_ms":3600000,"message_id_domain":"mail.example.test"}`,
	}
	for _, d := range ok {
		if issues := f.Validate(ext(d)); len(issues) != 0 {
			t.Errorf("Validate(%s) = %+v, want none", d, issues)
		}
	}
	bad := map[string]string{
		`{"user":"not an address"}`:           "user",
		`{"address":"Desk <d@example.test>"}`: "address",
		`{"addresses":["a b"]}`:               "addresses",
		`{"subject":"a\r\nBcc: x"}`:           "subject",
		`{"message_id_domain":"-bad"}`:        "message_id_domain",
		`{"topic":"mytopic","push_audience":"a","push_service_account":"p@x.test"}`:   "topic",
		`{"topic":"projects/my-proj/topics/gmail","push_service_account":"p@x.test"}`: "push_audience",
		`{"topic":"projects/my-proj/topics/gmail","push_audience":"a"}`:               "push_service_account",
		`{"push_audience":"a"}`:                 "topic",
		`{"watch_renew_interval_ms":604800000}`: "watch_renew_interval_ms",
		`{"poll_interval_ms":-1}`:               "poll_interval_ms",
		`{"unknown":1}`:                         "configuration.data",
	}
	for d, field := range bad {
		issues := f.Validate(ext(d))
		found := false
		for _, is := range issues {
			found = found || is.Field == field
		}
		if !found {
			t.Errorf("Validate(%s) = %+v, want an issue on %s", d, issues, field)
		}
	}
	if issues := f.Validate(model.Extension{Name: "other", Version: 1}); len(issues) != 1 || issues[0].Code != string(model.SchemaMismatch) {
		t.Errorf("schema mismatch: %+v", issues)
	}
}

func TestOpenNeedsHTTP(t *testing.T) {
	_, err := NewFactory(Options{}).Open(t.Context(), model.Channel{Ref: testChan, Type: Type, Configuration: ext(`{}`)}, adapter.Dependencies{})
	if model.CodeOf(err) != model.Unavailable {
		t.Fatalf("Open without an HTTP client = %v", err)
	}
}
