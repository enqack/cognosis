package daemon

import (
	"fmt"
	"log/slog"
	"runtime/debug"
)

// RecoverPanic recovers a panic in the calling goroutine, logs it with a
// stack trace, and -- if onPanic is non-nil -- hands it the resulting error so
// the caller can fail that one component instead of the panic taking down
// the whole daemon process.
//
// It MUST be called directly by defer (never wrapped in a closure): Go's
// recover() only stops a panic when called directly by the deferred
// function itself, not by a function that deferred function calls. So:
//
//	go func() {
//	    defer daemon.RecoverPanic(log, "watch.reconcile worker", func(err error) {
//	        // handle err
//	    })
//	    ...
//	}()
//
// not `defer func() { daemon.RecoverPanic(...) }()`, which would not recover.
func RecoverPanic(log *slog.Logger, component string, onPanic func(err error)) {
	if r := recover(); r != nil {
		err := fmt.Errorf("panic in %s: %v", component, r)
		log.Error("recovered panic", "component", component, "panic", r, "stack", string(debug.Stack()))
		if onPanic != nil {
			onPanic(err)
		}
	}
}
