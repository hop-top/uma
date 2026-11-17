package runtime_test

import "hop.top/uma/internal/enginetest"

// Shared fixtures under their local names.

const (
	tenantA     = enginetest.TenantA
	outPurpose  = enginetest.OutPurpose
	chanTwo     = enginetest.ChanTwo
	credRef     = enginetest.CredRef
	routeNSpace = enginetest.RouteNSpace
	ingressNS   = enginetest.IngressNS
)

var (
	validConfig              = enginetest.ValidConfig
	mustNew                  = enginetest.MustNew
	requireCode              = enginetest.RequireCode
	lostAnswer               = enginetest.LostAnswer
	transientFailure         = enginetest.TransientFailure
	rateLimited              = enginetest.RateLimited
	newRig                   = enginetest.NewRig
	withReconciler           = enginetest.WithReconciler
	withCredentialResolution = enginetest.WithCredentialResolution
	withRetry                = enginetest.WithRetry
	requireState             = enginetest.RequireState
	newHarness               = enginetest.NewHarness
	eventKinds               = enginetest.EventKinds
)

type (
	rig          = enginetest.Rig
	signalWaiter = enginetest.SignalWaiter
	harness      = enginetest.Harness
)
