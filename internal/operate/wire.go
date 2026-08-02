package operate

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/budget"
	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/execute"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
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
		return cyc, false, nil
	}

	gate, live, err := approve.GatekeeperFromEnv(approve.DefaultPolicy())
	if err != nil {
		return nil, false, fmt.Errorf("building the approval gate: %w", err)
	}
	cyc.gate = gate
	cyc.live = live
	return cyc, live, nil
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
