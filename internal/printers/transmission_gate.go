package printers

import "context"

// transmissionGate serializes print attempts across every worker owned by a
// Manager. Connections remain independent; only queue claim through outcome
// persistence is protected by this permit.
type transmissionGate struct {
	permit chan struct{}
}

func newTransmissionGate() *transmissionGate {
	g := &transmissionGate{permit: make(chan struct{}, 1)}
	g.permit <- struct{}{}
	return g
}

func (g *transmissionGate) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.permit:
		return nil
	}
}

func (g *transmissionGate) release() { g.permit <- struct{}{} }
