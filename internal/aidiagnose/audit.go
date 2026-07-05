package aidiagnose

import (
	"context"
	"log"

	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
)

// AuditOutcome classifies how one refinement attempt for a single incident
// resolved. It is the auditable heart of the T5 boundary: every attempt records
// exactly one outcome, so the trail always explains whether cluster-derived
// (redacted) data actually egressed to the model and what came back — or, when
// the layer degraded, precisely why it fell back to the deterministic hypotheses.
type AuditOutcome string

const (
	// OutcomeRefined means the provider was called and its suggestions were folded
	// into the hypotheses. This is the only outcome in which redacted evidence
	// actually left the process.
	OutcomeRefined AuditOutcome = "refined"

	// OutcomeNoChange means the provider was called but returned nothing usable, so
	// the deterministic hypotheses were kept unchanged. Redacted evidence egressed,
	// but no refinement resulted.
	OutcomeNoChange AuditOutcome = "no-change"

	// OutcomeSkippedBudget means the per-cycle call budget was exhausted, so no
	// provider call was made and nothing egressed for this incident.
	OutcomeSkippedBudget AuditOutcome = "skipped-budget"

	// OutcomeError means the provider call failed or timed out, so the layer
	// degraded to the deterministic hypotheses. Whether any evidence egressed
	// depends on the failure, but no suggestion was applied.
	OutcomeError AuditOutcome = "error"
)

// AuditRecord is the auditable trace of one refinement attempt for one incident.
// It deliberately carries NO evidence text and NO suggestion content — only
// metadata about the call — so the audit trail itself can never become a second
// egress path for sensitive data. It answers "what did MaKlaude ask the model to
// do, for which incident, and how did it turn out", which is exactly what an
// operator reviewing the safety boundary needs.
type AuditRecord struct {
	// Cluster is the registered cluster whose incident was being refined.
	Cluster string

	// Incident is the stable identity of the incident under refinement, so the
	// record ties back to the exact problem (and its issue in the comms trail).
	Incident correlate.IncidentIdentity

	// Purpose is a short, fixed description of why the model was consulted (for
	// example "refine root-cause hypotheses"). It is the human-readable "why" the
	// T5 boundary requires in the trail.
	Purpose string

	// Model is the model id the request targeted, so the trail records which
	// external system the (redacted) evidence was sent to.
	Model string

	// EvidenceBytes is the size of the redacted evidence payload, so cost and
	// egress volume are auditable without recording the payload itself.
	EvidenceBytes int

	// Outcome classifies how the attempt resolved. See [AuditOutcome].
	Outcome AuditOutcome

	// Detail is optional extra context, chiefly the (already non-sensitive) error
	// text when Outcome is [OutcomeError]. It is empty on success.
	Detail string
}

// Auditor records each provider call's purpose and outcome to MaKlaude's trail.
// It is the T5 "audited" guarantee made pluggable: the refiner emits one
// [AuditRecord] per refinement attempt, and an Auditor persists it however the
// deployment prefers. Implementations must be safe for concurrent use and must
// never block the scan — an audit sink that cannot keep up should drop, not stall.
type Auditor interface {
	// Record persists one audit record. It must not panic and should return
	// promptly; ctx carries the surrounding call's cancellation.
	Record(ctx context.Context, rec AuditRecord)
}

// LogAuditor is the default [Auditor]: it writes each record as a single
// structured line to a standard-library [log.Logger] (stderr by default). It
// keeps the audit trail visible in process logs with zero extra infrastructure,
// mirroring how the rest of MaKlaude leans on the standard toolchain rather than
// a bespoke logging stack. It records only metadata, never evidence, so the log
// can never become an egress path.
type LogAuditor struct {
	// logger is the destination; nil uses the standard logger ([log.Default]).
	logger *log.Logger
}

// NewLogAuditor builds a [LogAuditor] writing to the given logger, or to
// [log.Default] when logger is nil.
func NewLogAuditor(logger *log.Logger) *LogAuditor {
	return &LogAuditor{logger: logger}
}

// Record writes the record as one structured line. It never blocks on anything
// but the logger's own write and never panics.
func (a *LogAuditor) Record(_ context.Context, rec AuditRecord) {
	logger := a.logger
	if logger == nil {
		logger = log.Default()
	}
	detail := ""
	if rec.Detail != "" {
		detail = " detail=" + rec.Detail
	}
	logger.Printf("maklaude/aidiagnose audit: cluster=%q incident=%q purpose=%q model=%q evidence_bytes=%d outcome=%s%s",
		rec.Cluster, rec.Incident, rec.Purpose, rec.Model, rec.EvidenceBytes, rec.Outcome, detail)
}

// nopAuditor discards every record. It is the safe default when no [Auditor] is
// wired, so the refiner can always call Record without a nil check.
type nopAuditor struct{}

// Record discards the record.
func (nopAuditor) Record(context.Context, AuditRecord) {}
