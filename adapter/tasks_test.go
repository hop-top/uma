package adapter_test

import (
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func TestTaskQueryValidate(t *testing.T) {
	ch := model.ChannelRef{Tenant: "tenant-a", ID: "00000000-0000-7000-8000-000000000001"}
	ctxRef := model.ExternalRef{Channel: ch, Namespace: "a2a.context", Kind: model.RefKindContext, ID: "ctx-1"}
	room := ctxRef
	room.Kind = model.RefKindRoom
	for name, q := range map[string]adapter.TaskQuery{
		"all":         {},
		"one context": {Context: &ctxRef},
		"states":      {States: []string{model.TaskWorking, model.TaskCompleted}, Page: model.PageRequest{Limit: 50}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := q.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for name, c := range map[string]struct {
		q     adapter.TaskQuery
		field string
	}{
		"context not a context": {adapter.TaskQuery{Context: &room}, "context.kind"},
		"bad context":           {adapter.TaskQuery{Context: &model.ExternalRef{Channel: ch, Kind: model.RefKindContext, ID: "c"}}, "context.namespace"},
		"undeclared state":      {adapter.TaskQuery{States: []string{"done"}}, "states[0]"},
		"unspecified state":     {adapter.TaskQuery{States: []string{model.TaskWorking, model.TaskUnspecified}}, "states[1]"},
		"negative limit":        {adapter.TaskQuery{Page: model.PageRequest{Limit: -1}}, "page.limit"},
	} {
		t.Run(name, func(t *testing.T) {
			err := c.q.Validate()
			var e *model.Error
			if !model.IsCode(err, model.Validation) || !asError(err, &e) || !strings.HasPrefix(e.SafeMessage, c.field+": ") {
				t.Fatalf("error = %v, want validation on %s", err, c.field)
			}
		})
	}
}

func asError(err error, out **model.Error) bool {
	e, ok := err.(*model.Error)
	*out = e
	return ok
}
