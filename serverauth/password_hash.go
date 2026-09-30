package serverauth

import (
	"sync"
	"sync/atomic"
)

type passwordHashGate struct{ slots chan struct{} }

var sharedPasswordHashes atomic.Pointer[passwordHashGate]

func init() { ConfigurePasswordHashConcurrency(DefaultRegistrationConcurrency) }

// ConfigurePasswordHashConcurrency sets the process-wide cap shared by login
// and registration. Call once during startup, before accepting requests.
func ConfigurePasswordHashConcurrency(n int) {
	if n < 1 {
		n = DefaultRegistrationConcurrency
	}
	sharedPasswordHashes.Store(&passwordHashGate{slots: make(chan struct{}, n)})
}

// AcquirePasswordHash never waits for resources, so overload does not build an
// unbounded queue of requests holding passwords and HTTP connections.
func AcquirePasswordHash() (func(), bool) {
	gate := sharedPasswordHashes.Load()
	select {
	case gate.slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-gate.slots }) }, true
	default:
		return nil, false
	}
}
