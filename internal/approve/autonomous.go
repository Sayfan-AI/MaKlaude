package approve

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/budget"
)

// This file mints the second kind of permission slip: one that no person signed and
// that names the earned rule which stood in for them.
//
// # Why it lives here rather than where autonomy is decided
//
// [Authorization] is unforgeable outside this package by construction — `granted` is
// unexported and only [grantAs] sets it — and that property is load-bearing enough
// that the execution layer's entire authorization check is `auth.Valid()`. Minting a
// policy grant somewhere else would mean exporting a way to build one, which is the
// same as deleting the property. So the mint comes to the guarantee rather than the
// other way round, and this package takes a dependency on [autonomy] and [budget] to
// do it.
//
// # Why it takes both a verdict and a grant
//
// [autonomy]'s package doc is explicit that [autonomy.DecisionAutoApply] means "this
// proposal is ELIGIBLE", never "go", and [budget]'s is equally explicit that admission
// is the ceiling that turns eligibility into permission. Between them sits exactly one
// mistake worth designing out: a caller that runs the policy check, skips the budget,
// and acts. Written as prose in two package docs, that rule is remembered; written as
// two required arguments neither of which has a permissive zero value, it is enforced.
//
// Neither argument is unforgeable — both are plain structs a caller could fill in by
// hand — and this file does not pretend otherwise. The human path's unforgeability
// comes from a label event on a real artifact that MaKlaude cannot apply to itself;
// there is no equivalent for a decision computed in-process, and claiming one would be
// worse than not having it. What this function does buy is that the unsafe combination
// is unrepresentable in the shape of the call, that "who permitted this" is answered by
// a value rather than by a convention, and that there is exactly ONE place to grep.
//
// # Why the evidence is required
//
// [autonomy] already refuses a trust oracle that declares a shape trusted while citing
// nothing ([autonomy.ReasonTrustEvidenceMissing]), for a reason this layer inherits:
// nobody approved the action, so the citation is the entire oversight artifact an
// incident review has to work from. Re-checking it here is deliberate duplication. The
// verdict is a value that travelled between two packages, and the cost of the check is
// one string comparison against the cost of a world-readable record that says an action
// was earned and cannot say by what.

// ErrNotAutoApplicable reports that a permission slip was asked for on policy authority
// for something the policy layer did not authorize. It wraps every refusal in this file
// so a caller can branch on the class without matching prose.
var ErrNotAutoApplicable = errors.New("approve: refusing to mint a policy authorization for an action that was not auto-applicable")

// GrantAutonomous mints the permission slip for one action MaKlaude is about to take
// with nobody watching.
//
// It is the auto-apply path's counterpart to the authorize branch of [Decide], and the
// resulting [Authorization] is identical in kind to a human-approved one in every
// respect an executor cares about — it is valid, it is bound to one cluster and one
// object at one resourceVersion, it carries the preconditions to re-check — and
// deliberately different in the two respects a READER cares about:
//
//   - [Authorization.Authority] is [AuthorityPolicy], so every renderer that asks
//     before naming an approver states that no person reviewed this.
//   - [Authorization.Approver] is `policy:<rule-name>` rather than
//     [AutoApprovePolicy]. The distinction is the whole point of Milestone 5: the
//     bypass means a human waived review, an earned rule means a human approved this
//     shape repeatedly and it worked, and a renderer that collapsed the two would be a
//     bug. Naming the specific rule is what makes collapsing them impossible.
//
// ref is the disclosure artifact opened for this action BEFORE it runs. It is required.
// An auto-applied action has no approval artifact — nobody was asked — so without a
// disclosure to point at, [Gatekeeper.RecordExecution]'s counterpart has nowhere to
// write, the executed marker is never applied, and the action happens with no durable
// record anywhere. That is the precise failure this whole task exists to prevent, so it
// is refused at the mint rather than discovered afterwards.
//
// now stamps [Authorization.AuthorizedAt]. [Authorization.ApprovedAt] stays zero: no
// decision was made and there is no instant to record, exactly as under the bypass.
func GrantAutonomous(req Request, v autonomy.Verdict, g budget.Grant, ref ActionRef, now time.Time) (*Authorization, error) {
	p := req.Proposal

	switch {
	case !v.AutoApplies():
		return nil, fmt.Errorf("%w: the policy verdict was %q", ErrNotAutoApplicable, v.String())
	case v.Rule == "":
		// Unreachable through [autonomy.Decide], which only reaches
		// [autonomy.ReasonEarnedTrust] from inside a matched rule. Checked anyway: the
		// rule name is what the record is FOR, and a slip attributing an unattended
		// mutation to `policy:` with nothing after the colon is worse than no slip.
		return nil, fmt.Errorf("%w: the verdict auto-applies but names no rule", ErrNotAutoApplicable)
	case strings.TrimSpace(v.Evidence) == "":
		return nil, fmt.Errorf("%w: rule %q cited no trust evidence, and an unreviewed action's citation is its entire oversight record",
			ErrNotAutoApplicable, v.Rule)
	case !g.Admitted():
		return nil, fmt.Errorf("%w: the blast-radius budget did not admit it (%s)", ErrNotAutoApplicable, g.String())
	case ref == "":
		return nil, fmt.Errorf("%w: no disclosure artifact was opened for it, so the action would run unrecorded", ErrNotAutoApplicable)
	case p.Identity == "":
		return nil, fmt.Errorf("%w: the proposal carries no identity", ErrNotAutoApplicable)
	case g.Cluster != p.Cluster:
		// Multi-cluster isolation, checked at the one point where a policy decision, a
		// ceiling and a proposal meet. Every layer underneath re-checks its own pair;
		// this is the pair no other layer sees.
		return nil, fmt.Errorf("%w: the budget admitted cluster %q and the proposal names %q",
			ErrNotAutoApplicable, g.Cluster, p.Cluster)
	}

	identity := v.PolicyIdentity()
	if identity == AutoApprovePolicy {
		// [autonomy.Rule] validates names as lowercase and [AutoApprovePolicy] is an
		// upper-case environment variable name, so the two cannot collide today. The
		// check is here so that if either spelling ever changes, the failure is a refused
		// grant rather than an earned rule silently rendering as the blanket bypass —
		// which is the exact conflation this milestone is built to prevent.
		return nil, fmt.Errorf("%w: rule %q renders as the blanket bypass marker", ErrNotAutoApplicable, v.Rule)
	}

	return grantAs(req, AuthorityPolicy, identity, time.Time{}, ref, now), nil
}
