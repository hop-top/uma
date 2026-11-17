package services_test

import (
	"context"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Shared fixtures under their local names.

const (
	tenantA       = enginetest.TenantA
	tenantB       = enginetest.TenantB
	actor         = enginetest.Actor
	inboxOne      = enginetest.InboxOne
	inboxTwo      = enginetest.InboxTwo
	convOne       = enginetest.ConvOne
	thirdPartyKey = enginetest.ThirdPartyKey
	outKey        = enginetest.OutKey
	chanOne       = enginetest.ChanOne
	routeNSpace   = enginetest.RouteNSpace
	chatType      = enginetest.ChatType
)

var (
	principalA        = enginetest.PrincipalA
	requireCode       = enginetest.RequireCode
	receiveDescriptor = enginetest.ReceiveDescriptor
	newRig            = enginetest.NewRig
	newHarness        = enginetest.NewHarness
	perContact        = enginetest.PerContact
)

type (
	outFactory  = enginetest.OutFactory
	outInstance = enginetest.OutInstance
	rig         = enginetest.Rig
)

func all[T any](h *enginetest.Harness, list func(store.Reader, context.Context, model.ListRequest) (model.Page[T], error), f model.Filter) []T {
	return enginetest.All(h, list, f)
}
