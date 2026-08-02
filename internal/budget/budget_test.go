package budget

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDefaultLimits_MatchTheApprovedNumbers(t *testing.T) {
	// These are the defaults signed off on the Milestone 5 plan (#138) and restated on
	// #144. A change here is a change to what a human approved, so it fails the build.
	l := DefaultLimits()
	switch {
	case l.PerClusterPerPass != 2:
		t.Errorf("PerClusterPerPass = %d, want 2", l.PerClusterPerPass)
	case l.Cooldown != 30*time.Minute:
		t.Errorf("Cooldown = %s, want 30m", l.Cooldown)
	case l.FailureThreshold != 2:
		t.Errorf("FailureThreshold = %d, want 2", l.FailureThreshold)
	}
	if err := l.Validate(); err != nil {
		t.Errorf("the shipped defaults must validate: %v", err)
	}
}

func TestLimits_ValidateNamesTheFieldToFix(t *testing.T) {
	cases := map[string]struct {
		limits Limits
		field  string
	}{
		"zero cap":          {Limits{Cooldown: time.Minute, FailureThreshold: 1}, "PerClusterPerPass"},
		"negative cap":      {Limits{PerClusterPerPass: -1, Cooldown: time.Minute, FailureThreshold: 1}, "PerClusterPerPass"},
		"zero cooldown":     {Limits{PerClusterPerPass: 1, FailureThreshold: 1}, "Cooldown"},
		"negative cooldown": {Limits{PerClusterPerPass: 1, Cooldown: -time.Minute, FailureThreshold: 1}, "Cooldown"},
		"zero threshold":    {Limits{PerClusterPerPass: 1, Cooldown: time.Minute}, "FailureThreshold"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.limits.Validate()
			if err == nil {
				t.Fatalf("%+v must not validate", tc.limits)
			}
			var le *LimitsError
			if !errors.As(err, &le) {
				t.Fatalf("error = %v, want a *LimitsError so a loader can name the knob", err)
			}
			if le.Field != tc.field {
				t.Errorf("field = %q, want %q", le.Field, tc.field)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("the message must name the field, got %q", err.Error())
			}
		})
	}
}

func TestReason_TokensAreStable(t *testing.T) {
	// These tokens reach the audit trail and the operator-facing summary, so they are
	// contract rather than debug output.
	want := map[Reason]string{
		ReasonStateUnreadable:   "state-unreadable",
		ReasonLimitsInvalid:     "limits-invalid",
		ReasonClusterMismatch:   "cluster-mismatch",
		ReasonBreakerTripped:    "breaker-tripped",
		ReasonPassCapReached:    "pass-cap-reached",
		ReasonTargetCoolingDown: "target-cooling-down",
		ReasonNoPass:            "no-pass",
		ReasonAdmitted:          "admitted",
	}
	for r, token := range want {
		if got := r.String(); got != token {
			t.Errorf("Reason(%d).String() = %q, want %q", int(r), got, token)
		}
	}
	if got := Reason(99).String(); got != "reason(99)" {
		t.Errorf("an unknown reason must render its value, got %q", got)
	}
}

func TestReason_OnlyAdmittedPermits(t *testing.T) {
	for r := ReasonStateUnreadable; r <= ReasonAdmitted; r++ {
		want := r == ReasonAdmitted
		if got := r.Admits(); got != want {
			t.Errorf("%s.Admits() = %v, want %v", r, got, want)
		}
	}
	if Reason(99).Admits() {
		t.Error("a reason this build does not recognize must not permit an unattended action")
	}
}

func TestGrant_ZeroValueDenies(t *testing.T) {
	// A grant built by a helper that forgot a field, or one that survived a refactor
	// with the reason dropped, must read as denied rather than as permission.
	var g Grant
	if g.Admitted() {
		t.Fatal("the zero Grant must not admit")
	}
	if g.Reason != ReasonStateUnreadable {
		t.Errorf("the zero reason = %s, want %s", g.Reason, ReasonStateUnreadable)
	}
}

func TestGrant_StringCarriesTheDetail(t *testing.T) {
	g := Grant{Reason: ReasonPassCapReached, Cluster: "prod", Target: "deployment/default/api", Detail: "2 of 2"}
	got := g.String()
	for _, want := range []string{"pass-cap-reached", "prod", "deployment/default/api", "2 of 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q must contain %q", got, want)
		}
	}
}

func TestOutcome_TokensAndFailureReading(t *testing.T) {
	want := map[Outcome]string{
		OutcomeUnrecorded: "unrecorded",
		OutcomeSucceeded:  "succeeded",
		OutcomeFailed:     "failed",
	}
	for o, token := range want {
		if got := o.String(); got != token {
			t.Errorf("Outcome(%d).String() = %q, want %q", int(o), got, token)
		}
	}
	if got := Outcome(42).String(); got != "outcome(42)" {
		t.Errorf("an unknown outcome must render its value, got %q", got)
	}

	// Written as an allowlist of the one safe value: anything else counts against the
	// breaker rather than being ignored.
	if OutcomeSucceeded.failed() {
		t.Error("a success must not count against the breaker")
	}
	for _, o := range []Outcome{OutcomeUnrecorded, OutcomeFailed, Outcome(42)} {
		if !o.failed() {
			t.Errorf("%s must count against the breaker", o)
		}
	}
}

func TestConsequence_ZeroValueAsksForNothing(t *testing.T) {
	var c Consequence
	if c.Acted() {
		t.Fatal("the zero Consequence — what follows a success — must ask for nothing")
	}
	for name, c := range map[string]Consequence{
		"rollback":  {RollBack: true},
		"demote":    {Demote: true},
		"escalate":  {Escalate: true},
		"tripped":   {Tripped: true},
		"all of it": {RollBack: true, Demote: true, Escalate: true, Tripped: true},
	} {
		if !c.Acted() {
			t.Errorf("%s must report that something is asked of the caller", name)
		}
	}
}

func TestStatus_TrippedFiltersWithoutAliasing(t *testing.T) {
	s := Status{Breakers: []Breaker{
		{Cluster: "dev"},
		{Cluster: "prod", Tripped: true},
	}}
	tripped := s.Tripped()
	if len(tripped) != 1 || tripped[0].Cluster != "prod" {
		t.Fatalf("tripped = %+v, want just prod", tripped)
	}

	// A caller holding the filtered slice must not be able to reach back into the
	// status it came from.
	tripped[0].Cluster = "mutated"
	if s.Breakers[1].Cluster != "prod" {
		t.Error("Tripped must copy rather than alias the status's own slice")
	}
}
