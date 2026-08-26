package forkctl

import (
	"testing"
)

func TestCostCell(t *testing.T) {
	if got := (forkStatus{}).costCell(); got != "—" {
		t.Errorf("a fork with no cost = %q, want —", got)
	}
	if got := (forkStatus{Cost: 12.3}).costCell(); got != "$12.30" {
		t.Errorf("costCell(12.3) = %q, want $12.30", got)
	}
}
