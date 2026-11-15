package main

import (
	"os"
	"strconv"
	"testing"
)

func TestProcessStartTicks(t *testing.T) {
	ticks, err := processStartTicks(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if value, err := strconv.ParseUint(ticks, 10, 64); err != nil || value == 0 {
		t.Fatalf("invalid process start ticks %q: %v", ticks, err)
	}
	if _, err := processStartTicks(-1); err == nil {
		t.Fatal("missing process must fail")
	}
}
