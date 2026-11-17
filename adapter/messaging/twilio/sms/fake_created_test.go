package sms_test

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"hop.top/uma/conformance"
)

const createdAccount = "AC00000000000000000000000000000001"

// do issues one request to the fake as the account and returns the
// answer's status, content type and decoded JSON body.
func (f *fakeTwilio) do(t *testing.T, method, path string, form url.Values) (int, string, map[string]any) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(t.Context(), method, f.srv.URL+"/2010-04-01/Accounts/"+createdAccount+"/"+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(createdAccount, "token")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%s %s answered %q: %v", method, path, raw, err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), got
}

func (f *fakeTwilio) create(t *testing.T, to, from, text string) map[string]any {
	t.Helper()
	status, _, got := f.do(t, http.MethodPost, "Messages.json", url.Values{"To": {to}, "From": {from}, "Body": {text}})
	if status != http.StatusCreated {
		t.Fatalf("create answered %d, want 201", status)
	}
	return got
}

// shape flattens a JSON value to its field paths and the JSON kind of
// each (null, string, number, object): the shape of a resource.
func shape(prefix string, v any, into map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		into[prefix] = "object"
		for k, c := range x {
			shape(prefix+"."+k, c, into)
		}
	case nil:
		into[prefix] = "null"
	case string:
		into[prefix] = "string"
	case float64:
		into[prefix] = "number"
	default:
		into[prefix] = fmt.Sprintf("%T", v)
	}
}

func flatValues(prefix string, v any, into map[string]any) {
	if m, ok := v.(map[string]any); ok {
		for k, c := range m {
			flatValues(prefix+"."+k, c, into)
		}
		return
	}
	into[prefix] = v
}

// recordedCreate is the cassette's accepted create answer, read by
// itself, not through the fake's loader.
func recordedCreate(t *testing.T) (recordedAnswer, map[string]any) {
	t.Helper()
	a, _, err := readAnswer("send-and-fetch", http.MethodPost)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(a.body), &res); err != nil {
		t.Fatal(err)
	}
	return a, res
}

// TestFakeAcceptedCreateMatchesRecording pins the fake's answer to an
// accepted create against the one recorded in send-and-fetch: the same
// status and content type, the same field set (nested too) with the same
// kinds, and the recorded value for every field a call does not set. A
// field the recording has and the fake lacks, or the reverse, fails.
func TestFakeAcceptedCreateMatchesRecording(t *testing.T) {
	f := newFakeTwilio(t)
	rec, want := recordedCreate(t)
	status, contentType, got := f.do(t, http.MethodPost, "Messages.json",
		url.Values{"To": {"+15558675310"}, "From": {senderNumber}, "Body": {"hello"}})
	if status != rec.status || contentType != rec.contentType {
		t.Fatalf("answered %d %q, recorded %d %q", status, contentType, rec.status, rec.contentType)
	}

	wantShape, gotShape := map[string]string{}, map[string]string{}
	shape("", want, wantShape)
	shape("", got, gotShape)
	if !maps.Equal(wantShape, gotShape) {
		for k, kind := range wantShape {
			if gotShape[k] != kind {
				t.Errorf("field %q: recorded %q, fake %q", k, kind, gotShape[k])
			}
		}
		for k, kind := range gotShape {
			if _, ok := wantShape[k]; !ok {
				t.Errorf("field %q (%s) is not in the recording", k, kind)
			}
		}
	}

	// Every field a call does not set is the recorded value.
	perCall := map[string]bool{
		".sid": true, ".uri": true, ".to": true, ".from": true, ".body": true,
		".date_created": true, ".date_updated": true, ".account_sid": true,
		".subresource_uris.media": true,
	}
	wantFlat, gotFlat := map[string]any{}, map[string]any{}
	flatValues("", want, wantFlat)
	flatValues("", got, gotFlat)
	for k, v := range wantFlat {
		if !perCall[k] && gotFlat[k] != v {
			t.Errorf("field %q = %v, recorded %v", k, gotFlat[k], v)
		}
	}

	// Per-call fields carry the call, in the recorded formats.
	sid, _ := got["sid"].(string)
	if !regexp.MustCompile(`^SM[0-9a-f]{32}$`).MatchString(sid) {
		t.Errorf("sid %q is not an SM SID", sid)
	}
	uri := "/2010-04-01/Accounts/" + createdAccount + "/Messages/" + sid
	if got["uri"] != uri+".json" || got["account_sid"] != createdAccount ||
		got["subresource_uris"].(map[string]any)["media"] != uri+"/Media.json" {
		t.Errorf("uri %v, account %v, subresources %v; want %s", got["uri"], got["account_sid"], got["subresource_uris"], uri)
	}
	if got["to"] != "+15558675310" || got["from"] != senderNumber || got["body"] != "hello" {
		t.Errorf("to/from/body = %v/%v/%v, want the call's", got["to"], got["from"], got["body"])
	}
	for _, k := range []string{"date_created", "date_updated"} {
		if _, err := time.Parse(time.RFC1123Z, got[k].(string)); err != nil {
			t.Errorf("%s %q is not in the recorded format: %v", k, got[k], err)
		}
	}
	for k, v := range got {
		if s, ok := v.(string); ok && (strings.Contains(s, recordedPlaceholder) || strings.Contains(s, f.recorded.createdSID) ||
			strings.Contains(s, "FROM_NUMBER") || strings.Contains(s, "TO_NUMBER")) {
			t.Errorf("field %q leaks the recording: %q", k, s)
		}
	}
}

// TestFakeAcceptedScenarios checks the fake still behaves around the
// recorded create answer: each create is a new message, a fetch and a
// list answer for it, a lost answer's message is still created, and a
// refused one keeps the recorded failed shape.
func TestFakeAcceptedScenarios(t *testing.T) {
	f := newFakeTwilio(t)
	first := f.create(t, "+15558675310", senderNumber, "one")
	second := f.create(t, "+15558675310", senderNumber, "one")
	if first["sid"] == second["sid"] {
		t.Fatalf("two creates share the SID %v", first["sid"])
	}

	status, _, fetched := f.do(t, http.MethodGet, "Messages/"+first["sid"].(string)+".json", nil)
	if status != http.StatusOK || fetched["sid"] != first["sid"] || fetched["to"] != "+15558675310" ||
		fetched["from"] != senderNumber || fetched["body"] != "one" || fetched["status"] != "queued" {
		t.Fatalf("fetch = %d %v, want the created message, queued", status, fetched)
	}

	status, _, page := f.do(t, http.MethodGet, "Messages.json?To=%2B15558675310", nil)
	items, _ := page["messages"].([]any)
	if status != http.StatusOK || len(items) != 2 {
		t.Fatalf("list = %d with %d messages, want 2", status, len(items))
	}

	// An answer lost after the message was created: the call is applied.
	f.respond(createdAccount, conformance.Response{Outcome: conformance.OutcomeAnswerLost})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, f.srv.URL+"/2010-04-01/Accounts/"+createdAccount+"/Messages.json",
		strings.NewReader(url.Values{"To": {"+15558675310"}, "From": {senderNumber}, "Body": {"lost"}}.Encode()))
	req.SetBasicAuth(createdAccount, "token")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, err := f.srv.Client().Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a lost answer was answered")
	}
	if c := f.calls(createdAccount); !c[len(c)-1].Applied {
		t.Fatalf("lost answer's call = %+v, want applied", c[len(c)-1])
	}

	// A refused create (answer lost) keeps a failed message in the shape
	// of the recorded failed one, not of a created one.
	f.respond(createdAccount, conformance.Response{Outcome: outcomeRefusedAnswerLost})
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodPost, f.srv.URL+"/2010-04-01/Accounts/"+createdAccount+"/Messages.json",
		strings.NewReader(url.Values{"To": {senderNumber}, "From": {senderNumber}, "Body": {"refused"}}.Encode()))
	req.SetBasicAuth(createdAccount, "token")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, err := f.srv.Client().Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a lost refusal was answered")
	}
	calls := f.calls(createdAccount)
	status, _, failed := f.do(t, http.MethodGet, "Messages/"+calls[len(calls)-1].SID+".json", nil)
	var recorded map[string]any
	if err := json.Unmarshal([]byte(f.recorded.message), &recorded); err != nil {
		t.Fatal(err)
	}
	wantShape, gotShape := map[string]string{}, map[string]string{}
	shape("", recorded, wantShape)
	shape("", failed, gotShape)
	delete(wantShape, ".error_code") // null or a code by message
	delete(gotShape, ".error_code")
	delete(wantShape, ".price") // null or a price by message
	delete(gotShape, ".price")
	delete(wantShape, ".date_sent")
	delete(gotShape, ".date_sent")
	if status != http.StatusOK || failed["status"] != "failed" || failed["error_code"] != float64(21266) || !maps.Equal(wantShape, gotShape) {
		t.Fatalf("refused message = %d %v; want failed, error 21266, the recorded failed shape", status, failed)
	}
	if _, ok := failed["subresource_uris"].(map[string]any)["feedback"]; !ok {
		t.Fatalf("failed message lacks the feedback subresource of the recorded one: %v", failed["subresource_uris"])
	}
}
