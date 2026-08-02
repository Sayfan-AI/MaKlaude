package autonomy

// This file is the seam between the two halves of "earned autonomy": the rules an
// operator writes, which live in this package, and the history that decides whether
// a rule may fire, which does not.
//
// The split is deliberate and the direction of the dependency matters. Deriving
// trust from a recorded execution history — how many prior human-approved runs of
// this shape converged, whether any of them failed or was rolled back, how recently
// — is arithmetic over a durable store, with a clock and a persistence concern and
// a restart-survival requirement. None of that belongs in a pure decision function,
// and all of it belongs to the trust ledger (task T3). So this package states what
// it needs as a one-method interface and stays ignorant of how the answer is
// reached.
//
// What the interface does NOT do is let the ledger declare trust. A [TrustEvidence]
// that says trusted and cites nothing is refused — see [ReasonTrustEvidenceMissing].
// That is the one guard this package can hold against the failure the whole
// milestone exists to avoid: an allowlist a human writes into a config file is a
// blank cheque, MaKlaude already ships one of those under an honest name, and a
// second one wearing the word "earned" would be worse than the first.

// TrustEvidence is a trust ledger's answer about one [Shape]: whether the shape has
// earned autonomy, and what history says so.
//
// The zero value is untrusted with no citation, so a ledger that fails to answer,
// a map miss, and a partially-constructed value all read the same safe way.
type TrustEvidence struct {
	// Trusted reports that this shape's recorded history meets the promotion bar.
	Trusted bool

	// Citation is the short, human-readable record of WHY — the count of prior
	// human-approved executions that converged, the window, and the absence of any
	// failure or rollback. It is required when Trusted is set: nobody approved the
	// action this evidence will authorize, so the citation is the entire oversight
	// artifact an incident review has to work from.
	//
	// It must be stable for stable history. It lands verbatim in the audit trail, and
	// a citation that varied between two calls on the same facts would make the
	// verdict non-deterministic in the one field a human actually reads.
	Citation string
}

// TrustOracle answers whether a [Shape] has earned autonomy. The trust ledger
// implements it; [Decide] consumes it and nothing else about trust.
//
// Implementations must be pure with respect to a fixed recorded history: two calls
// with the same shape against the same history return the same [TrustEvidence],
// citation included. [Decide]'s determinism is only as good as this promise,
// because the oracle is one of its inputs.
//
// A nil TrustOracle is a valid argument to [Decide] and means no ledger is wired
// up, which gates everything — see [ReasonNoTrustLedger].
type TrustOracle interface {
	// Trust reports this shape's standing. It must not block, read a cluster, or
	// depend on when it is called.
	Trust(Shape) TrustEvidence
}

// StaticTrust is a fixed shape-to-citation map satisfying [TrustOracle]: a shape
// present in the map is trusted and its value is the citation, and a shape absent
// from it is untrusted.
//
// It exists for two callers. Tests use it to pin an exact trust posture without
// standing up a ledger, which is what keeps this package's tests pure. And a ledger
// that computes every shape's standing up front can return one of these directly
// rather than implementing the interface itself.
//
// A nil map is a valid StaticTrust and trusts nothing.
type StaticTrust map[Shape]string

// Trust implements [TrustOracle].
func (s StaticTrust) Trust(shape Shape) TrustEvidence {
	citation, ok := s[shape]
	return TrustEvidence{Trusted: ok, Citation: citation}
}
