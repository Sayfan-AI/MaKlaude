package operate

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/budget"
	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/disclose"
	"github.com/Sayfan-AI/MaKlaude/internal/execute"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/rules"
	"github.com/Sayfan-AI/MaKlaude/internal/trust"
)

// ExecuteModeEnv is the single environment variable that opts a deployment into the
// write path. Accepted values are the [kube.ParseExecuteMode] vocabulary: "disabled"
// (or unset), "dry-run", "enabled".
//
// It is an environment variable rather than a key in the cluster registry file for the
// same reason the approval flow's configuration is: that file is secret-safe by design
// and is copied, templated and committed, and "the checked-in example turned execution
// on" is a failure mode worth designing out. It is also the reason there is exactly ONE
// of these rather than a per-cluster override — the kill switch's value comes from
// being a single thing an operator can reason about and unset.
const ExecuteModeEnv = "MAKLAUDE_EXECUTE_MODE"

// ExecuteModeFromEnv reads the opt-in from the environment.
//
// An unset or empty value is [kube.ExecuteDisabled] — "the operator said nothing" and
// "the operator said off" are the same posture, and it is the safe one. An
// unrecognized value is an ERROR rather than a default in either direction: guessing
// low would silently ignore an operator who meant to enable execution, and guessing
// high needs no explanation. This mirrors the strictness [approve.AutoApproveFromEnv]
// already applies to its own flag.
func ExecuteModeFromEnv(getenv func(string) string) (kube.ExecuteMode, error) {
	mode, err := kube.ParseExecuteMode(getenv(ExecuteModeEnv))
	if err != nil {
		return kube.ExecuteDisabled, fmt.Errorf("%s: %w", ExecuteModeEnv, err)
	}
	return mode, nil
}

// AutonomyStateEnv points at the file the blast-radius budget keeps its circuit
// breakers and per-target cooldowns in.
//
// Unset — the shipped default — means no budget is constructed, which the state summary
// reports as "not configured". That is the safe posture rather than a permissive one: a
// [budget.Budget] only ever DENIES an auto-apply, so its absence cannot grant anything;
// what it means is that the ceiling autonomy would need does not exist, and every
// proposal therefore takes the human gate.
//
// It is a path rather than a boolean because the state has to outlive the process. A
// breaker that forgets it tripped on restart is not a breaker: the condition that
// tripped it is a cluster MaKlaude is wrong about, and that outlasts any one run.
const AutonomyStateEnv = "MAKLAUDE_AUTONOMY_STATE"

// AutonomyRulesEnv points at the file granting autonomy: the operator's allowlist of
// (cluster, namespace, operation) shapes that may run unattended once they have EARNED
// it. See [rules] for the format and docs/autonomous-mode.md for the whole posture.
//
// Unset — the shipped default — means no rule exists, so [autonomy.Decide] returns
// [autonomy.ReasonAutonomyNotConfigured] for every proposal and every one of them takes
// the human gate. That is byte-for-byte Milestone 4's behaviour.
//
// It is a path to a SEPARATE file rather than a key in the cluster registry, for the
// reason [ExecuteModeEnv] is not in that file either: the registry is copied, templated
// and committed, and "the checked-in example turned autonomy on" is a failure mode worth
// designing out. The registry describes what to look at; this describes what may be
// changed without asking, and the two do not belong in one blast radius.
const AutonomyRulesEnv = "MAKLAUDE_AUTONOMY_RULES"

// TrustLedgerEnv points at the file the trust ledger keeps its history of recorded
// executions in — the evidence that decides whether a shape has earned autonomy.
//
// It is required whenever [AutonomyRulesEnv] is set, and [New] refuses to start without
// it rather than falling back to an in-memory ledger. An in-memory ledger starts empty
// on every run, so no shape could ever accumulate the three human-approved executions
// promotion needs: the rules would be configured, valid, and silently incapable of ever
// firing. That is the exact failure this milestone's documentation criterion exists to
// prevent — a claim in the docs that is unreachable in the binary.
//
// The file is a CACHE and not the authority. The durable record of a human's approval is
// the approval artifact; the ledger is a local projection of those artifacts, kept so a
// trust decision never depends on an API call that can rate-limit. It can be deleted and
// rebuilt — see [internal/rebuild] — and deleting it revokes every earned shape until the
// history is rebuilt, which is the cheapest revocation there is.
const TrustLedgerEnv = "MAKLAUDE_TRUST_LEDGER"

// ChaosWindowSuffix names the quarantine-window log by appending to the trust ledger's
// own path.
//
// It is derived rather than given its own environment variable, and that is a decision
// about failure modes rather than about ergonomics. The window log is the explanation for
// every gap in the ledger beside it, so the two files belong together on disk and
// unsetting one while setting the other is not a configuration anybody wants. A separate
// knob would allow exactly that: a ledger with no window log, which silently means a
// deliberate fault demotes real shapes. Deriving it means an operator who configured a
// ledger has configured this.
//
// A [Cycle] reports the resolved path ([Cycle.QuarantinePath]) rather than making anybody
// re-derive it, for the same reason `checkpoint.sh --path` exists: two places computing
// one path drift, and the symptom is a report pointing at a file that is not the one being
// written.
const ChaosWindowSuffix = ".chaos-windows"

// New builds the production cycle from the environment.
//
// Under [kube.ExecuteDisabled] — the default — it builds NO approval gate and leaves
// the mutator builder wired but uncalled, because [Cycle.runCluster] returns before
// reaching it. That ordering is the safety property, and it is asserted by
// TestRun_DisabledBuildsNoExecutor rather than left to this comment.
//
// Under either opted-in mode it builds the gate via [approve.GatekeeperFromEnv], which
// is the same construction the escalation trail uses and which refuses outright on the
// two misconfigurations that would otherwise produce a gate that looks like it works:
// an unreadable auto-approve value, and a live trail with no self-identity (so a label
// MaKlaude applied to its own artifact could not be recognized and refused). An error
// here means no cycle was built and nothing ran.
//
// live reports whether the gate reaches a real comms system. A false value with an
// opted-in mode is a legitimate configuration — it rehearses the whole path with an
// in-memory trail nobody can approve on — so it is reported rather than refused.
//
// # Autonomy is opt-in, and every way of half-configuring it is an error
//
// With [AutonomyRulesEnv] unset — the shipped default — a cycle built here auto-applies
// nothing: it has no ruleset, no trust oracle and no disclosure trail, so
// [Cycle.autonomyWired] is false and every proposal takes the human gate. That is
// byte-for-byte Milestone 4's behaviour, and [Cycle.autonomyOff] says so in the report
// rather than leaving an operator to infer it from an absence.
//
// With it set, [Cycle.autonomyFromEnv] requires the other two paths as well — a state
// file for the blast-radius ceiling and a ledger file for the history trust is derived
// from — and refuses to start without them. Each missing piece would otherwise produce a
// deployment where autonomy is configured, valid, and silently incapable of ever firing,
// which is indistinguishable from one where no shape has earned trust yet.
//
// The one exception is the kill switch. Under [kube.ExecuteDisabled] autonomy is not
// wired even when its files are configured, and that is NOT an error: setting
// MAKLAUDE_EXECUTE_MODE=disabled must always be a safe action, and a binary that refused
// to start because of it would turn the kill switch into an outage. The report states the
// posture instead.
func New() (c *Cycle, live bool, err error) {
	mode, err := ExecuteModeFromEnv(os.Getenv)
	if err != nil {
		return nil, false, err
	}

	cyc := &Cycle{
		mode:       mode,
		newClient:  kube.NewClient,
		newMutator: newExecutor,
		trail:      audit.NewTrail(),
		policy:     execute.DefaultPolicy(),
		now:        time.Now,
	}
	if path := strings.TrimSpace(os.Getenv(AutonomyStateEnv)); path != "" {
		// Open never refuses over a corrupt file — it returns a sealed budget that
		// denies everything and says so in the report. The error here is the narrow
		// case of an unusable path, where there is nothing to attach and refusing to
		// start is right: an operator who configured a ceiling and did not get one
		// must not have the run proceed as though they had not configured it.
		b, err := budget.Open(path, budget.DefaultLimits(), time.Now)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", AutonomyStateEnv, err)
		}
		cyc.budget = b
	}
	if mode == kube.ExecuteDisabled {
		cyc.autonomyOff = disabledByKillSwitch(os.Getenv)
		return cyc, false, nil
	}

	gate, live, err := approve.GatekeeperFromEnv(approve.DefaultPolicy())
	if err != nil {
		return nil, false, fmt.Errorf("building the approval gate: %w", err)
	}
	cyc.gate = gate
	cyc.live = live

	if err := cyc.autonomyFromEnv(os.Getenv); err != nil {
		return nil, false, err
	}
	return cyc, live, nil
}

// autonomyFromEnv wires the unattended half from the environment, or records why it is
// off. It returns an error only for a configuration that cannot be honored as written.
//
// # The four things it has to assemble, and why each one is required
//
// [Cycle.UseAutonomy] takes rules, a trust oracle, a disclosure trail and a ledger, and
// [Cycle.autonomyWired] additionally requires the blast-radius budget. This function is
// where an operator's environment becomes those five things, so it is also where every
// partial configuration has to be caught:
//
//   - RULES, from [AutonomyRulesEnv]. Absent means fully gated, which is a posture and
//     not an error.
//   - THE LEDGER, from [TrustLedgerEnv]. Required with rules: an in-memory ledger is
//     empty on every start, so nothing could ever earn autonomy.
//   - THE BUDGET, from [AutonomyStateEnv], opened by [New] before this runs. Required
//     with rules: eligibility with no ceiling is the failure [budget]'s doc opens with,
//     and a breaker that forgets it tripped across a restart is not a breaker.
//   - THE DISCLOSURE TRAIL, from the MAKLAUDE_GITHUB_* variables the escalation trail
//     already uses. It needs no new knob, but it does need to be LIVE: an in-memory trail
//     means an unattended mutation whose only record dies with the process, and that is
//     the one outcome this milestone forbids outright. A non-live trail therefore leaves
//     autonomy unwired rather than failing the run — the gated path is unaffected, and the
//     posture names the variables to set.
//
// The ledger is passed twice, as the oracle and as the recorder, because it is one
// object playing both roles: [trust.Ledger.Trust] answers whether a shape earned
// autonomy, and [trust.Ledger.RecordLifecycle] is how the gated path's approvals get
// into the history that answer is derived from. Wiring them from one file is what makes
// the cold start work — every human approval recorded through the gate is evidence, so a
// deployment that has enabled autonomy and trusts nothing yet is a deployment earning
// trust, not one waiting for a switch.
func (c *Cycle) autonomyFromEnv(getenv func(string) string) error {
	rulesPath := strings.TrimSpace(getenv(AutonomyRulesEnv))
	if rulesPath == "" {
		c.autonomyOff = fmt.Sprintf(
			"no autonomy rules are configured (%s is unset), so every proposal takes the human gate", AutonomyRulesEnv)
		return nil
	}

	rs, err := rules.Load(rulesPath)
	if err != nil {
		return fmt.Errorf("%s: %w", AutonomyRulesEnv, err)
	}

	ledgerPath := strings.TrimSpace(getenv(TrustLedgerEnv))
	if ledgerPath == "" {
		return fmt.Errorf("%s names a rules file (%s) but %s is unset: trust is derived from a durable history of human-approved executions, so an in-memory ledger would start empty on every run and no shape could ever earn autonomy — the rules would be valid and silently unable to fire",
			AutonomyRulesEnv, rulesPath, TrustLedgerEnv)
	}
	if c.budget == nil {
		return fmt.Errorf("%s names a rules file (%s) but %s is unset: an unattended action needs a blast-radius ceiling (per-pass cap, per-target cooldown, per-cluster circuit breaker), and that state has to outlive the process — a breaker that forgets it tripped on restart is not a breaker",
			AutonomyRulesEnv, rulesPath, AutonomyStateEnv)
	}

	ledger, err := trust.Open(ledgerPath)
	if err != nil {
		return fmt.Errorf("%s: %w", TrustLedgerEnv, err)
	}

	windowsPath := ledgerPath + ChaosWindowSuffix
	windows, err := trust.OpenWindows(windowsPath)
	if err != nil {
		return fmt.Errorf("%s: %w", TrustLedgerEnv, err)
	}
	recorder, err := trust.NewQuarantine(ledger, windows)
	if err != nil {
		return fmt.Errorf("%s: %w", TrustLedgerEnv, err)
	}

	trail, live := disclose.TrailFromEnv()
	switch {
	case trail == nil:
		return fmt.Errorf("%s names a rules file (%s) and the disclosure trail could not be built, so an unattended action would have nowhere to be recorded",
			AutonomyRulesEnv, rulesPath)
	case !live:
		c.autonomyOff = fmt.Sprintf(
			"autonomy rules are configured (%s) but the disclosure trail is not live, so an unattended action's only record would die with this process; set MAKLAUDE_GITHUB_REPO and MAKLAUDE_GITHUB_TOKEN, or leave autonomy off",
			rulesPath)
		return nil
	}

	// The ledger is the oracle and the QUARANTINE is the recorder. The asymmetry is the
	// point: the quarantine only ever withholds evidence from the history, so reading
	// trust through the bare ledger is correct and reading through the quarantine would
	// add nothing. See [trust.Quarantine].
	c.UseAutonomy(rs, ledger, trail, recorder)
	c.ledgerPath = ledgerPath
	c.windowsPath = windowsPath
	c.rulesPath = rulesPath
	return nil
}

// disabledByKillSwitch renders the posture for a cycle the kill switch stopped before
// autonomy could be wired.
//
// It reports the two cases differently because they are different situations: an
// operator who has never configured autonomy is running the shipped posture, while one
// who configured it and then set the kill switch has deliberately turned off something
// that is otherwise ready, and needs to see that MaKlaude noticed.
func disabledByKillSwitch(getenv func(string) string) string {
	if strings.TrimSpace(getenv(AutonomyRulesEnv)) == "" {
		return fmt.Sprintf("execution is disabled and no autonomy rules are configured (%s is unset)", AutonomyRulesEnv)
	}
	return fmt.Sprintf("autonomy rules are configured (%s) but %s is disabled, so nothing is applied at all — gated or not",
		strings.TrimSpace(getenv(AutonomyRulesEnv)), ExecuteModeEnv)
}

// NewForTest builds a cycle with every seam supplied explicitly. now may be nil, in
// which case time.Now is used; a zero policy takes the execution layer's defaults.
//
// gate may be nil only when mode is [kube.ExecuteDisabled], matching what [New]
// produces for that posture — a test that passes a gate for a disabled cycle would be
// testing a configuration the binary cannot produce.
func NewForTest(mode kube.ExecuteMode, newClient clientBuilder, newMutator mutatorBuilder,
	gate *approve.Gatekeeper, trail audit.Sink, policy execute.Policy, live bool, now func() time.Time) (*Cycle, error) {

	switch {
	case newClient == nil:
		return nil, fmt.Errorf("operate: a cycle requires a read-only client builder")
	case newMutator == nil:
		return nil, fmt.Errorf("operate: a cycle requires a mutator builder")
	case trail == nil:
		return nil, fmt.Errorf("operate: a cycle requires an audit sink")
	case gate == nil && mode != kube.ExecuteDisabled:
		return nil, fmt.Errorf("operate: an opted-in cycle (%s) requires an approval gate", mode)
	}
	if now == nil {
		now = time.Now
	}
	return &Cycle{
		mode:       mode,
		newClient:  newClient,
		newMutator: newMutator,
		gate:       gate,
		trail:      trail,
		policy:     policy,
		live:       live,
		now:        now,
	}, nil
}

// Mode returns the kill-switch posture this cycle runs under.
func (c *Cycle) Mode() kube.ExecuteMode { return c.mode }

// UseBudget attaches a blast-radius budget to a cycle built by [NewForTest].
//
// It is a setter rather than a constructor parameter for one reason: adding a
// parameter to [NewForTest] would touch every existing call site to pass nil, and a
// wall of nils is how a genuinely-forgotten argument stops being noticeable. It is
// intended to be called once, at construction, before the cycle runs.
//
// A nil budget is the shipped posture and is accepted here as such — see [Cycle.budget]
// for why nothing being bounded means nothing is auto-applied.
func (c *Cycle) UseBudget(b *budget.Budget) { c.budget = b }

// UseAutonomy attaches the four things a cycle needs to act without asking: the
// operator's rules, the trust oracle that says whether a shape earned them, the trail
// every unattended action is disclosed on, and the ledger both halves of the cycle
// record finished executions in — the gated path's approvals are what promote a shape,
// the unattended path's failures are what demote one.
//
// It is one setter rather than four for a reason [Cycle.autonomyWired] then enforces:
// these are not independent knobs. Rules with no oracle grant nothing, an oracle with no
// disclosure would let an action run unrecorded, and either half wired alone is a
// half-configured autonomy that looks configured. Taking them together makes the
// all-or-nothing shape visible at the call site.
//
// ledger may be nil, and that is the one genuine option here: a deployment can run
// autonomy against a trust oracle it does not write back to (a static allowlist under
// test, a ledger owned by another process). A failure then cannot demote the shape —
// the disclosure says so in those words rather than reporting a demotion that did not
// happen — and a gated approval earns nothing in this process, though the lifecycle
// marker on its artifact keeps the evidence recoverable by [internal/rebuild].
//
// It is a setter rather than a [NewForTest] parameter for the reason [Cycle.UseBudget]
// is: four more arguments would touch every existing call site to pass nil, and a wall
// of nils is how a genuinely-forgotten argument stops being noticeable.
func (c *Cycle) UseAutonomy(rules autonomy.Ruleset, oracle autonomy.TrustOracle, trail *disclose.Trail, ledger TrustRecorder) {
	c.rules = rules
	c.oracle = oracle
	c.disclosure = trail
	c.ledger = ledger
}

// Autonomous reports whether this cycle can auto-apply anything at all. It is the
// posture an operator most wants to confirm before believing a quiet report, and it is
// exposed so a caller can state it without reconstructing the four-way condition.
func (c *Cycle) Autonomous() bool { return c.autonomyWired() }

// Budget returns the blast-radius budget this cycle bounds unattended actions with,
// nil when autonomy is not configured. It is exposed so a caller can record an
// execution's outcome against the breaker and render the posture outside a full report.
func (c *Cycle) Budget() *budget.Budget { return c.budget }

// Trail returns the audit sink every attempt was written to. It is exposed so a caller
// can render the lifecycle of a run that has finished; the in-process trail dies with
// the process, which is why the durable record is the approval artifact.
func (c *Cycle) Trail() audit.Sink { return c.trail }

// newExecutor is the production [mutatorBuilder]: a real, write-capable
// [kube.Executor] fixed to one cluster and one mode.
//
// It exists as a named function rather than a closure so the compile-time assertion
// below has something to check, and so the one place in MaKlaude that constructs a
// write-capable client in production is greppable by name.
func newExecutor(h *cluster.Handle, mode kube.ExecuteMode) (execute.Mutator, error) {
	return kube.NewExecutor(h, mode)
}

// The production builder must satisfy the seam. If [kube.NewExecutor]'s signature
// moves, the build fails here rather than at the call site.
var _ mutatorBuilder = newExecutor
