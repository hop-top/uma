package workitems_test

import (
	"errors"
	"strings"
	"testing"

	"hop.top/uma/internal/workitems"
	"hop.top/uma/model"
)

func ptr[T any](v T) *T { return &v }

func requireCode(t *testing.T, err error, code model.ErrorCode, want string) {
	t.Helper()
	var e *model.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
	if !strings.Contains(e.SafeMessage, want) {
		t.Fatalf("message %q, want it to contain %q", e.SafeMessage, want)
	}
}

func item(status string, local ...string) model.WorkItem {
	return model.WorkItem{Status: status, Binding: model.WorkItemBindingActive, LocalFields: local}
}

// Field ownership routes an update: only local fields commit locally; a
// provider-owned status alone, with or without a reason, goes to the
// provider; a provider-owned status never shares an action with a local
// field; a reason never travels with a local status.
func TestClassify(t *testing.T) {
	provider := item(model.WorkItemOpen, model.WorkItemFieldSubject)
	local := item(model.WorkItemOpen, model.WorkItemFieldStatus, model.WorkItemFieldSubject)
	for name, c := range map[string]struct {
		cur  model.WorkItem
		u    model.WorkItemUpdate
		want workitems.Path
	}{
		"local status":             {local, model.WorkItemUpdate{Status: ptr(model.WorkItemResolved)}, workitems.PathLocal},
		"local subject":            {provider, model.WorkItemUpdate{Subject: ptr("Renamed")}, workitems.PathLocal},
		"local status and subject": {local, model.WorkItemUpdate{Status: ptr(model.WorkItemResolved), Subject: ptr("Renamed")}, workitems.PathLocal},
		"provider status":          {provider, model.WorkItemUpdate{Status: ptr(model.WorkItemClosed)}, workitems.PathProvider},
		"provider status reason":   {provider, model.WorkItemUpdate{Status: ptr(model.WorkItemClosed), StatusReason: ptr("not_planned")}, workitems.PathProvider},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := workitems.Classify(c.cur, c.u)
			if err != nil || got != c.want {
				t.Fatalf("Classify = %v, %v; want %v", got, err, c.want)
			}
		})
	}

	_, err := workitems.Classify(provider, model.WorkItemUpdate{Status: ptr(model.WorkItemClosed), Subject: ptr("Renamed")})
	requireCode(t, err, model.Validation, "send provider-owned and uma-owned fields in separate actions")
	_, err = workitems.Classify(local, model.WorkItemUpdate{Status: ptr(model.WorkItemResolved), StatusReason: ptr("completed")})
	requireCode(t, err, model.Validation, "status_reason")
	_, err = workitems.Classify(provider, model.WorkItemUpdate{Status: ptr(model.WorkItemClosed), Assignees: &[]model.ExternalRef{}})
	requireCode(t, err, model.Unsupported, "the provider owns work item field assignees")
	_, err = workitems.Classify(item(model.WorkItemOpen), model.WorkItemUpdate{Subject: ptr("Renamed")})
	requireCode(t, err, model.Unsupported, "the provider owns work item field subject")
	merged := provider
	merged.Binding = model.WorkItemBindingMerged
	_, err = workitems.Classify(merged, model.WorkItemUpdate{Status: ptr(model.WorkItemClosed)})
	requireCode(t, err, model.Conflict, "binding is merged")
}

// Update is the local path: it applies only what Classify routes locally
// (a provider-routed update conflicts), and a closed local status stays
// terminal.
func TestUpdateLocalPathOnly(t *testing.T) {
	provider := item(model.WorkItemOpen, model.WorkItemFieldSubject)
	_, err := workitems.Update(provider, model.WorkItemUpdate{Status: ptr(model.WorkItemClosed)})
	requireCode(t, err, model.Conflict, "provider-owned")
	_, err = workitems.Update(provider, model.WorkItemUpdate{Status: ptr(model.WorkItemClosed), Subject: ptr("Renamed")})
	requireCode(t, err, model.Validation, "separate actions")
	local := item(model.WorkItemOpen, model.WorkItemFieldStatus)
	_, err = workitems.Update(local, model.WorkItemUpdate{Status: ptr(model.WorkItemResolved), StatusReason: ptr("completed")})
	requireCode(t, err, model.Validation, "status_reason")
	_, err = workitems.Update(item(model.WorkItemClosed, model.WorkItemFieldStatus), model.WorkItemUpdate{Status: ptr(model.WorkItemOpen)})
	requireCode(t, err, model.Conflict, "terminal")
	ch, err := workitems.Update(local, model.WorkItemUpdate{Status: ptr(model.WorkItemResolved)})
	if err != nil || ch.Item.Status != model.WorkItemResolved {
		t.Fatalf("Update = %+v, %v", ch, err)
	}
}
