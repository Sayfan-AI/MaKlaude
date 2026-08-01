package devsystem

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A required env var read by an e2e test but never produced by the e2e workflow
// is invisible until the job runs: the Go test hard-fails (correctly — a missing
// kubeconfig must not silently skip a safety proof), but nothing catches it at
// review time, and the whole kind cluster is spun up before anyone finds out.
//
// That is exactly how PR #122 failed: it added test/e2e/executor_test.go, which
// requires MAKLAUDE_E2E_EXECUTOR_KUBECONFIG, while the workflow only ever minted
// the read-only one. Two of the milestone's load-bearing dry-run proofs died on
// a missing variable, 83 seconds into a job that had already built a cluster.
//
// This is the same shape as the turn-budget and concurrency-group guards: the
// thing to test is the SET — every required variable, discovered from the tests
// themselves rather than from a hand-maintained list, so a newly-added one is
// covered the moment it is written.
const e2eWorkflow = ".github/workflows/e2e.yml"

// requiredEnvCall matches the e2e suite's `env(t, "NAME")` helper, which
// t.Fatalf's on an empty value. Optional variables are read with os.Getenv
// directly (MAKLAUDE_E2E_AUDIT_LOG is the one such case) and are deliberately
// NOT matched here — an optional corroboration that degrades gracefully is not
// something the workflow must supply.
var requiredEnvCall = regexp.MustCompile(`\benv\(t,\s*"([A-Z0-9_]+)"\)`)

// requiredE2EEnvVars scans the e2e test sources for variables the suite treats
// as mandatory.
func requiredE2EEnvVars(t *testing.T) []string {
	t.Helper()

	dir := filepath.Join("..", "e2e")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	seen := map[string]bool{}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		for _, m := range requiredEnvCall.FindAllStringSubmatch(string(b), -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				names = append(names, m[1])
			}
		}
	}

	if len(names) == 0 {
		t.Fatal("found no required env vars in test/e2e; the env(t, ...) helper was probably renamed, " +
			"which would silently disable this guard")
	}
	return names
}

func readE2EWorkflow(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", e2eWorkflow))
	if err != nil {
		t.Fatalf("reading %s: %v", e2eWorkflow, err)
	}
	return string(b)
}

// TestE2EWorkflowPassesEveryRequiredEnvVar asserts the step that runs the suite
// forwards each mandatory variable into the test process.
func TestE2EWorkflowPassesEveryRequiredEnvVar(t *testing.T) {
	wf := readE2EWorkflow(t)

	marker := "run: task e2e"
	idx := strings.Index(wf, marker)
	if idx < 0 {
		t.Fatalf("%s no longer contains a %q step; this guard cannot locate the run step", e2eWorkflow, marker)
	}
	// The env: block precedes `run:` within the same step, so scan back to the
	// step boundary rather than forward.
	step := wf[:idx]
	if last := strings.LastIndex(step, "\n      - name:"); last >= 0 {
		step = step[last:]
	}

	for _, name := range requiredE2EEnvVars(t) {
		if !strings.Contains(step, name+":") {
			t.Errorf("e2e test requires %s but the `task e2e` step does not pass it; "+
				"the job will spin up a cluster and then hard-fail on the missing variable", name)
		}
	}
}

// TestE2EWorkflowProducesEveryRequiredEnvVar is the half that actually caught
// #122's failure mode. Forwarding `NAME: ${{ env.NAME }}` is not enough on its
// own: if no earlier step ever wrote NAME to $GITHUB_ENV, the expression expands
// to the empty string and the test fails exactly as if nothing were passed. So
// the value must be produced somewhere, not merely referenced.
func TestE2EWorkflowProducesEveryRequiredEnvVar(t *testing.T) {
	wf := readE2EWorkflow(t)

	for _, name := range requiredE2EEnvVars(t) {
		produced := false
		for _, line := range strings.Split(wf, "\n") {
			if strings.Contains(line, name+"=") && strings.Contains(line, "GITHUB_ENV") {
				produced = true
				break
			}
		}
		if !produced {
			t.Errorf("e2e test requires %s but no step writes it to $GITHUB_ENV; "+
				"the ${{ env.%s }} reference would expand to an empty string", name, name)
		}
	}
}
