package chaos

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// The teardown guarantee, executed rather than asserted.
//
// Every other test in this package proves something about a request MaKlaude composes.
// This one proves something about MaKlaude NOT EXISTING, which is the condition the
// milestone's worst outcome actually happens in: a process killed between injecting a
// fault and giving it back. `defer` does not run on SIGKILL, on an OOM kill, or on a CI
// runner that disappears — so a test that stubs "the process died" by returning early
// from a function would be testing the wrong thing, because a returning function still
// runs its defers.
//
// So the injector runs in a REAL child process, against the parent's stub API server,
// and the parent SIGKILLs it once the create has landed. What survives that is exactly
// what would survive it in production, and there are two things to prove about the
// aftermath:
//
//  1. The fault ends on its own. The child never tore anything down — the parent
//     asserts zero DELETEs ever reached the API server — and the object it left behind
//     carries a bounded server-side duration, which Chaos Mesh's controller honours with
//     no help from MaKlaude. That is [SelfLimitServerDuration], and it is the only
//     mechanism in this system whose enforcing party is not this program.
//  2. The object is collected on the next cycle. The parent then runs a fresh [Reaper] —
//     standing in for the next scheduled run, which is the only thing a dead process's
//     residue can be cleaned up by — and it removes the orphan the child left.
//
// The child is a re-invocation of this test binary, guarded by an environment variable
// so it is inert under a normal `go test` run.

// childEnvVar makes the helper test below run as the injecting child. Without it the
// helper skips, so `go test ./...` does not spawn anything.
const childEnvVar = "MAKLAUDE_CHAOS_KILL_CHILD_STUB_URL"

// childInjectedMarker is what the child prints once its create has been accepted. The
// parent waits for this rather than for a sleep: killing before the create lands would
// prove nothing about what a killed process leaves behind, and a fixed sleep would make
// the test slow when it works and flaky when the machine is loaded.
const childInjectedMarker = "CHAOS-CHILD-INJECTED"

// TestKilledInjector_LeavesAFaultThatExpiresAndAnObjectTheReaperCollects is the
// executed form of Milestone 6's load-bearing property.
func TestKilledInjector_LeavesAFaultThatExpiresAndAnObjectTheReaperCollects(t *testing.T) {
	if os.Getenv(childEnvVar) != "" {
		t.Skip("this process IS the child; the helper test does the injecting")
	}

	stub := newChaosStub(t)

	// The child injects a PERSISTING fault on purpose. A one-shot action would make the
	// test pass for the wrong reason: its fault is over before the kill either way, so it
	// says nothing about whether a fault that outlives the create can outlive the process.
	child := exec.Command(os.Args[0], "-test.run=^TestHelperInjectsAndHangs$", "-test.v")
	child.Env = append(os.Environ(), childEnvVar+"="+stub.URL)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatalf("wiring the child's stdout: %v", err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatalf("starting the child injector: %v", err)
	}

	// Kill the child no matter how this test exits, so a failed assertion does not leave
	// a hung process behind.
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})

	injected := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if line := scanner.Text(); strings.Contains(line, childInjectedMarker) {
				injected <- line
				return
			}
		}
		close(injected)
	}()

	select {
	case line, ok := <-injected:
		if !ok {
			t.Fatal("the child exited without injecting; its stderr is above")
		}
		t.Logf("child reported: %s", line)
	case <-time.After(60 * time.Second):
		t.Fatal("the child did not inject within 60s")
	}

	// SIGKILL. Not Signal(os.Interrupt), not a cancelled context: the whole point is a
	// death the child cannot observe, so no deferred teardown can possibly run.
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("killing the child: %v", err)
	}
	_ = child.Wait()

	// --- Property 1: the fault MaKlaude left behind expires without MaKlaude. ---

	create := stub.only(t, "POST")
	if got := stub.requestsFor("DELETE"); len(got) != 0 {
		t.Fatalf("a SIGKILLed process cannot have torn anything down, yet the stub saw %+v", got)
	}

	spec, ok := create.Body["spec"].(map[string]any)
	if !ok {
		t.Fatalf("the create carried no spec: %+v", create.Body)
	}
	rawDuration, ok := spec["duration"].(string)
	if !ok {
		t.Fatalf("the orphaned object carries no spec.duration, so nothing but MaKlaude would end its fault: %+v", spec)
	}
	duration, err := time.ParseDuration(rawDuration)
	if err != nil {
		t.Fatalf("spec.duration %q is not a duration Chaos Mesh can parse: %v", rawDuration, err)
	}
	if duration <= 0 || duration > MaxDuration() {
		t.Errorf("spec.duration = %s, want a positive value no greater than the %s ceiling", duration, MaxDuration())
	}

	// --- Property 2: the next cycle collects the object the dead process left. ---

	orphan := orphanFromCreate(t, create)
	// Aged past the grace, which is what the passage of time between the dead run and the
	// next scheduled one does in production.
	orphan.created = fixedNow.Add(-DefaultOrphanGrace - time.Minute)
	reaper := newReaperAgainst(t, stub, kube.ExecuteEnabled, orphan)

	sweep, err := reaper.Reap(context.Background(), chaosNamespace)
	if err != nil {
		t.Fatalf("the sweep after a killed run must succeed, got: %v", err)
	}
	if len(sweep.Reaped) != 1 || len(sweep.Skipped) != 0 {
		t.Fatalf("sweep = %+v, want the dead run's orphan removed", sweep)
	}
	if sweep.Reaped[0].Name != orphan.name {
		t.Errorf("reaped %q, want the object the child created (%q)", sweep.Reaped[0].Name, orphan.name)
	}

	// The reaper recognised the object from the child's own request rather than from a
	// hand-written fixture, which is the part that would silently rot: if the injector's
	// labels, cluster annotation or name derivation changed, this assertion fails instead
	// of the reaper quietly ceasing to recognise MaKlaude's own experiments.
	del := stub.only(t, "DELETE")
	if !strings.HasSuffix(del.Path, "/"+orphan.name) {
		t.Errorf("delete path %q does not address the orphan %q", del.Path, orphan.name)
	}
}

// orphanFromCreate turns the child's recorded create into the object a later LIST would
// return for it — same name, labels and annotations, straight off the wire.
func orphanFromCreate(t *testing.T, create recordedRequest) stubObject {
	t.Helper()
	meta, ok := create.Body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("the create carried no metadata: %+v", create.Body)
	}
	name, _ := meta["name"].(string)
	if name == "" {
		t.Fatalf("the create carried no object name: %+v", meta)
	}

	toStringMap := func(key string) map[string]string {
		out := map[string]string{}
		raw, _ := meta[key].(map[string]any)
		for k, v := range raw {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
		return out
	}

	return stubObject{
		name:        name,
		uid:         "uid-1", // what the stub's canned create response returned.
		labels:      toStringMap("labels"),
		annotations: toStringMap("annotations"),
	}
}

// TestHelperInjectsAndHangs is the child. It injects a persisting fault against the
// stub whose URL it is handed, announces that the create landed, and then blocks
// forever waiting to be killed.
//
// It deliberately has NO teardown — no defer, no cleanup, no signal handler. Adding one
// would not change the outcome under SIGKILL, but it would make a reader think the
// guarantee depends on it, and the guarantee is precisely that it does not.
func TestHelperInjectsAndHangs(t *testing.T) {
	stubURL := os.Getenv(childEnvVar)
	if stubURL == "" {
		t.Skip("not the child process")
	}

	injector, err := NewInjector(eligibleTarget(t, stubURL), kube.ExecuteEnabled)
	if err != nil {
		t.Fatalf("child: building injector: %v", err)
	}

	experiment := Experiment{
		Action:    ActionPodFailure,
		Namespace: chaosNamespace,
		Mode:      ModeOne,
		Selector: Selector{
			Namespaces:     []string{"demo"},
			LabelSelectors: map[string]string{"app": "web"},
		},
		Duration: 2 * time.Minute,
	}
	if experiment.SelfLimit() != SelfLimitServerDuration {
		t.Fatalf("child: this test needs a fault that persists, got %s", experiment.SelfLimit())
	}

	in, err := injector.Inject(context.Background(), experiment)
	if err != nil {
		t.Fatalf("child: injecting: %v", err)
	}
	if in.DryRun {
		t.Fatal("child: the fault must be real, or nothing survives to be leaked")
	}

	// Written straight to stdout rather than through t.Logf. The parent blocks on this
	// line, and testing's log output is buffered per-test outside chatty mode — a
	// handshake that only arrives when the test finishes is not a handshake for a test
	// whose whole job is never to finish.
	fmt.Fprintf(os.Stdout, "%s name=%s uid=%s\n", childInjectedMarker, in.Name, in.UID)

	// Block until killed. A long sleep rather than an infinite loop so a stray child
	// cannot spin a CPU forever if the parent dies first.
	time.Sleep(10 * time.Minute)
	t.Fatal("child: was never killed, so the parent proved nothing")
}

// TestKillTestHelperIsInertWithoutItsEnvVar guards the harness itself. A helper test
// that ran on its own during `go test ./...` would spawn nothing and prove nothing, but
// a helper that FAILED on its own would look like a broken package — and a helper that
// blocked would hang the suite. The skip is the contract.
func TestKillTestHelperIsInertWithoutItsEnvVar(t *testing.T) {
	if got := os.Getenv(childEnvVar); got != "" {
		t.Fatalf("%s leaked into the parent's environment (%q); the child guard is not a guard", childEnvVar, got)
	}
}
