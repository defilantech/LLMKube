package weak

import "testing"

// Weak: references Add by value but never CALLS it, so neutering Add's body to a
// panic is never triggered and this test still passes. That is exactly the
// coverage gap the neuter-survival check must flag: the new logic is not
// exercised by any test.
func TestAddTypeExists(t *testing.T) {
	var _ func(int, int) int = Add
}
