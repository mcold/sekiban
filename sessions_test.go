package main

import (
	"testing"
)

// A connection id that is not a number is not a connection id, and it must
// be refused before it can reach a statement. The check comes first, so this
// needs no server.
func TestKillSessionRejectsABadID(t *testing.T) {
	for _, id := range []int64{0, -1, -9999} {
		err := killSession("", id)
		if err == nil {
			t.Errorf("killSession(%d) should have failed", id)
		}
	}
}
