package lease

import (
	"os"
	"testing"
)

// TestMain shortens the lock wait for the whole package. A lock that is leaked
// (the failure a mutated lock() produces) then costs a second per later call
// instead of twenty, so the suite goes red at once rather than appearing to hang.
func TestMain(m *testing.M) {
	lockTries = 1000
	os.Exit(m.Run())
}
