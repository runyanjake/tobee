package agent

// Strategy is a reasoning scheme: it takes one turn from a fresh
// conversation to either Turn.Reply or Turn.Await. The runtime owns
// everything around it — the queue, scope, the turn budget, delivery,
// and parking (D-037).
//
// PlanExecute is the one strategy today. AGENT_STRATEGY selects it.
type Strategy interface {
	Name() string
	Handle(t *Turn)
}
