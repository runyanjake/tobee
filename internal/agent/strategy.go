package agent

// Strategy fills Turn.Reply or Turn.Await; the runtime owns scope, budget, delivery, and parking (D-037).
type Strategy interface {
	Name() string
	Handle(t *Turn)
}
