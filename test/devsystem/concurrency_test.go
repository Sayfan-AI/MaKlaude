// Concurrency-serialization tests for the genesis agent workflows.
//
// Why these exist: the repo-mutating agents (orchestrator, events, ci-failure,
// evolver) all commit, push branches, open PRs and comment on issues. Running
// two of them at once is the "two orchestrators duplicating work" case the
// shared concurrency group was introduced to prevent — but the group is opt-in
// per workflow, and the evolver was simply never added to it. Nothing failed
// loudly: two Claude sessions just ran in parallel on this repo, every day, for
// as long as the sampled history goes back (15/15 days; on 2026-07-27 the two
// runs started 2 seconds apart).
//
// The root cause was a *cron collision*: GitHub dispatches all of a repo's
// crons sharing one expression in the same batch, and then delays them by the
// same amount, so `0 6 * * *` (evolver) and `0 */6 * * *` (orchestrator) landed
// together to the second. Both properties are mechanical, so both get a
// deterministic test rather than a comment nobody re-reads:
//
//  1. every Claude-invoking workflow declares a concurrency group, and the
//     mutually-exclusive ones declare the *same* group;
//  2. no two genesis workflows fire on the same cron minute.
package devsystem

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// sharedGroup serializes every repo-mutating agent. The literal must match the
// workflows verbatim, expression included — two workflows only share a group if
// the rendered strings are identical.
const sharedGroup = "genesis-orchestrator-${{ github.repository }}"

// ownGroup marks a workflow that is deliberately NOT serialized against the
// agents. genesis-merge.yml is the only one: it runs a narrow fixed procedure
// (check a green bot PR, squash, close) and must not sit behind a multi-minute
// agent run to do it.
const ownGroup = "<deliberately separate>"

// wantGroup is the agreed concurrency group per Claude-invoking workflow.
// Adding a Claude-invoking workflow without classifying it here fails the test:
// an unclassified agent workflow is exactly how the evolver ended up running
// unserialized.
var wantGroup = map[string]string{
	"genesis-orchestrator.yml": sharedGroup, // scheduled tick
	"genesis-events.yml":       sharedGroup, // same agent, event-triggered
	"genesis-ci-failure.yml":   sharedGroup, // CI triage; also pushes fixes
	"genesis-evolver.yml":      sharedGroup, // commits, opens PRs — same hazard
	"genesis-merge.yml":        ownGroup,    // narrow scope — see ownGroup note
}

// mapValue returns the value node for key in a YAML mapping node.
//
// It matches on the key node's raw scalar text on purpose: GitHub's `on:` key
// is resolved to a boolean by some YAML schemas, and the raw text is stable
// under all of them.
func mapValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// parseWorkflow returns the root mapping node of a workflow file.
func parseWorkflow(t *testing.T, name string) *yaml.Node {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(workflowDir(t), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	if len(doc.Content) == 0 {
		t.Fatalf("%s: empty document", name)
	}
	return doc.Content[0]
}

// TestAgentWorkflowsDeclareSerializationGroup is the guard: no repo-mutating
// agent runs unserialized against the others.
func TestAgentWorkflowsDeclareSerializationGroup(t *testing.T) {
	for name := range claudeWorkflows(t) {
		want, classified := wantGroup[name]
		if !classified {
			t.Errorf("%s invokes %s but has no entry in wantGroup — classify it (sharedGroup or ownGroup) so it cannot run in parallel with another agent by accident", name, claudeActionMarker)
			continue
		}

		conc := mapValue(parseWorkflow(t, name), "concurrency")
		if conc == nil {
			t.Errorf("%s declares no `concurrency:` block — it can run in parallel with another agent session on the same repo", name)
			continue
		}
		group := mapValue(conc, "group")
		if group == nil {
			t.Errorf("%s has a `concurrency:` block with no `group:`", name)
			continue
		}
		if want == ownGroup {
			if group.Value == sharedGroup {
				t.Errorf("%s is classified as deliberately separate but joined the shared group %q — a narrow fixed-procedure job should not queue behind multi-minute agent runs", name, sharedGroup)
			}
			continue
		}
		if group.Value != want {
			t.Errorf("%s concurrency group is %q, want %q — a group name that differs by even one character is a different mutex, so the two workflows would still run in parallel", name, group.Value, want)
		}
	}
}

// TestAgentWorkflowsDoNotCancelEachOther keeps the shared group from being
// turned into a cancelling one. `cancel-in-progress: true` there would let a
// scheduled tick kill an event run mid-commit, which is worse than queueing.
func TestAgentWorkflowsDoNotCancelEachOther(t *testing.T) {
	for name, want := range wantGroup {
		if want != sharedGroup {
			continue
		}
		conc := mapValue(parseWorkflow(t, name), "concurrency")
		if conc == nil {
			continue // already reported by the test above
		}
		cancel := mapValue(conc, "cancel-in-progress")
		if cancel == nil {
			t.Errorf("%s does not set `cancel-in-progress` — state it explicitly (false) so the intent survives the next edit", name)
			continue
		}
		if cancel.Value != "false" {
			t.Errorf("%s sets cancel-in-progress: %s in the shared group — a new tick would abort an agent mid-commit, leaving a half-pushed branch and no diagnosis", name, cancel.Value)
		}
	}
}

// cronFirings expands the minute and hour fields of a 5-field cron into the set
// of "HH:MM" times it fires at each day.
//
// It deliberately supports only the forms actually used here (`N`, `*`, `*/K`,
// comma lists, `A-B`) and fails loudly on anything else, including a non-`*`
// day/month/weekday field. A cron form this cannot expand must not silently
// pass the collision check.
func cronFirings(t *testing.T, wf, expr string) map[string]bool {
	t.Helper()
	f := strings.Fields(expr)
	if len(f) != 5 {
		t.Fatalf("%s: cron %q does not have 5 fields", wf, expr)
	}
	for i, name := range []string{"day-of-month", "month", "day-of-week"} {
		if f[i+2] != "*" {
			t.Fatalf("%s: cron %q restricts %s to %q; cronFirings only models daily schedules, so extend it rather than letting the collision check pass vacuously", wf, expr, name, f[i+2])
		}
	}

	expand := func(field string, upper int) []int {
		var out []int
		for _, part := range strings.Split(field, ",") {
			step := 1
			if base, s, found := strings.Cut(part, "/"); found {
				k, err := strconv.Atoi(s)
				if err != nil || k <= 0 {
					t.Fatalf("%s: cron %q has bad step %q", wf, expr, s)
				}
				step, part = k, base
			}
			lo, hi := 0, upper
			if part != "*" {
				a, b, isRange := strings.Cut(part, "-")
				var err error
				if lo, err = strconv.Atoi(a); err != nil {
					t.Fatalf("%s: cron %q has bad value %q", wf, expr, a)
				}
				hi = lo
				if isRange {
					if hi, err = strconv.Atoi(b); err != nil {
						t.Fatalf("%s: cron %q has bad range end %q", wf, expr, b)
					}
				}
			}
			for v := lo; v <= hi && v <= upper; v += step {
				out = append(out, v)
			}
		}
		return out
	}

	firings := map[string]bool{}
	for _, h := range expand(f[1], 23) {
		for _, m := range expand(f[0], 59) {
			firings[fmt.Sprintf("%02d:%02d", h, m)] = true
		}
	}
	if len(firings) == 0 {
		t.Fatalf("%s: cron %q expands to no firing times", wf, expr)
	}
	return firings
}

// workflowCrons returns every cron expression declared by a workflow.
func workflowCrons(t *testing.T, name string) []string {
	t.Helper()
	sched := mapValue(mapValue(parseWorkflow(t, name), "on"), "schedule")
	if sched == nil || sched.Kind != yaml.SequenceNode {
		return nil
	}
	var out []string
	for _, entry := range sched.Content {
		if c := mapValue(entry, "cron"); c != nil {
			out = append(out, c.Value)
		}
	}
	return out
}

// TestGenesisCronsDoNotCollide is the root-cause guard. Two genesis workflows
// sharing a firing minute are dispatched in the same batch and delayed
// identically, so they start together — which is how the evolver and the
// orchestrator came to run in parallel every day despite each looking fine on
// its own. Serialization now catches that, but a collision still means one
// agent's daily cycle spends its time queued, and can be superseded while
// pending. Keep the schedules apart.
func TestGenesisCronsDoNotCollide(t *testing.T) {
	entries, err := os.ReadDir(workflowDir(t))
	if err != nil {
		t.Fatalf("read workflow dir: %v", err)
	}

	type sched struct {
		wf      string
		firings map[string]bool
	}
	var scheds []sched
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "genesis-") {
			continue
		}
		for _, expr := range workflowCrons(t, name) {
			scheds = append(scheds, sched{name, cronFirings(t, name, expr)})
		}
	}
	if len(scheds) < 2 {
		t.Fatalf("found %d genesis cron schedules — expected at least the evolver and the scheduled orchestrator, so this test is no longer guarding anything", len(scheds))
	}

	for i := range scheds {
		for j := i + 1; j < len(scheds); j++ {
			if scheds[i].wf == scheds[j].wf {
				continue
			}
			var shared []string
			for at := range scheds[i].firings {
				if scheds[j].firings[at] {
					shared = append(shared, at)
				}
			}
			if len(shared) > 0 {
				sort.Strings(shared)
				t.Errorf("%s and %s both fire at %s UTC — GitHub dispatches same-minute crons together and delays them equally, so these two agents start seconds apart every day; stagger one of them", scheds[i].wf, scheds[j].wf, strings.Join(shared, ", "))
			}
		}
	}
}
