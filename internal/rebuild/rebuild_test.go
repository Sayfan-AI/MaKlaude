package rebuild

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/budget"
	"github.com/Sayfan-AI/MaKlaude/internal/disclose"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
	"github.com/Sayfan-AI/MaKlaude/internal/trust"
)

// The property this package exists for is that a ledger rebuilt from nothing but the
// comms artifacts agrees, entry for entry, with the ledger built by appending as the
// executions happened.
//
// The version of that test that shipped with the marker proved it over bodies a test
// constructed. What it could not prove is the part the T3 carry-over said was missing:
// that the artifacts can be ENUMERATED after they close, and that the GATED trail carries
// the marker at all. So these drive the real renderers on both trails, close the
// artifacts the way the system closes them, and read everything back through the
// production list calls.

var base = time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)

// execution is one finished action: which shape, on whose authority, and how it ended.
// The mix across the table spans every outcome the classifier distinguishes.
type execution struct {
	identity    string
	cluster     string
	operation   remediate.Operation
	authority   audit.Authority
	convergence string
	failure     string
	cleanAbort  bool
	rollback    bool
	at          time.Time
}

// gated reports whether this execution belongs on the approval trail. Authority is the
// whole rule: a human-approved action's artifact is an approval request, and a
// policy-authorized one's is a disclosure.
func (e execution) gated() bool { return e.authority == audit.AuthorityHuman }

// executions covers both trails and every outcome. The three converged human-approved
// entries on prod/rollout-restart are the promotion evidence — the case the T3 carry-over
// singles out as the one a disclosure-only marker would make unrebuildable.
var executions = []execution{
	{"e1", "prod", remediate.OpRolloutRestart, audit.AuthorityHuman, "converged", "", false, false, base},
	{"e2", "prod", remediate.OpRolloutRestart, audit.AuthorityHuman, "converged", "", false, false, base.Add(time.Minute)},
	{"e3", "prod", remediate.OpRolloutRestart, audit.AuthorityHuman, "converged", "", false, false, base.Add(2 * time.Minute)},
	{"e4", "prod", remediate.OpRolloutRestart, audit.AuthorityPolicy, "timed-out", "", false, false, base.Add(3 * time.Minute)},
	{"e5", "prod", remediate.OpDeletePod, audit.AuthorityHuman, "", "drifted", true, false, base.Add(4 * time.Minute)},
	{"e6", "staging", remediate.OpCordonNode, audit.AuthorityHuman, "converged", "", false, true, base.Add(5 * time.Minute)},
	{"e7", "staging", remediate.OpCordonNode, audit.AuthorityPolicy, "", "execute-failed", false, false, base.Add(6 * time.Minute)},
}

// shapes are the three (cluster, operation) pairs the table exercises.
var shapes = []autonomy.Shape{
	{Cluster: "prod", Operation: remediate.OpRolloutRestart},
	{Cluster: "prod", Operation: remediate.OpDeletePod},
	{Cluster: "staging", Operation: remediate.OpCordonNode},
}

// records builds the audit lifecycle one execution produced, attributed to the artifact
// it lives on.
func (e execution) records(ref string) []audit.Record {
	action := audit.Action{
		Identity:  remediate.ProposalIdentity(e.identity),
		Cluster:   e.cluster,
		Operation: e.operation,
	}
	approver := audit.Approver{Authority: e.authority, Ref: ref}
	change := audit.Change{Sent: true, Applied: true, FinishedAt: e.at}

	recs := []audit.Record{
		{RecordedAt: e.at, Phase: audit.PhaseExecuted, Action: action, Approver: approver, Change: change},
	}
	if e.convergence != "" {
		recs = append(recs, audit.Record{
			RecordedAt: e.at, Phase: audit.PhaseVerified, Action: action, Approver: approver, Change: change,
			Outcome: audit.Outcome{Convergence: e.convergence, Failure: "none"},
		})
	}
	if e.failure != "" {
		recs = append(recs, audit.Record{
			RecordedAt: e.at, Phase: audit.PhaseFailed, Action: action, Approver: approver, Change: change,
			Outcome: audit.Outcome{Failure: e.failure, CleanAbort: e.cleanAbort},
		})
	}
	if e.rollback {
		recs = append(recs, audit.Record{
			RecordedAt: e.at, Phase: audit.PhaseRolledBack, Action: action, Approver: approver, Change: change,
			Rollback: audit.Rollback{Attempted: true, Performed: true},
		})
	}
	for i := range recs {
		recs[i].Seq = i + 1
	}
	return recs
}

// proposal is the remediation proposal behind one execution, populated enough for both
// trails' renderers to produce a real body.
func (e execution) proposal() remediate.Proposal {
	return remediate.Proposal{
		Identity:       remediate.ProposalIdentity(e.identity),
		Cluster:        e.cluster,
		Operation:      e.operation,
		Title:          "restore " + e.identity,
		Intent:         "the workload is unhealthy and this is the catalogued fix",
		ExpectedEffect: "the workload returns to a healthy state",
		Reversibility:  remediate.ReversibilityReversible,
		Target: remediate.Target{
			Kind: "Deployment", Namespace: "default", Name: e.identity, ResourceVersion: "1000",
		},
		ProposedAt: e.at,
	}
}

// writeGated puts one human-approved execution on the approval trail the way the gate
// does: a rendered body carrying the gate's own markers, then the lifecycle marker
// attached through the sink's single-purpose write, then closed — because a finished
// action's approval artifact is withdrawn and closed, which is exactly the state
// [approve.ApprovalSink.ListOpen] cannot see.
func writeGated(t *testing.T, sink *approve.MemorySink, e execution) {
	t.Helper()
	ctx := context.Background()

	body := approve.Body(approve.Request{Proposal: e.proposal()}, e.at, approve.DefaultPolicy())
	ref, err := sink.Create(ctx, "approval "+e.identity, body, []string{approve.ManagedLabel})
	if err != nil {
		t.Fatalf("creating the approval artifact for %s: %v", e.identity, err)
	}
	marker, err := audit.LifecycleMarker(e.records(string(ref)))
	if err != nil {
		t.Fatalf("marking %s: %v", e.identity, err)
	}
	if err := sink.SetLifecycleMarker(ctx, ref, marker); err != nil {
		t.Fatalf("attaching the marker for %s: %v", e.identity, err)
	}
	if err := sink.Close(ctx, ref); err != nil {
		t.Fatalf("closing the approval artifact for %s: %v", e.identity, err)
	}
}

// writeDisclosed puts one policy-authorized execution on the disclosure trail through the
// real [disclose.Trail], so the body under test is the one production renders.
func writeDisclosed(t *testing.T, sink *disclose.MemorySink, e execution) {
	t.Helper()
	ctx := context.Background()

	trail, err := disclose.NewTrail(sink, nil)
	if err != nil {
		t.Fatalf("building the disclosure trail: %v", err)
	}
	action := disclose.Action{
		Proposal: e.proposal(),
		Verdict: autonomy.Verdict{
			Decision: autonomy.DecisionAutoApply,
			Rule:     "restart-unhealthy",
			Evidence: "3 human-approved converged executions of this shape",
		},
		Grant: budget.Grant{Reason: budget.ReasonAdmitted, Cluster: e.cluster, Target: e.identity},
		Mode:  "enforce",
		At:    e.at,
	}
	ref, err := trail.Open(ctx, action)
	if err != nil {
		t.Fatalf("opening the disclosure for %s: %v", e.identity, err)
	}
	if err := trail.Complete(ctx, ref, action, disclose.Outcome{
		Records: e.records(string(ref)),
		At:      e.at,
	}); err != nil {
		t.Fatalf("completing the disclosure for %s: %v", e.identity, err)
	}
}

// liveLedger appends every execution as it finished, which is the ledger a rebuild has to
// reproduce. The artifact reference each lifecycle names is the one the trail assigned, so
// the two sides agree on the entry's Ref as well as on its arithmetic.
func liveLedger(t *testing.T, asink *approve.MemorySink, dsink *disclose.MemorySink) *trust.Ledger {
	t.Helper()
	ctx := context.Background()

	refByIdentity := map[string]string{}
	all, err := asink.ListAll(ctx)
	if err != nil {
		t.Fatalf("ListAll (approval): %v", err)
	}
	for _, a := range all {
		id, ok := approve.ParseProposalMarker(a.Body)
		if !ok {
			t.Fatalf("an approval artifact this test wrote carries no proposal marker: %s", a.Ref)
		}
		refByIdentity[string(id)] = string(a.Ref)
	}
	dall, err := dsink.ListAll(ctx)
	if err != nil {
		t.Fatalf("ListAll (disclosure): %v", err)
	}
	for _, a := range dall {
		id, ok := disclose.ParseProposalMarker(a.Body)
		if !ok {
			t.Fatalf("a disclosure artifact this test wrote carries no proposal marker: %s", a.Ref)
		}
		refByIdentity[string(id)] = string(a.Ref)
	}

	live := trust.NewMemory()
	for _, e := range executions {
		ref, ok := refByIdentity[e.identity]
		if !ok {
			t.Fatalf("no artifact was written for %s", e.identity)
		}
		if err := live.RecordLifecycle(e.records(ref)); err != nil {
			t.Fatalf("live append for %s: %v", e.identity, err)
		}
	}
	return live
}

// writeAll puts every execution on the trail its authority belongs to.
func writeAll(t *testing.T) (*approve.MemorySink, *disclose.MemorySink) {
	t.Helper()
	asink, dsink := approve.NewMemorySink(), disclose.NewMemorySink()
	for _, e := range executions {
		if e.gated() {
			writeGated(t, asink, e)
			continue
		}
		writeDisclosed(t, dsink, e)
	}
	return asink, dsink
}

// archives adapts the two sinks to the reader's interface.
func archives(asink *approve.MemorySink, dsink *disclose.MemorySink) []Archive {
	return []Archive{ApprovalArchive(asink), DisclosureArchive(dsink)}
}

// TestLedger_ReproducesTheLiveLedgerAcrossBothTrails is the criterion T5 carried over from
// T3, extended across the artifact boundary as that comment asked.
//
// Nothing is handed to the rebuild but two sinks. Every body it reads was written by the
// production renderer, every artifact it enumerates was closed first, and the entries it
// derives must match — field for field, and in the same window order — the ledger that was
// appended to as the executions happened.
func TestLedger_ReproducesTheLiveLedgerAcrossBothTrails(t *testing.T) {
	asink, dsink := writeAll(t)
	live := liveLedger(t, asink, dsink)

	rebuilt := trust.NewMemory()
	report, err := Ledger(context.Background(), rebuilt, archives(asink, dsink)...)
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}

	if report.Artifacts != len(executions) {
		t.Errorf("the rebuild read %d artifacts, want %d", report.Artifacts, len(executions))
	}
	if report.Finished != len(executions) {
		t.Errorf("the rebuild derived %d entries, want %d", report.Finished, len(executions))
	}
	if report.InFlight != 0 {
		t.Errorf("the rebuild found %d in-flight artifacts, want 0", report.InFlight)
	}
	// Both trails must have contributed. A rebuild that silently read only one would
	// still look healthy on the counts above if the other happened to be empty.
	if report.ByTrail[TrailApproval] == 0 {
		t.Error("the rebuild read nothing from the approval trail — the promotion evidence is exactly what lives there")
	}
	if report.ByTrail[TrailDisclosure] == 0 {
		t.Error("the rebuild read nothing from the disclosure trail")
	}

	liveEntries, rebuiltEntries := live.Entries(), rebuilt.Entries()
	if len(liveEntries) != len(rebuiltEntries) {
		t.Fatalf("the rebuilt ledger has %d entries, the live one has %d", len(rebuiltEntries), len(liveEntries))
	}
	for i := range liveEntries {
		if liveEntries[i] != rebuiltEntries[i] {
			t.Errorf("entry %d differs:\n  live:    %+v\n  rebuilt: %+v", i, liveEntries[i], rebuiltEntries[i])
		}
	}

	// The arithmetic the entries exist for must also agree. Comparing the standing rather
	// than only the entries is what catches a difference that survives field equality — an
	// ordering that puts the same entries in a different window.
	for _, shape := range shapes {
		if want, got := live.Trust(shape), rebuilt.Trust(shape); want != got {
			t.Errorf("shape %s: rebuilt trust %+v, live trust %+v", shape, got, want)
		}
	}
}

// TestLedger_ReadsTheGatedTrailsEvidenceForAutonomy is the T3 carry-over stated as its own
// property, because it is the one a disclosure-only marker would have silently failed.
//
// Promotion counts human-approved converged executions. Those live on the APPROVAL trail,
// so a rebuild that read only disclosures would reproduce every failure that re-gates a
// shape and none of the approvals that earn it — a ledger that looks correct and withholds
// autonomy forever.
func TestLedger_ReadsTheGatedTrailsEvidenceForAutonomy(t *testing.T) {
	asink, dsink := writeAll(t)

	fromBoth := trust.NewMemory()
	if _, err := Ledger(context.Background(), fromBoth, archives(asink, dsink)...); err != nil {
		t.Fatalf("Ledger (both trails): %v", err)
	}

	restarts := shapes[0]
	promoting := 0
	for _, e := range fromBoth.Entries() {
		if e.Shape == restarts && e.Promotes() {
			promoting++
		}
	}
	if promoting != 3 {
		t.Fatalf("the rebuild recovered %d promoting entries for %s, want the 3 the approval trail records",
			promoting, restarts)
	}

	// And the counterfactual: reading only the disclosure trail recovers none of them.
	disclosureOnly := trust.NewMemory()
	if _, err := Ledger(context.Background(), disclosureOnly, DisclosureArchive(dsink)); err != nil {
		t.Fatalf("Ledger (disclosure only): %v", err)
	}
	for _, e := range disclosureOnly.Entries() {
		if e.Promotes() {
			t.Fatalf("a disclosure-only rebuild produced a promoting entry %+v, "+
				"so this test cannot show that the approval trail is what carries the evidence", e)
		}
	}
}

// TestLedger_RefusesAnUnreadableMarkerAndLeavesTheLedgerAlone is the asymmetry the whole
// package turns on: a lost failure re-grants autonomy, a lost approval merely delays it.
// So an artifact whose marker exists and cannot be parsed aborts the rebuild, and the
// ledger keeps whatever it had rather than being replaced by a shorter history.
func TestLedger_RefusesAnUnreadableMarkerAndLeavesTheLedgerAlone(t *testing.T) {
	asink, dsink := writeAll(t)
	ctx := context.Background()

	// Corrupt one closed artifact's marker, the way a hand-edit or a half-written body
	// would. Which one does not matter; that it is not skipped does.
	all, err := asink.ListAll(ctx)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	victim := all[0]
	corrupted := strings.Replace(victim.Body, `{"v":1,`, `{"v":1,"records":`, 1)
	if corrupted == victim.Body {
		t.Fatalf("the marker was not corrupted, so this test proves nothing:\n%s", victim.Body)
	}
	if err := asink.Update(ctx, victim.Ref, "approval", corrupted, []string{approve.ManagedLabel}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// A ledger with existing history, so "left alone" is observable.
	ledger := trust.NewMemory()
	if err := ledger.RecordLifecycle(executions[0].records("pre-existing")); err != nil {
		t.Fatalf("seeding the ledger: %v", err)
	}
	before := ledger.Entries()

	_, err = Ledger(ctx, ledger, archives(asink, dsink)...)
	if err == nil {
		t.Fatal("a corrupt marker was skipped rather than refused")
	}
	var unreadable *UnreadableError
	if !errors.As(err, &unreadable) {
		t.Fatalf("error is %T (%v), want *UnreadableError so a caller can tell it from a transport failure", err, err)
	}
	if unreadable.Trail != TrailApproval || unreadable.Ref != string(victim.Ref) {
		t.Errorf("the error names %s artifact %q, want %s artifact %q",
			unreadable.Trail, unreadable.Ref, TrailApproval, victim.Ref)
	}

	after := ledger.Entries()
	if len(before) != len(after) {
		t.Fatalf("the ledger was modified by a failed rebuild: %d entries before, %d after", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("entry %d changed during a failed rebuild:\n  before: %+v\n  after:  %+v", i, before[i], after[i])
		}
	}
}

// TestLedger_TreatsAnInFlightArtifactAsNothingToContribute. An artifact is opened BEFORE
// its action runs, so a body with no lifecycle marker is the ordinary state of an action
// in progress — reported in the count, never an error, and never an entry.
func TestLedger_TreatsAnInFlightArtifactAsNothingToContribute(t *testing.T) {
	asink, dsink := writeAll(t)
	ctx := context.Background()

	// One more disclosure, opened and not completed: the action started and has not
	// reported back.
	trail, err := disclose.NewTrail(dsink, nil)
	if err != nil {
		t.Fatalf("building the disclosure trail: %v", err)
	}
	inFlight := execution{"e8", "prod", remediate.OpRolloutRestart, audit.AuthorityPolicy, "", "", false, false, base.Add(time.Hour)}
	if _, err := trail.Open(ctx, disclose.Action{
		Proposal: inFlight.proposal(),
		Verdict: autonomy.Verdict{
			Decision: autonomy.DecisionAutoApply,
			Rule:     "restart-unhealthy",
			Evidence: "3 human-approved converged executions of this shape",
		},
		Grant: budget.Grant{Reason: budget.ReasonAdmitted, Cluster: inFlight.cluster, Target: inFlight.identity},
		Mode:  "enforce",
		At:    inFlight.at,
	}); err != nil {
		t.Fatalf("opening the in-flight disclosure: %v", err)
	}

	rebuilt := trust.NewMemory()
	report, err := Ledger(ctx, rebuilt, archives(asink, dsink)...)
	if err != nil {
		t.Fatalf("an in-flight artifact failed the rebuild: %v", err)
	}
	if report.InFlight != 1 {
		t.Errorf("InFlight = %d, want 1", report.InFlight)
	}
	if report.Artifacts != len(executions)+1 {
		t.Errorf("the rebuild read %d artifacts, want %d", report.Artifacts, len(executions)+1)
	}
	if report.Finished != len(executions) {
		t.Errorf("the rebuild derived %d entries, want %d — an unfinished action must contribute none",
			report.Finished, len(executions))
	}
	if got := rebuilt.Len(); got != len(executions) {
		t.Errorf("the rebuilt ledger holds %d entries, want %d", got, len(executions))
	}
}

// TestLedger_IsIdempotent. A rebuild is the operation an operator reaches for when they
// distrust the file, so running it twice must not change the answer — and running it over
// a ledger that is already correct must be a no-op rather than a duplication.
func TestLedger_IsIdempotent(t *testing.T) {
	asink, dsink := writeAll(t)
	ctx := context.Background()

	ledger := trust.NewMemory()
	if _, err := Ledger(ctx, ledger, archives(asink, dsink)...); err != nil {
		t.Fatalf("first rebuild: %v", err)
	}
	first := ledger.Entries()

	if _, err := Ledger(ctx, ledger, archives(asink, dsink)...); err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	second := ledger.Entries()

	if len(first) != len(second) {
		t.Fatalf("a second rebuild changed the entry count: %d -> %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("entry %d changed on the second rebuild:\n  first:  %+v\n  second: %+v", i, first[i], second[i])
		}
	}
}

// TestLedger_RefusesWithNoLedgerToRebuild. Silently succeeding here would report a
// successful rebuild to an operator who has just been given no ledger at all.
func TestLedger_RefusesWithNoLedgerToRebuild(t *testing.T) {
	asink, dsink := writeAll(t)
	if _, err := Ledger(context.Background(), nil, archives(asink, dsink)...); err == nil {
		t.Error("a rebuild with no ledger reported success")
	}
}
