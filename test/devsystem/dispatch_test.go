package devsystem

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Why a test and not a review note: a bare `gh workflow run` in a load-bearing
// re-trigger step is invisible until a transient 5xx from api.github.com fails
// the job, trips escalate.sh, and manufactures a `needs:human` gate no human can
// act on (#88 from a 502, cleaned up in #91). The fix is one shared script with
// retries; this test is what stops the bare form from being reintroduced by the
// next person who just needs "a quick dispatch here".

// dispatchScript is the only sanctioned way to fire a workflow_dispatch.
const dispatchScript = ".genesis/scripts/dispatch.sh"

// bareDispatchRE matches a `gh workflow run` that is NOT routed through
// dispatch.sh. The script itself is excluded by filename, not by pattern.
var bareDispatchRE = regexp.MustCompile(`gh\s+workflow\s+run`)

// TestNoBareWorkflowDispatch asserts every self-dispatch goes through the
// retrying wrapper.
func TestNoBareWorkflowDispatch(t *testing.T) {
	dir := workflowDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		checked++

		for i, line := range strings.Split(string(b), "\n") {
			if !bareDispatchRE.MatchString(line) {
				continue
			}
			if strings.Contains(line, dispatchScript) {
				continue
			}
			t.Errorf("%s:%d dispatches a workflow directly (%q) — route it through %s so a transient 5xx retries instead of failing the job and manufacturing a needs:human escalation (#91)",
				name, i+1, strings.TrimSpace(line), dispatchScript)
		}
	}
	if checked == 0 {
		t.Fatalf("no workflow files found under %s — layout changed, so this test guards nothing", dir)
	}
}

// TestDispatchCallersCheckOutTheScript catches the other half of the mistake:
// referencing dispatch.sh from a job that never checked the repo out, which
// fails with "No such file or directory" on every run rather than transiently.
func TestDispatchCallersCheckOutTheScript(t *testing.T) {
	dir := workflowDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(b)
		if !strings.Contains(body, dispatchScript) {
			continue
		}
		if !strings.Contains(body, "actions/checkout") {
			t.Errorf("%s calls %s but has no actions/checkout step — the script would not exist in the workspace", name, dispatchScript)
		}
	}
}

// TestDispatchScriptRetries pins the two properties the callers depend on: the
// script is executable-shaped bash with retry logic, and it fails (rather than
// silently succeeding) once attempts are exhausted. A future "simplification"
// down to a single attempt would reintroduce #91 exactly.
func TestDispatchScriptRetries(t *testing.T) {
	path := filepath.Join("..", "..", dispatchScript)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", dispatchScript, err)
	}
	body := string(b)

	for _, want := range []string{
		"DISPATCH_ATTEMPTS", // retry count is configurable
		"sleep",             // attempts are spaced, not hammered
		"exit 1",            // exhausted retries fail the job (chosen semantics)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s no longer contains %q — retry-then-fail semantics are what keep a transient 502 from inventing a human gate (#91)", dispatchScript, want)
		}
	}
	if !strings.Contains(body, "--ref") {
		t.Errorf("%s must pin --ref; without it `gh` needs repo metadata an actions-only app token lacks", dispatchScript)
	}
}
