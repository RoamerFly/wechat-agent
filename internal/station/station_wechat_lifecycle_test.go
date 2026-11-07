package station

import (
	"testing"
	"time"
)

func waitAppLifecycle(t *testing.T, ready func() bool) {
	t.Helper()
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ready() {
			return
		}
	}
	t.Fatal("expected connection lifecycle state not reached")
}
