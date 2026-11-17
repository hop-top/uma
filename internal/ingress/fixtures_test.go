package ingress_test

import (
	"context"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Shared fixtures under their local names.

const (
	tenantA    = enginetest.TenantA
	tenantB    = enginetest.TenantB
	chatType   = enginetest.ChatType
	ingressNS  = enginetest.IngressNS
	serviceAct = enginetest.ServiceAct
	dest       = enginetest.Dest
)

var (
	t0           = enginetest.T0
	requireCode  = enginetest.RequireCode
	withSelector = enginetest.WithSelector
	newHarness   = enginetest.NewHarness
	perContact   = enginetest.PerContact
	eventKinds   = enginetest.EventKinds
)

type (
	ingressAdapter = enginetest.IngressAdapter
	queueReceiver  = enginetest.QueueReceiver
	harness        = enginetest.Harness
)

func all[T any](h *harness, list func(store.Reader, context.Context, model.ListRequest) (model.Page[T], error), f model.Filter) []T {
	return enginetest.All(h, list, f)
}
