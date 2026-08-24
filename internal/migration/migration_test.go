package migration

import "testing"

func TestNextFollowsStrictOrder(t *testing.T) {
	want := []Step{
		StepScaleMons,
		StepExpandOSDs,
		StepFailureDomain,
		StepRaiseReplicas,
		StepRebalance,
		StepFinalize,
		StepDone,
	}
	got := StepPreflight
	for i, exp := range want {
		got = Next(got)
		if got != exp {
			t.Fatalf("step %d: got %q want %q", i, got, exp)
		}
	}
	// Done is terminal.
	if Next(StepDone) != StepDone {
		t.Fatalf("Done should be terminal")
	}
}

func TestHealthyGate(t *testing.T) {
	cases := map[string]bool{
		"HEALTH_OK":   true,
		"HEALTH_WARN": true,
		"HEALTH_ERR":  false,
		"":            false,
	}
	for h, want := range cases {
		if got := healthy(CephStatus{Health: h}); got != want {
			t.Errorf("healthy(%q)=%v want %v", h, got, want)
		}
	}
}

func TestWaitRebalanceGatesOnMisplaced(t *testing.T) {
	e := &Engine{}
	// Not clean yet.
	res, _ := e.waitRebalance(CephStatus{Health: "HEALTH_WARN", PGsActiveClean: false, MisplacedRatio: 0.2})
	if res.Advance {
		t.Fatal("should not advance while misplaced")
	}
	// Converged.
	res, _ = e.waitRebalance(CephStatus{Health: "HEALTH_OK", PGsActiveClean: true})
	if !res.Advance {
		t.Fatal("should advance when clean")
	}
}
