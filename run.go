package agentgo

import "context"

// BeforeRunContext describes the state before one Agent run starts. The
// returned state becomes the run baseline before prompts or turns are applied.
type BeforeRunContext struct {
	State AgentState
}

// AfterRunContext describes the final state of one Agent run.
type AfterRunContext struct {
	State   AgentState
	Summary RunSummary
	Err     error
}

// BeforeRunHook runs once per accepted run. Its returned state replaces the
// run baseline; returning an error ends the run before its first turn.
type BeforeRunHook func(context.Context, BeforeRunContext) (AgentState, error)

// AfterRunHook runs once for every accepted run, including failed, cancelled
// and zero-turn runs. Returning an error changes the terminal result to error.
type AfterRunHook func(context.Context, AfterRunContext) error
