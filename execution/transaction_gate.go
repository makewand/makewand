package execution

import (
	"context"
	"sync"
)

// Queue local transactions before opening files or polling the OS lock. The
// byte lock remains authoritative across processes and differently spelled
// paths; this gate only prevents local contenders from starving settlement.
type transactionGate struct {
	busy chan struct{}
	refs int
}

var transactionGates = struct {
	sync.Mutex
	paths map[string]*transactionGate
}{paths: make(map[string]*transactionGate)}

func enterTransaction(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	transactionGates.Lock()
	gate := transactionGates.paths[path]
	if gate == nil {
		gate = &transactionGate{busy: make(chan struct{}, 1)}
		transactionGates.paths[path] = gate
	}
	gate.refs++
	transactionGates.Unlock()
	drop := func() {
		transactionGates.Lock()
		gate.refs--
		if gate.refs == 0 {
			delete(transactionGates.paths, path)
		}
		transactionGates.Unlock()
	}
	select {
	case gate.busy <- struct{}{}:
		return func() { <-gate.busy; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}
