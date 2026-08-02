package approve

import (
	"errors"
	"fmt"
	"strings"
)

// This file is the autonomous-mode bypass: the one way MaKlaude is permitted to act
// on a cluster with no human in the loop, and the requirement that pairs with it.
//
// # Why a bypass exists at all
//
// Everything else in this package is built on the premise that a person decides. That
// premise is right for the deployment MaKlaude ships as, and wrong for the operator
// who has watched it for a month, trusts the four operations in the catalog, and wants
// the loop to close overnight. Before this, such an operator's only option was to leave
// the gate on and approve everything by hand, or to fork the gate out — and a safety
// mechanism people route around is worse than one with a documented exit, because the
// route around it is neither logged nor auditable.
//
// So the exit is explicit, hostile to accidents, and recorded everywhere it is taken.
// It is modelled on `claude --dangerously-skip-permissions`: the name carries the
// warning, the default is off, and nothing infers it from a neighbouring setting.
//
// # What it waives, and what it emphatically does not
//
// It waives CONSENT and nothing else. The gate asks two separable questions —
// "did somebody say yes?" and "does this still make sense to run?" — and only the
// first is a question about a human. So under the bypass:
//
//   - a human's `rejected` label still stops the action, because a person saying no
//     outranks configuration saying go;
//   - resourceVersion drift still refuses, because the action MaKlaude previewed and
//     the action now possible are still not the same action;
//   - a failed dry-run, a missing rollback plan, and the executed-label idempotency
//     flag all still block, because none of them is about consent;
//   - [execute.Runner] still re-checks every precondition the artifact displayed, and
//     the [kube] kill switch still governs whether a real write happens. Auto-approval
//     under [kube.ExecuteDryRun] previews and changes nothing.
//
// # It never fakes a human
//
// An [Authorization] granted this way carries [AuthorityPolicy] and an approver of
// [AutoApprovePolicy] — a marker, not a login. Every renderer that names an approver
// asks the authority first, so a waived action reads as waived in the issue comment,
// in the execution note, and in the audit trail. A trail that claimed a person
// reviewed something no person saw would launder an unreviewed action into a reviewed
// one, permanently, in the artifact an incident review trusts. That is worse than
// having no gate: no gate is at least honest about itself.
//
// # The bypass is what makes the honest path affordable
//
// The pairing with [ErrSelfIdentityUnknown] is the point of shipping the two together.
// Before this, [SelfLoginEnv] was unset everywhere and [GitHubSink.isSelfActor] fell
// back to its bot heuristic, which catches MaKlaude running as a GitHub App and misses
// it entirely when it runs as a person's account — the `genesis serve` local mode,
// where MaKlaude and the operator share one identity. The check failed OPEN: MaKlaude
// could label its own approval issue and the gate read a human approval.
//
// Requiring the self-login closes that. It could not be required before, because in
// local mode the operator's own approvals would then be refused as self-approval and
// the gate would be unusable — there was no third answer. The bypass is the third
// answer: run unattended and say so, or tell MaKlaude who it is. What is no longer
// available is the silent middle, where the gate looks armed and is not.

// AutoApproveEnv is the environment variable that turns the approval requirement off.
//
// The name is deliberately unpleasant. It contains DANGEROUSLY because the variable
// will be read by someone skimming a deployment manifest years from now, and the only
// reliable place to put a warning is in the thing they will actually read.
const AutoApproveEnv = "MAKLAUDE_DANGEROUSLY_AUTO_APPROVE"

// AutoApprovePolicy is the approver recorded on an [Authorization] the bypass granted.
// It names the policy that waived the requirement, which is the most useful thing an
// incident reviewer can be handed: the exact knob to go and look at.
//
// It is structurally impossible for it to be mistaken for a person. A GitHub login is
// alphanumerics and hyphens only, so the colon in this value cannot occur in one — a
// reader (or a script) that sees it cannot conclude a human is named, whatever else
// goes wrong upstream. That is a stronger guarantee than a value that merely looks
// unlikely, like "maklaude-policy", which is a perfectly registrable username.
const AutoApprovePolicy = "policy:" + AutoApproveEnv

// ErrAmbiguousAutoApprove reports a value of [AutoApproveEnv] that is neither clearly
// on nor clearly off.
//
// Refusing is the whole point, and the direction of the danger is worth spelling out.
// The lazy parse — "non-empty means on" — turns `MAKLAUDE_DANGEROUSLY_AUTO_APPROVE=no`
// and `=off` and `=disabled` into an armed autonomous mode set by someone who was
// trying to disable it. The opposite lazy parse — "only the exact string 1 means on" —
// silently ignores `=yes` from someone who was trying to enable it, which is merely
// confusing. Neither guess is acceptable for a switch this size, so an unrecognized
// value stops the process instead of picking one.
var ErrAmbiguousAutoApprove = errors.New("approve: the autonomous-mode switch is set to a value that is neither clearly on nor clearly off")

// ErrSelfIdentityUnknown reports that MaKlaude does not know which account it acts as
// on the comms trail, while still claiming to enforce a human approval gate.
//
// It is an error rather than a warning because the failure it prevents is invisible.
// A MaKlaude that cannot recognize its own label events does not behave like a broken
// gate — it behaves like a working one that happens to approve everything MaKlaude
// asks for, and nothing in the trail says otherwise.
var ErrSelfIdentityUnknown = errors.New("approve: MaKlaude's own GitHub login is not configured, so it cannot tell its own approvals from a human's")

// AutoApproveFromEnv reports whether the autonomous-mode bypass is enabled.
//
// Accepted: "" / "0" / "false" for off, "1" / "true" for on, case-insensitively and
// ignoring surrounding whitespace. Everything else is [ErrAmbiguousAutoApprove] — see
// there for why guessing in either direction is unacceptable.
//
// A nil getenv reads as off, so a caller that has no environment to consult gets the
// safe posture rather than a panic.
func AutoApproveFromEnv(getenv func(string) string) (bool, error) {
	if getenv == nil {
		return false, nil
	}
	raw := strings.TrimSpace(getenv(AutoApproveEnv))
	switch strings.ToLower(raw) {
	case "", "0", "false":
		return false, nil
	case "1", "true":
		return true, nil
	default:
		return false, fmt.Errorf("%w: %s=%q; use %s=1 to enable it (and read docs/autonomous-mode.md first) or unset it to keep the human approval gate",
			ErrAmbiguousAutoApprove, AutoApproveEnv, raw, AutoApproveEnv)
	}
}

// GateConfig is the approval gate's resolved environment posture: who MaKlaude is on
// the comms trail, and whether the requirement for a human decision has been waived.
//
// The two live in one value because they are one decision. Either MaKlaude knows its
// own identity and can therefore promise that an approval came from somebody else, or
// the operator has explicitly said no such promise is being made. Reading the two
// variables independently at two call sites is how the third state — neither known nor
// waived, the state this change exists to eliminate — gets reintroduced.
type GateConfig struct {
	// SelfLogin is the account MaKlaude itself acts as, from [SelfLoginEnv]. Empty is
	// permitted only when AutoApprove is true; see [GateConfig.Check].
	SelfLogin string

	// AutoApprove is the autonomous-mode bypass, from [AutoApproveEnv].
	AutoApprove bool
}

// GateConfigFromEnv reads both variables, returning [ErrAmbiguousAutoApprove] if the
// bypass switch cannot be read as a yes or a no.
//
// It deliberately does NOT enforce the self-login requirement — [GateConfig.Check]
// does, and the split is because the requirement depends on something this function
// cannot see: whether a live comms trail is configured at all. See
// [GateConfig.Check] for that argument.
func GateConfigFromEnv(getenv func(string) string) (GateConfig, error) {
	auto, err := AutoApproveFromEnv(getenv)
	if err != nil {
		return GateConfig{}, err
	}
	return GateConfig{SelfLogin: SelfLoginFromEnv(getenv), AutoApprove: auto}, nil
}

// Check reports whether this posture is a coherent one to run a LIVE approval trail
// under, returning [ErrSelfIdentityUnknown] when it is not.
//
// The rule is one sentence: if the requirement for a human approval is still in force,
// MaKlaude must know which account it is, because the self-approval refusal is the
// narrowest and most important forgery the gate has to survive and it cannot fire
// against an identity nobody named. If the requirement has been waived, the identity
// is merely useful and its absence is not a lie.
//
// # Why the caller decides when to apply it, rather than the constructor
//
// Two things could enforce this instead and neither can: [NewGitHubSink] cannot,
// because it has no way to see the bypass, so a constructor that hard-required a login
// would make the sanctioned unattended deployment unrepresentable; and the degraded
// [MemorySink] path must not, because an in-memory artifact can never receive a
// decision from anyone outside this process — there is no labeler there to mistake for
// a human, so requiring an identity would impose startup friction on a credential-less
// deployment in exchange for closing a hole that does not exist in it. So the check
// belongs exactly where both facts are known, which is [SinkFromEnv].
func (c GateConfig) Check() error {
	if c.AutoApprove || strings.TrimSpace(c.SelfLogin) != "" {
		return nil
	}
	return fmt.Errorf("%w: set %s to the login MaKlaude's %s belongs to, so a decision label it applied to its own artifact is recognized and refused. "+
		"If MaKlaude is meant to run unattended, set %s=1 instead and read docs/autonomous-mode.md — but do not leave both unset, which looks like a human gate and is not one",
		ErrSelfIdentityUnknown, SelfLoginEnv, "MAKLAUDE_GITHUB_TOKEN", AutoApproveEnv)
}
