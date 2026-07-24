package store

import "fmt"

// errFromPanic converts a recovered panic value into an error for rollback.
func errFromPanic(r any) error {
	if e, ok := r.(error); ok {
		return fmt.Errorf("store: panic during tx: %w", e)
	}
	return fmt.Errorf("store: panic during tx: %v", r)
}
