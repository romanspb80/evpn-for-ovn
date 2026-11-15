package main

import "testing"

func TestInvalidActionCannotOpenState(t *testing.T) {
	if err := run("/nonexistent", "delete-nlri"); err == nil {
		t.Fatal("non-UUID withdrawal accepted")
	}
}
