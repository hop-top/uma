package engine

import "hop.top/uma/adapter"

// Builtin name sets, for tests in package engine_test.
var (
	BuiltinCapabilities = builtinCapabilities
	BuiltinEvents       = builtinEvents
	BuiltinActions      = builtinActions

	BuiltinActionClasses = builtinActionClasses
)

// Action classes, for tests in package engine_test.
const (
	ClassRead   = classRead
	ClassEffect = classEffect
	ClassCore   = classCore
)

// Extensions returns the engine's registered extension definitions.
func (e *Engine) Extensions() []adapter.ExtensionDefinition { return e.reg.extensions }
