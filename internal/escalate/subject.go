package escalate

import (
	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
)

// Subject is the unit of escalation: one correlated [correlate.Incident] together
// with its ranked, evidence-backed [diagnose.Hypothesis]es. It is what the comms
// layer opens/updates/closes exactly one tracked issue for.
//
// # Why the escalator keys on incidents, not findings
//
// Milestone 1 escalated at *finding* granularity: one issue per raw
// [detect.Finding]. But a single real cause radiates into a fan of findings — a
// bad image shows up as a crashlooping pod AND a stalled ReplicaSet AND an
// unavailable Deployment — so a finding-per-issue trail buries the operator in N
// issues for what is really one problem, and invites N separate remediations for
// one cause. The correlate and diagnose layers already collapse that fan back
// into one incident and rank *why* it happened; this package now escalates at
// that same incident granularity, so the human-facing trail carries the diagnosis
// (a correlated incident + ranked root-cause hypotheses + the exact evidence),
// not just a raw symptom.
//
// A Subject is a plain value that simply pairs the two upstream results. It holds
// no behaviour beyond small accessors and carries no live references, so — like
// the incident and hypotheses it wraps — it is trivially serializable and
// comparable, and the pure [Reconcile] core can key on it without any I/O.
//
// # Invariants the escalator relies on
//
//   - Identity is the incident's [correlate.IncidentIdentity]: stable across
//     collection cycles for the same ongoing incident (so recurrence dedups to an
//     update, never a duplicate) and cluster-scoped (so multi-cluster isolation is
//     free).
//   - Hypotheses are ordered most-confident-first, exactly as [diagnose.Diagnose]
//     returns them, and there is always at least one (diagnose guarantees a
//     fallback [diagnose.CauseUnknown] hypothesis when no rule matches). The body
//     renderer relies on both: it leads with the top hypothesis and, when that top
//     hypothesis is uncertain, honestly presents the competing ones rather than
//     overcommitting.
type Subject struct {
	// Incident is the correlated problem this subject escalates. Its Identity is
	// the durable dedup key, and its Primary/Effects give the affected objects.
	Incident correlate.Incident

	// Hypotheses are the incident's ranked root-cause hypotheses, most-confident
	// first (as [diagnose.Diagnose] returns them). Never empty for a real incident.
	Hypotheses []diagnose.Hypothesis
}

// Identity returns the subject's stable dedup key — the incident's identity. It
// is what [Reconcile] joins current subjects against currently-tracked issues, so
// the same ongoing incident maps to the same issue across cycles.
func (s Subject) Identity() correlate.IncidentIdentity { return s.Incident.Identity }

// Cluster returns the cluster the incident concerns. A subject is always scoped
// to one cluster (its identity encodes the cluster), so a single Reconcile pass
// over several clusters' subjects preserves isolation.
func (s Subject) Cluster() string { return s.Incident.Cluster }

// Severity returns the incident's severity (its primary finding's severity). It
// drives the needs:human gate and the issue title, exactly as a finding's
// severity did at finding granularity.
func (s Subject) Severity() detect.Severity { return s.Incident.Severity() }

// TopHypothesis returns the most-confident hypothesis (the first, since
// Hypotheses is ranked) and ok=false only for the degenerate empty case, which a
// real incident never produces. Callers use it to lead the body with the leading
// diagnosis and to decide whether to flag competing hypotheses.
func (s Subject) TopHypothesis() (diagnose.Hypothesis, bool) {
	if len(s.Hypotheses) == 0 {
		return diagnose.Hypothesis{}, false
	}
	return s.Hypotheses[0], true
}
