package sms_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// recordedAnswers are Twilio's own answers, read from the cassettes
// TestRecordedAnswers replays. Where a recording covers an answer, the
// fake Messages API answers in its shape rather than a documented one.
type recordedAnswers struct {
	// rejected is a create Twilio refuses (HTTP 400), unauthorized a
	// request with a wrong auth token (401), notFound a fetch of an
	// unknown message (404), whose message names notFoundPath.
	rejected, unauthorized, notFound recordedAnswer
	notFoundPath                     string
	// listing is a Messages list page, and message one message resource
	// from a listing, both as JSON.
	listing, message string
	// created is the answer to a create Twilio accepts (HTTP 201), and
	// createdSID the SID of the message it creates: the template of the
	// fake's accepted create answers.
	created    recordedAnswer
	createdSID string
}

type recordedAnswer struct {
	status      int
	contentType string
	body        string
}

// recordedPlaceholder is the account SID placeholder in recordings.
const recordedPlaceholder = "ACCOUNT_SID"

var loadRecordedAnswers = sync.OnceValues(func() (*recordedAnswers, error) {
	var (
		r   recordedAnswers
		err error
	)
	if r.rejected, _, err = readAnswer("rejected", http.MethodPost); err != nil {
		return nil, err
	}
	if r.unauthorized, _, err = readAnswer("refused-credentials", http.MethodGet); err != nil {
		return nil, err
	}
	var u string
	if r.notFound, u, err = readAnswer("fetch-unknown", http.MethodGet); err != nil {
		return nil, err
	}
	r.notFoundPath = u[strings.Index(u, "/2010-04-01/"):]
	empty, _, err := readAnswer("reconcile-none", http.MethodGet)
	if err != nil {
		return nil, err
	}
	r.listing = empty.body
	full, _, err := readAnswer("rejected", http.MethodGet)
	if err != nil {
		return nil, err
	}
	var page struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(full.body), &page); err != nil || len(page.Messages) == 0 {
		return nil, fmt.Errorf("recorded listing holds no message: %v", err)
	}
	r.message = string(page.Messages[0])
	if r.created, _, err = readAnswer("send-and-fetch", http.MethodPost); err != nil {
		return nil, err
	}
	var made struct {
		SID string `json:"sid"`
	}
	if err := json.Unmarshal([]byte(r.created.body), &made); err != nil || made.SID == "" {
		return nil, fmt.Errorf("recorded create answer holds no message SID: %v", err)
	}
	r.createdSID = made.SID
	return &r, nil
})

// readAnswer returns the recorded answer to the one method request of a
// scenario directory, and the request URL.
func readAnswer(scenario, method string) (recordedAnswer, string, error) {
	reqs, err := filepath.Glob(filepath.Join(cassettes, scenario, "*.req.yaml"))
	if err != nil {
		return recordedAnswer{}, "", err
	}
	var file struct {
		Payload struct {
			Method  string            `yaml:"method"`
			URL     string            `yaml:"url"`
			Status  int               `yaml:"status"`
			Headers map[string]string `yaml:"headers"`
			Body    string            `yaml:"body"`
		} `yaml:"payload"`
	}
	for _, name := range reqs {
		if err := readYAML(name, &file); err != nil {
			return recordedAnswer{}, "", err
		}
		if file.Payload.Method != method {
			continue
		}
		u := file.Payload.URL
		if err := readYAML(strings.TrimSuffix(name, ".req.yaml")+".resp.yaml", &file); err != nil {
			return recordedAnswer{}, "", err
		}
		p := file.Payload
		return recordedAnswer{status: p.Status, contentType: p.Headers["Content-Type"], body: p.Body}, u, nil
	}
	return recordedAnswer{}, "", errors.New("no recorded " + method + " request in " + scenario)
}

func readYAML(name string, v any) error {
	b, err := os.ReadFile(name) // #nosec G304 -- test data
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, v)
}

// write answers a recorded answer, with old/new replacement pairs applied
// to its body.
func (a recordedAnswer) write(w http.ResponseWriter, oldnew ...string) {
	w.Header().Set("Content-Type", a.contentType)
	w.WriteHeader(a.status)
	_, _ = w.Write([]byte(strings.NewReplacer(oldnew...).Replace(a.body))) // #nosec G705 -- recorded provider answer
}
