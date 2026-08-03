package agentgo

// ContextKey is the shared identity used to correlate information present in
// an agent context with information later demanded by the trajectory. Kind and
// Identity are application-defined; AgentGo assigns them no semantics.
type ContextKey struct {
	Kind     string `json:"kind"`
	Identity string `json:"identity"`
}

// ContextItem describes one identifiable piece of information present in an
// AgentMessage. Representation, Reason, and Ref are open application labels;
// AgentGo records them but does not rank or interpret them.
type ContextItem struct {
	ContextKey
	Representation string `json:"representation"`
	Reason         string `json:"reason,omitempty"`
	Ref            string `json:"ref,omitempty"`
}

// ContextDemand is the protocol-level counterpart of ContextItem. It records
// that a trajectory exposed demand for the same ContextKey through an
// application-defined Signal. AgentGo defines the shape only; applications and
// evaluators decide how demands are derived and what they mean.
type ContextDemand struct {
	ContextKey
	Signal string `json:"signal"`
}

// ContextItemProvider is an optional AgentMessage capability. A message may
// expose zero, one, or many identifiable context items without changing its
// model projection.
type ContextItemProvider interface {
	AgentMessage
	ContextItems() []ContextItem
}

// ContextDemandProvider is an optional capability for messages or trajectory
// artifacts that can state demands explicitly. Demand inferred from raw tool
// or model events remains the evaluator's responsibility.
type ContextDemandProvider interface {
	ContextDemands() []ContextDemand
}

// CollectContextItems returns the item inventory exposed by messages in
// message order. It deliberately does not validate, deduplicate, rank, or
// otherwise interpret application identities.
func CollectContextItems(messages []AgentMessage) []ContextItem {
	var items []ContextItem
	for _, message := range messages {
		provider, ok := message.(ContextItemProvider)
		if !ok {
			continue
		}
		items = append(items, provider.ContextItems()...)
	}
	return items
}

// CollectContextDemands returns explicit demands exposed by messages in
// message order. Evaluators may combine these with demands inferred from the
// raw event trajectory.
func CollectContextDemands(messages []AgentMessage) []ContextDemand {
	var demands []ContextDemand
	for _, message := range messages {
		provider, ok := message.(ContextDemandProvider)
		if !ok {
			continue
		}
		demands = append(demands, provider.ContextDemands()...)
	}
	return demands
}
