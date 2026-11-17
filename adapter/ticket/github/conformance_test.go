package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// The adapter passes the shared suite for receive_http, send, reconcile,
// work_item_act, the message mutations, history and the update effect
// against the documented GitHub REST shapes.
func TestAdapterConformance(t *testing.T) {
	conformance.RunAdapterSuite(t, conformance.AdapterHarness{
		New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
			gh := newFakeGitHub(t, func() time.Time { return time.Unix(1_700_000_000, 0).UTC() })
			p := &provider{gh: gh}
			return &recordingFactory{Factory: github.NewFactory(github.Options{}), p: p}, p
		},
		Capabilities: github.NewFactory(github.Options{}).Describe().Capabilities,
		Selection:    model.Selection{Mode: model.ModePerWorkItem},
		Extensions:   github.NewFactory(github.Options{}).Extensions(),
		Action: model.Extension{
			Name: github.ExtensionUpdate, Version: github.ExtensionVersion,
			Data: json.RawMessage(`{"labels":["bug","triage"]}`),
		},
		// An update that sets no field: the adapter's own rule.
		InvalidAction: model.Extension{
			Name: github.ExtensionUpdate, Version: github.ExtensionVersion,
			Data: json.RawMessage(`{}`),
		},
	})
}

// provider is the conformance view of the fake GitHub: one account (API
// root) per channel, and one issue per peer.
type provider struct {
	gh *fakeGitHub

	mu       sync.Mutex
	prefixes map[model.ChannelRef]string
	peers    map[string]int
	hook     *webhookState
	// executes are the update effects the engine handed the adapter, per
	// channel, in order; see recordingInstance.
	executes map[model.ChannelRef][]executed
}

// executed is what GitHub never sees of an update effect: the operation
// and audience it ran under.
type executed struct {
	op       model.OperationRef
	audience model.Audience
}

// recordingFactory opens instances that record each update effect's
// operation and audience for the provider. An issue update carries no
// marker, so the PATCH shows the provider the fields only; the suite's
// checks on the operation and audience the call carried read them from
// what the engine handed Execute, paired with the PATCH calls in order.
// Every Execute reaches GitHub: fields the adapter refuses are refused at
// the call (adapter.ActionValidator), before any effect is stored.
type recordingFactory struct {
	*github.Factory
	p *provider
}

func (f *recordingFactory) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	inst, err := f.Factory.Open(ctx, ch, deps)
	if err != nil {
		return nil, err
	}
	return &recordingInstance{Instance: inst.(*github.Instance), p: f.p, ch: ch.Ref}, nil
}

type recordingInstance struct {
	*github.Instance
	p  *provider
	ch model.ChannelRef
}

func (i *recordingInstance) Execute(ctx context.Context, r adapter.ExecuteRequest) (adapter.Outcome, error) {
	i.p.mu.Lock()
	if i.p.executes == nil {
		i.p.executes = map[model.ChannelRef][]executed{}
	}
	i.p.executes[i.ch] = append(i.p.executes[i.ch], executed{op: r.Operation, audience: r.Audience})
	i.p.mu.Unlock()
	return i.Instance.Execute(ctx, r)
}

// executedAt returns the k-th update effect executed on ch.
func (p *provider) executedAt(ch model.ChannelRef, k int) (executed, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if k < len(p.executes[ch]) {
		return p.executes[ch][k], true
	}
	return executed{}, false
}

func (p *provider) prefix(ch model.ChannelRef) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.prefixes == nil {
		p.prefixes = map[model.ChannelRef]string{}
	}
	pre, ok := p.prefixes[ch]
	if !ok {
		pre = "acct" + strconv.Itoa(len(p.prefixes)+1)
		p.prefixes[ch] = pre
		p.gh.account(pre, botID)
	}
	return pre
}

// HTTPClient is the client the suite's engine injects.
func (p *provider) HTTPClient() ports.HTTPClient { return p.gh.client() }

// Account configures replies and channel-wide history over the sandbox
// repository, the one every route names.
func (p *provider) Account(ch model.ChannelRef) conformance.Account {
	cfg := replyConfig(p.gh.apiURL(p.prefix(ch)))
	cfg.HistoryRepositories = []string{sandboxRepo}
	return conformance.Account{Configuration: cfg.Extension(), CredentialRef: webhookSecretID}
}

// Route is the work item reference of the peer's issue. GitHub holds the
// issue from the start: the account's copy is seeded, without a
// description, a week before the suite's clock, so an update of its
// fields finds it and only its comments fall in a history window.
func (p *provider) Route(ch model.ChannelRef, peer string) model.ExternalRef {
	p.mu.Lock()
	if p.peers == nil {
		p.peers = map[string]int{}
	}
	n, ok := p.peers[peer]
	if !ok {
		n = len(p.peers) + 1
		p.peers[peer] = n
	}
	p.mu.Unlock()
	if pre := p.prefix(ch); !p.gh.hasIssue(pre, sandboxRepo, n) {
		p.gh.putIssue(pre, sandboxRepo, n, 5000001, "", time.Unix(1_700_000_000, 0).UTC().Add(-7*24*time.Hour), false)
	}
	return model.ExternalRef{Channel: ch, Namespace: github.Namespace, Kind: model.RefKindWorkItem, ID: strconv.Itoa(n), ParentID: sandboxRepo}
}

func (p *provider) Respond(ch model.ChannelRef, rs ...conformance.Response) error {
	var as []answer
	for _, r := range rs {
		switch r.Outcome {
		case conformance.OutcomeAccepted:
			as = append(as, accepted)
		case conformance.OutcomeRejected:
			as = append(as, statusAnswer(http.StatusUnprocessableEntity, `{"message":"Validation Failed","documentation_url":"`+docsURL+`","status":"422"}`, nil))
		case conformance.OutcomeUnauthorized:
			as = append(as, statusAnswer(http.StatusForbidden, `{"message":"Resource not accessible by integration","documentation_url":"`+commentsDocsURL+`","status":"403"}`, nil))
		case conformance.OutcomeTransient:
			as = append(as, unreachable)
		case conformance.OutcomeRateLimited:
			as = append(as, statusAnswer(http.StatusTooManyRequests,
				`{"message":"You have exceeded a secondary rate limit.","documentation_url":"https://docs.github.com/rest/using-the-rest-api/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`,
				map[string]string{"Retry-After": strconv.Itoa(int(r.RetryAfter / time.Second))}))
		case conformance.OutcomeAnswerLost:
			as = append(as, answerLost)
		case conformance.OutcomeRequestLost:
			as = append(as, requestLost)
		default:
			return conformance.ErrNotExpressible
		}
	}
	p.gh.respond(p.prefix(ch), as...)
	return nil
}

// Calls maps requests to capabilities: creating a comment is send, updating
// an issue's state is work_item_act and its fields extension_execute,
// reading one is reconcile. The operation of a send is read from the
// comment's marker; an issue update carries none (see recordingFactory).
func (p *provider) Calls(ch model.ChannelRef) []conformance.ProviderCall {
	var out []conformance.ProviderCall
	calls := p.gh.calls(p.prefix(ch))
	updates := 0
	for i, c := range calls {
		route := routeOf(ch, c.Path)
		if mc, ok := mutationCall(ch, c, calls[i+1:]); ok {
			if mc.Capability != "" {
				out = append(out, mc)
			}
			continue
		}
		switch c.Method {
		case http.MethodPatch:
			var req struct {
				State       *string `json:"state"`
				StateReason string  `json:"state_reason"`
			}
			_ = json.Unmarshal([]byte(c.Body), &req)
			if req.State == nil {
				pc := conformance.ProviderCall{
					Capability: model.CapabilityExtensionExecute, Route: route, Applied: c.Applied,
					Action: &model.Extension{Name: github.ExtensionUpdate, Version: github.ExtensionVersion, Data: json.RawMessage(c.Body)},
				}
				if x, ok := p.executedAt(ch, updates); ok {
					pc.Operation, pc.Audience = x.op, x.audience
				}
				updates++
				out = append(out, pc)
				continue
			}
			out = append(out, conformance.ProviderCall{
				Capability: model.CapabilityWorkItemAct, Route: route, Applied: c.Applied,
				Status: *req.State, Reason: req.StateReason,
			})
		case http.MethodPost:
			var req struct {
				Body string `json:"body"`
			}
			_ = json.Unmarshal([]byte(c.Body), &req)
			text, op := splitMarker(req.Body)
			pc := conformance.ProviderCall{
				Capability: model.CapabilitySend, Operation: model.OperationRef{Tenant: ch.Tenant, ID: op},
				Route: route, Applied: c.Applied, Audience: model.Audience{Visibility: model.Public},
				Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
			}
			if c.Applied {
				pc.References = []model.ExternalRef{commentRef(ch, c.Comment)}
			}
			out = append(out, pc)
		case http.MethodGet:
			out = append(out, conformance.ProviderCall{Capability: model.CapabilityReconcile, Route: route})
		}
	}
	return out
}

// SeedWorkItem opens or closes the issue for peer. GitHub holds no other
// status.
func (p *provider) SeedWorkItem(ch model.ChannelRef, peer, status string) (model.ExternalRef, error) {
	route := p.Route(ch, peer)
	n, _ := strconv.Atoi(route.ID)
	updated := time.Unix(1_700_000_000, 0).UTC().Add(-2 * time.Hour)
	switch status {
	case model.WorkItemOpen:
		p.gh.setIssue(p.prefix(ch), sandboxRepo, n, 5000001, "open", nil, updated)
	case model.WorkItemClosed:
		p.gh.setIssue(p.prefix(ch), sandboxRepo, n, 5000001, "closed", "completed", updated)
	default:
		return model.ExternalRef{}, conformance.ErrNotExpressible
	}
	return route, nil
}

// WorkItemStatus reads the issue's state.
func (p *provider) WorkItemStatus(ch model.ChannelRef, work model.ExternalRef) (string, error) {
	state, ok := p.gh.issueState(p.prefix(ch), work.ParentID+"#"+work.ID)
	if !ok {
		return "", fmt.Errorf("no issue %s#%s", work.ParentID, work.ID)
	}
	return state, nil
}

// Revoke: an installation's permissions are only visible in GitHub's
// answers, so a revocation cannot be known before a call.
func (p *provider) Revoke(model.ChannelRef, model.Capability) error {
	return conformance.ErrNotExpressible
}

func routeOf(ch model.ChannelRef, path string) model.ExternalRef {
	s := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(s) < 5 {
		return model.ExternalRef{}
	}
	return model.ExternalRef{Channel: ch, Namespace: github.Namespace, Kind: model.RefKindWorkItem, ID: s[4], ParentID: s[1] + "/" + s[2]}
}

// splitMarker returns a body's text and the operation its marker names.
func splitMarker(body string) (string, model.ID) {
	i := strings.LastIndex(body, "\n\n<!-- uma:")
	if i < 0 {
		return body, ""
	}
	op, _, _ := strings.Cut(strings.TrimPrefix(body[i:], "\n\n<!-- uma:"), ":")
	return body[:i], model.ID(op)
}
