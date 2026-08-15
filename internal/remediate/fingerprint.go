package remediate

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// This file answers one question: are the fix a human approved and the fix MaKlaude
// is about to run unattended the SAME fix?
//
// [ProposalIdentity] cannot answer it. Identity exists to make a proposal survive the
// cluster ticking over — it is operation plus cluster plus kind/namespace/name, and
// it deliberately ignores everything that shifts while the underlying situation
// persists, so that a pending approval is not re-asked every cycle. That is exactly
// the right key for "is this the same pending decision", and exactly the wrong key
// for "does a past approval still cover this", because the things identity discards
// include the ones that change what the action would actually do.
//
// A [Fingerprint] is the second key. It is coarser than the proposal and finer than
// the identity: two proposals share a fingerprint when running one is materially the
// same act as running the other, and differ when a person who approved the first
// would have been shown something different for the second.

// PlannerVersion is the version of the proposal logic in this package: the rules that
// turn a diagnosed cause into an action, the preconditions attached to it, and the
// reversibility assigned to it.
//
// It is part of every [Fingerprint], which is the whole reason it exists. Trust is a
// cached judgment about a fix, and the fix is not only the operation and the object
// — it is also the reasoning that decided this operation was the right response and
// these preconditions were enough to guard it. When that reasoning changes, the
// cached approval was given for something this build no longer produces, so it must
// not carry over.
//
// BUMP THIS whenever a change in this package alters what a proposal would do or
// which guards it carries: a new or removed precondition, a different reversibility
// classification, a different operation chosen for a cause, a changed precondition
// expectation. Do NOT bump it for a reworded [Precondition.Description] or
// [Proposal.Title] — prose is not part of the fingerprint (see [Proposal.Fingerprint])
// and bumping for it would re-gate every shape on the planet to fix a typo.
//
// The cost of bumping when you did not need to is that every trusted shape returns to
// the human gate once and re-earns. The cost of not bumping when you should have is
// that autonomy keeps firing on an approval that was given for a different action.
// Those are not symmetric, so when it is unclear, bump.
const PlannerVersion = 1

// fingerprintScheme prefixes every rendered [Fingerprint].
//
// It is a format version for the fingerprint's own composition, distinct from
// [PlannerVersion], and the two move for different reasons: the planner version
// changes when the FIX changes, this changes when the way a fix is summarized
// changes. Both invalidate every outstanding fingerprint, which is correct in both
// cases — a fingerprint computed under different rules is not comparable to one
// computed under these — but keeping them separate means a reader of a stored token
// can tell which kind of change happened.
const fingerprintScheme = "fp1"

// Fingerprint is a validity token for one fix: a short, opaque, deterministic
// summary of everything about a [Proposal] that a cached approval depends on.
//
// Trust attaches to it. A shape that has earned autonomy has earned it for the
// fingerprints a human actually approved, and a proposal whose fingerprint is not one
// of those returns to the gate however good the shape's record is. That is the whole
// of the invalidation model — see the package doc of [trust] for the other half, which
// is what still ends trust when the fingerprint has NOT changed.
//
// It is opaque on purpose. The inputs include a namespace and an object name, which
// are cluster-derived strings the audit trail is careful about; hashing them means the
// token can be written to a world-readable artifact, compared for equality, and used
// as a map key, while carrying no content anyone has to redact. Equality is the only
// operation it supports, and equality is the only operation the model needs.
//
// The zero value is the empty fingerprint. It is not equal to any computed
// fingerprint, so it can never authorize anything — which is the correct reading of a
// ledger entry recorded before fingerprints existed, or one whose artifact did not
// carry the field. See [trust.Entry.Fingerprint].
type Fingerprint string

// String renders the fingerprint. It is already a string; the method exists so the
// type satisfies the same fmt contract as the other identifiers in this package.
func (f Fingerprint) String() string { return string(f) }

// Fingerprint computes this proposal's [Fingerprint].
//
// It is pure and deterministic: the same proposal produces the same token in every
// process, on every build with the same [PlannerVersion]. Nothing here reads a clock,
// a cluster, or the environment — the trust decision that consumes it has to be
// reproducible from a stored record, and a fingerprint that varied between two
// computations over the same proposal would make a shape lose and regain autonomy at
// random.
//
// # What is in, and why each field is in
//
//   - [PlannerVersion] — the reasoning that produced the action. See there.
//   - Operation — the act itself. Restarting and deleting are not the same fix.
//   - The target's cluster, kind, namespace, and name — WHICH object. This is the
//     field the whole issue was about: without it a shape's trust covers an operation
//     class, so three approved restarts on one deployment authorize restarting every
//     deployment in the cluster, including ones that did not exist when the approvals
//     were given.
//   - Cause — WHY. The same restart proposed for a bad image and for an OOM kill are
//     the same keystrokes and not the same decision, and a person approving the first
//     was shown the first. [diagnose.Cause] is a small stable enum, not free text, so
//     including it costs no churn for a fault that persists across cycles.
//   - Reversibility — how hard it is to undo. [PreconditionPodHasController] says it
//     directly: the same pod deletion is a different action when nothing will recreate
//     the pod, and it must be rechecked rather than assumed.
//   - The set of precondition KINDS — which guards the fix carries. A proposal that
//     dropped a guard is a weaker action than the one that was approved.
//   - The expected value of the preconditions that BIND (see
//     [PreconditionKind.BindsFingerprint]) — the parts of the target's spec the fix
//     actually depends on.
//
// # What is deliberately out, and why
//
//   - The target's resourceVersion, and [PreconditionUnchanged]'s expectation of it.
//     It changes on every write to the object by anyone, so including it would give
//     every proposal a fresh fingerprint every cycle: no shape would ever be trusted
//     twice, which is the "too fine" column of the issue's table and would present as
//     autonomy silently never working. Drift is already handled, and handled better,
//     at execution time — the precondition is sent to the API server and a moved
//     target aborts the action cleanly.
//   - [PreconditionPodCrashLooping]'s expectation, which is the crashlooping pod's
//     namespace and name. For a Deployment target that pod is a different object after
//     every restart, so it is an instance of the symptom rather than a property of the
//     fix. The KIND is still in, so a proposal that stopped checking for a crashloop
//     at all does get a new fingerprint.
//   - Hypothesis and Confidence. Both are provenance for the diagnosis rather than
//     content of the fix, and confidence in particular moves as evidence accumulates
//     for a situation that has not changed. Identity already collapses two hypotheses
//     that independently reach the same action, and splitting trust back apart here
//     would undo that on purpose.
//   - Every prose field — Title, Intent, ExpectedEffect, and each
//     [Precondition.Description]. Trust must not lapse because someone fixed a typo.
//     This is the field group most likely to be added here by mistake, because it is
//     what a human actually reads on the approval; what they are consenting to is the
//     act, and the act is described by the structured fields above.
//   - Evidence. It is a list of findings that grows and shrinks as the collector
//     samples, and it justifies the proposal without changing what the proposal does.
//   - ProposedAt. It is a clock reading, and a fingerprint that moved with it would
//     make every proposal unique.
func (p Proposal) Fingerprint() Fingerprint {
	sum := sha256.Sum256([]byte(fingerprintPreimage(p)))
	// Half the digest. 128 bits is far past any collision concern for a value space
	// bounded by the objects in a handful of clusters, and the shorter token is one a
	// person can compare by eye in an escalation without their gaze sliding off it.
	return Fingerprint(fingerprintScheme + ":" + hex.EncodeToString(sum[:16]))
}

// fingerprintPreimage is the exact byte sequence [Proposal.Fingerprint] hashes.
//
// It is separated out so a test can assert what went INTO a fingerprint rather than
// only that two fingerprints differ. A digest is one-way by design, so "did this field
// contribute?" is otherwise answerable only by mutating the field and watching the
// output move — which cannot distinguish a field that was included from one that
// merely perturbed something else.
func fingerprintPreimage(p Proposal) string {
	// A length-prefixed encoding rather than a delimiter-joined one. Namespaces and
	// object names are cluster-controlled strings, so any separator character can
	// appear inside a field; joining on one would let two different proposals encode
	// to the same bytes, and a fingerprint collision is a proposal inheriting an
	// approval that was given for something else.
	var b strings.Builder
	field := func(s string) {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}

	field(fingerprintScheme)
	field(strconv.Itoa(PlannerVersion))
	field(string(p.Operation))
	field(p.Target.Cluster)
	field(p.Target.Kind)
	field(p.Target.Namespace)
	field(p.Target.Name)
	field(string(p.Cause))
	field(strconv.Itoa(int(p.Reversibility)))

	// Preconditions are sorted before hashing. The planner emits them in a fixed
	// order today, so this changes nothing now; it means a reordering later is not
	// silently a mass invalidation of every outstanding approval, which would look
	// exactly like a bug and would be found only by an operator wondering why
	// everything re-gated.
	guards := make([]string, 0, len(p.Preconditions))
	for _, pre := range p.Preconditions {
		if !pre.Kind.InFingerprint() {
			continue
		}
		guard := string(pre.Kind)
		if pre.Kind.BindsFingerprint() {
			guard += "=" + pre.Expect
		}
		guards = append(guards, guard)
	}
	sort.Strings(guards)
	field(strconv.Itoa(len(guards)))
	for _, g := range guards {
		field(g)
	}

	return b.String()
}

// InFingerprint reports whether this precondition kind contributes to
// [Proposal.Fingerprint] at all.
//
// Exactly one kind is excluded: [PreconditionUnchanged], which is present on every
// proposal and therefore distinguishes nothing, and whose expectation is the
// resourceVersion — see [Proposal.Fingerprint] on why that must stay out. Every other
// kind counts, and the default is to count, so a precondition added to this package
// later is part of the fingerprint unless someone deliberately excludes it. That
// direction is the fail-closed one: forgetting to include a guard means trust
// survives a change that dropped it.
func (k PreconditionKind) InFingerprint() bool { return k != PreconditionUnchanged }

// BindsFingerprint reports whether this kind's [Precondition.Expect] is part of the
// fix, rather than a property of the moment the proposal was computed.
//
// It is an allowlist of exactly one kind rather than a denylist, which is the opposite
// direction from [PreconditionKind.InFingerprint] and deliberately so. The two
// questions have opposite fail-closed answers: a guard that vanishes from the
// fingerprint lets stale trust through, so kinds are included by default; an
// expectation that churns re-gates everything forever, so values are excluded by
// default. A new kind whose value genuinely identifies the fix must be added here,
// and the symptom of forgetting is that trust is slightly broader than it could be —
// visible, bounded, and recoverable, unlike autonomy that never fires.
//
//   - [PreconditionRevisionExists] binds. Its expectation is the revision a rollback
//     would land on, and rolling a deployment back to revision 5 and to revision 9 are
//     two different fixes with two different outcomes. A human who approved one has
//     not approved the other.
//   - [PreconditionPodCrashLooping] does not. Its expectation names the crashlooping
//     pod, which for a Deployment target is a new object after every restart.
//   - Every other kind carries no expectation at all, so the question is moot for
//     them and the answer costs nothing.
func (k PreconditionKind) BindsFingerprint() bool { return k == PreconditionRevisionExists }
