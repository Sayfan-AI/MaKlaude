package execute

import (
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// TestEnumTokensAreStable pins the rendered form of every enum in this package.
// The tokens land in escalations, in approval-trail comments, and in whatever a
// caller serializes a [Report] into, so they are part of the contract rather than
// cosmetics — and the unknown-value cases matter most, because a value with no
// String case must render as something a reader can search for rather than as a bare
// integer with no context.
func TestEnumTokensAreStable(t *testing.T) {
	convergence := map[Convergence]string{
		ConvergenceUnobserved:   "unobserved",
		ConvergenceConverged:    "converged",
		ConvergenceTimedOut:     "timed-out",
		ConvergenceUnobservable: "unobservable",
		Convergence(99):         "convergence(99)",
	}
	for value, token := range convergence {
		if got := value.String(); got != token {
			t.Errorf("Convergence(%d).String() = %q, want %q", int(value), got, token)
		}
	}

	failures := map[FailureClass]string{
		FailureNone:            "none",
		FailureNotAuthorized:   "not-authorized",
		FailureClusterMismatch: "cluster-mismatch",
		FailureRefused:         "refused",
		FailureKillSwitch:      "kill-switch",
		FailureUnobservable:    "unobservable",
		FailureDrifted:         "drifted",
		FailureConflict:        "precondition-conflict",
		FailureExecute:         "execute-failed",
		FailureRecord:          "record-failed",
		FailureNotRollbackable: "not-rollbackable",
		FailureClass(99):       "failure(99)",
	}
	for value, token := range failures {
		if got := value.String(); got != token {
			t.Errorf("FailureClass(%d).String() = %q, want %q", int(value), got, token)
		}
	}

	rollbacks := map[RollbackKind]string{
		RollbackUnclassified: "unclassified",
		RollbackNotRequired:  "not-required",
		RollbackImpossible:   "impossible",
		RollbackPerformable:  "performable",
		RollbackKind(99):     "rollback(99)",
	}
	for value, token := range rollbacks {
		if got := value.String(); got != token {
			t.Errorf("RollbackKind(%d).String() = %q, want %q", int(value), got, token)
		}
	}
}

// TestOnlyDriftIsACleanAbort enumerates every failure class and asserts exactly
// which ones mean "nothing is wrong, re-propose".
//
// It is written as an exhaustive table rather than as two positive assertions on
// purpose. The consequence of a class wrongly reporting itself clean is that a real
// problem — a denied write, a lost recording, a refused irreversible action — stops
// reaching a human, and that is a silence nobody notices. Adding a class without
// deciding which side it falls on fails here.
func TestOnlyDriftIsACleanAbort(t *testing.T) {
	clean := map[FailureClass]bool{
		FailureNone:            false,
		FailureNotAuthorized:   false,
		FailureClusterMismatch: false,
		FailureRefused:         false,
		FailureKillSwitch:      false,
		FailureUnobservable:    false,
		FailureDrifted:         true,
		FailureConflict:        true,
		FailureExecute:         false,
		FailureRecord:          false,
		FailureNotRollbackable: false,
	}
	for value, want := range clean {
		if got := value.CleanAbort(); got != want {
			t.Errorf("FailureClass(%s).CleanAbort() = %t, want %t", value, got, want)
		}
	}

	// Every declared class must appear above. The loop bound is the last constant, so
	// a new one added after it fails this test rather than silently defaulting.
	for value := FailureNone; value <= FailureNotRollbackable; value++ {
		if _, listed := clean[value]; !listed {
			t.Errorf("FailureClass(%d) = %q is not classified as clean-abort or not", int(value), value)
		}
	}
}

// TestReportStringLeadsWithWhetherAnythingChanged pins the one-line rendering. It
// leads with the executed/previewed/not-executed state because that is the first
// thing a reader needs and the only part that is irreversible if it is wrong.
func TestReportStringLeadsWithWhetherAnythingChanged(t *testing.T) {
	base := Report{
		Cluster:   testCluster,
		Operation: remediate.OpCordonNode,
		Target:    remediate.Target{Kind: "node", Name: "node-a"},
		Attempts:  1,
	}

	executed := base
	executed.Executed = true
	executed.Convergence = ConvergenceConverged
	if got := executed.String(); !strings.Contains(got, "executed") || !strings.Contains(got, "converged") {
		t.Errorf("executed report renders as %q", got)
	}

	previewed := base
	previewed.DryRun = true
	if got := previewed.String(); !strings.Contains(got, "previewed") {
		t.Errorf("previewed report renders as %q", got)
	}

	aborted := base
	aborted.Attempts = 0
	aborted.Failure = FailureDrifted
	got := aborted.String()
	if !strings.Contains(got, "not executed") || !strings.Contains(got, "drifted") {
		t.Errorf("aborted report renders as %q", got)
	}

	rb := RollbackReport{
		Cluster:   testCluster,
		Operation: remediate.OpCordonNode,
		Target:    remediate.Target{Kind: "node", Name: "node-a"},
		Performed: true,
		Attempts:  1,
	}
	if got := rb.String(); !strings.Contains(got, "performed") {
		t.Errorf("performed rollback renders as %q", got)
	}
	rb.Performed = false
	rb.AlreadyAtPreState = true
	if got := rb.String(); !strings.Contains(got, "already at pre-state") {
		t.Errorf("no-op rollback renders as %q", got)
	}
}

// TestPolicyDefaultsReplaceEveryUnsetField proves a zero policy behaves like a
// configured one, in both the unset and the nonsensical direction. A negative
// observation window read literally would make the bound meaningless, and a zero
// attempt count read literally would execute nothing while looking like it tried.
func TestPolicyDefaultsReplaceEveryUnsetField(t *testing.T) {
	for name, p := range map[string]Policy{
		"zero":     {},
		"negative": {ObserveWindow: -1, ObserveInterval: -1, MaxAttempts: -1, RetryBackoff: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if got := p.normalized(); got != DefaultPolicy() {
				t.Fatalf("normalized to %+v, want the shipped defaults %+v", got, DefaultPolicy())
			}
		})
	}

	custom := Policy{ObserveWindow: 1, ObserveInterval: 2, MaxAttempts: 3, RetryBackoff: 4}
	if got := custom.normalized(); got != custom {
		t.Fatalf("normalized a fully-configured policy to %+v, want it untouched", got)
	}
}
