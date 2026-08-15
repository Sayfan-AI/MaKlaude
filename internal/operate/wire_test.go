package operate

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// These tests are about the CONFIGURATION SURFACE: the four files and variables an
// operator sets to enable autonomy, and every way of setting some but not all of them.
//
// The half-configured cases are the point. Autonomy that is configured, valid, and
// silently unable to fire is indistinguishable from autonomy that is on and has not
// earned anything yet — both produce a report with no unattended actions in it — so each
// one either refuses to start or says in words that it is off. A test that only covered
// the happy path would leave exactly the failure this task exists to prevent: a claim in
// the docs that is unreachable in the binary.

const rulesFixture = `version: 1
rules:
  - name: staging-web-restart
    clusters: [staging]
    namespaces: [web]
    operations: [rolloutrestart]
`

// autonomyEnv writes a rules file and returns a getenv function over the four autonomy
// variables. The GitHub half is set through the process environment by liveDisclosure,
// because disclose.TrailFromEnv reads os.Getenv directly.
func autonomyEnv(t *testing.T, rules, ledger, state string) func(string) string {
	t.Helper()
	return func(key string) string {
		switch key {
		case AutonomyRulesEnv:
			return rules
		case TrustLedgerEnv:
			return ledger
		case AutonomyStateEnv:
			return state
		default:
			return ""
		}
	}
}

// writeRules puts the fixture on disk and returns its path.
func writeRules(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "autonomy.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the rules fixture: %v", err)
	}
	return path
}

// liveDisclosure configures the MAKLAUDE_GITHUB_* variables so the disclosure trail
// reports itself live. Nothing here reaches the network: the sink is constructed eagerly
// and only makes a request when something is disclosed.
func liveDisclosure(t *testing.T) {
	t.Helper()
	t.Setenv("MAKLAUDE_GITHUB_REPO", "example/infra")
	t.Setenv("MAKLAUDE_GITHUB_TOKEN", "test-token")
	t.Setenv("MAKLAUDE_GITHUB_SELF_LOGIN", "maklaude-bot")
}

// wiredCycle is a cycle with every autonomy input configured, built through the real
// environment path.
func wiredCycle(t *testing.T) *Cycle {
	t.Helper()
	liveDisclosure(t)
	c := &Cycle{mode: kube.ExecuteEnabled}
	c.budget = memoryBudget()
	getenv := autonomyEnv(t, writeRules(t, rulesFixture), filepath.Join(t.TempDir(), "trust.jsonl"), "state")
	if err := c.autonomyFromEnv(getenv); err != nil {
		t.Fatalf("autonomyFromEnv: %v", err)
	}
	return c
}

func TestAutonomyFromEnv_UnsetRulesIsThePosture(t *testing.T) {
	c := &Cycle{mode: kube.ExecuteEnabled}
	if err := c.autonomyFromEnv(autonomyEnv(t, "", "", "")); err != nil {
		t.Fatalf("an unconfigured deployment is the shipped posture, not an error: %v", err)
	}
	if c.Autonomous() {
		t.Fatal("no rules were configured, so nothing may be auto-applied")
	}
	if !strings.Contains(c.autonomyOff, AutonomyRulesEnv) {
		t.Errorf("the posture must name the variable that turns autonomy on, got %q", c.autonomyOff)
	}
}

func TestAutonomyFromEnv_Wires(t *testing.T) {
	c := wiredCycle(t)
	if !c.Autonomous() {
		t.Fatal("rules, ledger, state and a live disclosure trail were all configured, so autonomy must be in force")
	}
	if c.autonomyOff != "" {
		t.Errorf("a wired cycle must report no reason for being off, got %q", c.autonomyOff)
	}
	if len(c.rules) != 1 {
		t.Errorf("got %d rule(s), want the 1 in the fixture", len(c.rules))
	}
	if c.oracle == nil || c.ledger == nil {
		t.Error("the ledger is both the trust oracle and the recorder; one file plays both roles")
	}
}

// TestAutonomyFromEnv_ColdStartIsTrustedNothing is the day-one behaviour the docs
// promise: autonomy is fully configured, the ledger is empty, and therefore nothing is
// trusted. It is asserted because "nothing is trusted on day one, by design" is a claim
// about the shipped binary, and an empty ledger that somehow trusted a shape would make
// the word "earned" a lie.
func TestAutonomyFromEnv_ColdStartIsTrustedNothing(t *testing.T) {
	c := wiredCycle(t)
	evidence := c.oracle.Trust(autonomy.Subject{
		Shape:       autonomy.Shape{Cluster: "staging", Operation: remediate.OpRolloutRestart},
		Fingerprint: "fp1:anything",
	})
	if evidence.Trusted {
		t.Fatalf("a fresh ledger must trust nothing, got %+v", evidence)
	}
	// The zero evidence, not a citation explaining the absence: an untrusted shape must
	// carry nothing a caller could mistake for a reason to act. The operator-facing
	// explanation is [trust.Ledger.Explain], which is a different reader.
	if evidence.Citation != "" {
		t.Errorf("an untrusted shape must cite nothing, got %q", evidence.Citation)
	}
}

func TestAutonomyFromEnv_Refuses(t *testing.T) {
	rules := writeRules(t, rulesFixture)
	ledger := filepath.Join(t.TempDir(), "trust.jsonl")

	tests := []struct {
		name string
		// budget reports whether a blast-radius state file was opened before this ran,
		// which is what New does with MAKLAUDE_AUTONOMY_STATE.
		budget bool
		rules  string
		ledger string
		state  string
		// want is the variable or phrase the error must name, because the message is
		// where the operator's next action comes from.
		want string
	}{{
		name:   "rules without a ledger",
		budget: true,
		rules:  rules,
		state:  "state",
		want:   TrustLedgerEnv,
	}, {
		name:   "rules without a blast-radius state file",
		budget: false,
		rules:  rules,
		ledger: ledger,
		want:   AutonomyStateEnv,
	}, {
		name:   "a rules file that does not exist",
		budget: true,
		rules:  filepath.Join(t.TempDir(), "absent.yaml"),
		ledger: ledger,
		state:  "state",
		want:   "does not exist",
	}, {
		name:   "a rules file that grants nothing",
		budget: true,
		rules:  writeRules(t, "version: 1\nrules: []\n"),
		ledger: ledger,
		state:  "state",
		want:   "rules is empty",
	}, {
		name:   "a rules file with an invalid grant",
		budget: true,
		rules:  writeRules(t, "version: 1\nrules:\n  - name: r\n    clusters: [staging]\n    operations: [rolloutrestart]\n"),
		ledger: ledger,
		state:  "state",
		want:   "namespaces is empty",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			liveDisclosure(t)
			c := &Cycle{mode: kube.ExecuteEnabled}
			if tt.budget {
				c.budget = memoryBudget()
			}
			err := c.autonomyFromEnv(autonomyEnv(t, tt.rules, tt.ledger, tt.state))
			if err == nil {
				t.Fatalf("this configuration must be refused; autonomous=%v", c.Autonomous())
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("the error does not say what to fix\n got %v\nwant a message containing %q", err, tt.want)
			}
			if c.Autonomous() {
				t.Error("a refused configuration must leave autonomy unwired")
			}
		})
	}
}

// TestAutonomyFromEnv_NonLiveDisclosureLeavesAutonomyOff is the one required piece whose
// absence is reported rather than refused. An unattended mutation whose only record dies
// with the process is what this milestone forbids outright — so autonomy does not engage
// — but the gated path is unaffected and must keep running, so it is not a startup error.
func TestAutonomyFromEnv_NonLiveDisclosureLeavesAutonomyOff(t *testing.T) {
	t.Setenv("MAKLAUDE_GITHUB_REPO", "")
	t.Setenv("MAKLAUDE_GITHUB_TOKEN", "")

	c := &Cycle{mode: kube.ExecuteEnabled}
	c.budget = memoryBudget()
	getenv := autonomyEnv(t, writeRules(t, rulesFixture), filepath.Join(t.TempDir(), "trust.jsonl"), "state")

	if err := c.autonomyFromEnv(getenv); err != nil {
		t.Fatalf("an unreachable disclosure trail must not stop the gated path: %v", err)
	}
	if c.Autonomous() {
		t.Fatal("autonomy must not engage when an unattended action could not be durably recorded")
	}
	if !strings.Contains(c.autonomyOff, "disclosure trail is not live") {
		t.Errorf("the posture must say the disclosure trail is the missing piece, got %q", c.autonomyOff)
	}
	if !strings.Contains(c.autonomyOff, "MAKLAUDE_GITHUB_REPO") {
		t.Errorf("the posture must name the variables to set, got %q", c.autonomyOff)
	}
}

// TestNew_KillSwitchNeverRefusesToStart pins the one exception to "half-configured
// autonomy is an error". Setting the kill switch to disabled must always be a safe
// action; a binary that refused to start because autonomy files were still configured
// would turn the kill switch into an outage.
func TestNew_KillSwitchNeverRefusesToStart(t *testing.T) {
	t.Setenv(ExecuteModeEnv, "disabled")
	t.Setenv(AutonomyRulesEnv, writeRules(t, rulesFixture))
	t.Setenv(TrustLedgerEnv, "")
	t.Setenv(AutonomyStateEnv, "")

	c, live, err := New()
	if err != nil {
		t.Fatalf("the kill switch must not be an error, even with autonomy configured: %v", err)
	}
	if live {
		t.Error("a disabled cycle builds no gate, so nothing is live")
	}
	if c.Autonomous() {
		t.Fatal("autonomy must not be wired under the kill switch")
	}
	if !strings.Contains(c.autonomyOff, ExecuteModeEnv) {
		t.Errorf("the posture must say the kill switch is why nothing runs, got %q", c.autonomyOff)
	}
}

// TestNew_UnsetIsMilestoneFoursPosture is the regression that matters most: the shipped
// default must not have changed. No autonomy variables set means no rules, no oracle, no
// disclosure trail — every proposal takes the human gate exactly as before this task.
func TestNew_UnsetIsMilestoneFoursPosture(t *testing.T) {
	t.Setenv(ExecuteModeEnv, "")
	t.Setenv(AutonomyRulesEnv, "")
	t.Setenv(TrustLedgerEnv, "")
	t.Setenv(AutonomyStateEnv, "")

	c, _, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	switch {
	case c.Autonomous():
		t.Error("the shipped posture must auto-apply nothing")
	case len(c.rules) != 0:
		t.Error("the shipped posture must load no rules")
	case c.oracle != nil:
		t.Error("the shipped posture must wire no trust oracle")
	case c.disclosure != nil:
		t.Error("the shipped posture must build no disclosure trail")
	case c.ledger != nil:
		t.Error("the shipped posture must wire no trust ledger")
	}
}

// TestNew_RulesWithoutALedgerRefusesToStart carries one refusal through the real New,
// rather than only through autonomyFromEnv, so the wiring order is covered too: a
// misconfiguration discovered after the gate is built must still fail the run.
func TestNew_RulesWithoutALedgerRefusesToStart(t *testing.T) {
	liveDisclosure(t)
	t.Setenv(ExecuteModeEnv, "enabled")
	t.Setenv(AutonomyRulesEnv, writeRules(t, rulesFixture))
	t.Setenv(TrustLedgerEnv, "")
	t.Setenv(AutonomyStateEnv, filepath.Join(t.TempDir(), "budget.json"))

	c, _, err := New()
	if err == nil {
		t.Fatalf("rules with no ledger must refuse to start; autonomous=%v", c.Autonomous())
	}
	if !strings.Contains(err.Error(), TrustLedgerEnv) {
		t.Errorf("the error must name the variable to set, got %v", err)
	}
	if c != nil {
		t.Error("a refused configuration must return no cycle")
	}
}

// --- The posture in the report. ----------------------------------------------------

// TestPosture_OffStatesTheCause is the invisible-nothing-happened case for this section.
// A report with no unattended actions looks identical whether autonomy is off or simply
// had nothing to do, so the reason is printed rather than left to be inferred.
func TestPosture_OffStatesTheCause(t *testing.T) {
	c := &Cycle{mode: kube.ExecuteEnabled}
	if err := c.autonomyFromEnv(autonomyEnv(t, "", "", "")); err != nil {
		t.Fatalf("autonomyFromEnv: %v", err)
	}
	out := renderAutonomy(t, autonomyReport(nil, c.posture()))

	if !strings.Contains(out, "Unattended actions: OFF") {
		t.Fatalf("the report must state the unattended posture:\n%s", out)
	}
	if !strings.Contains(out, AutonomyRulesEnv) {
		t.Errorf("the report must name the variable that turns autonomy on:\n%s", out)
	}
}

func TestPosture_OnNamesTheFilesItRead(t *testing.T) {
	c := wiredCycle(t)
	out := renderAutonomy(t, autonomyReport(c.budget, c.posture()))

	for _, want := range []string{
		"Unattended actions: ON",
		"1 autonomy rule(s)",
		c.rulesPath,
		c.ledgerPath,
		"EARNED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the ON posture must contain %q so an operator can check it:\n%s", want, out)
		}
	}
}

// TestPosture_DerivesTheCauseWhenNobodySetOne covers the cycles New did not build:
// NewForTest, and anything wiring UseAutonomy directly. They record no explanation, and
// an unexplained "OFF" with no cause is the rendering this section must not have.
func TestPosture_DerivesTheCauseWhenNobodySetOne(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Cycle)
		want string
	}{{
		name: "no rules",
		mut:  func(*Cycle) {},
		want: "no autonomy rules are loaded",
	}, {
		name: "rules but no ledger",
		mut: func(c *Cycle) {
			c.rules = permissiveRuleset()
		},
		want: "no trust ledger is wired",
	}, {
		name: "rules and oracle but no ceiling",
		mut: func(c *Cycle) {
			c.rules, c.oracle = permissiveRuleset(), autonomy.StaticTrust{}
		},
		want: "no blast-radius ceiling is wired",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Cycle{mode: kube.ExecuteEnabled}
			tt.mut(c)
			p := c.posture()
			if p.autonomous {
				t.Fatal("this cycle cannot auto-apply anything")
			}
			if !strings.Contains(p.off, tt.want) {
				t.Fatalf("posture reason\n got %q\nwant one containing %q", p.off, tt.want)
			}
		})
	}
}

// renderAutonomy renders just the autonomy section of a report.
func renderAutonomy(t *testing.T, a AutonomyReport) string {
	t.Helper()
	report := &Report{GeneratedAt: fixedTime, Mode: kube.ExecuteEnabled.String(), Autonomy: a}
	var buf bytes.Buffer
	if err := report.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return buf.String()
}
