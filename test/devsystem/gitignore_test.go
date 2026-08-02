// Guard on the .gitignore entry for genesis serve's local runtime state.
//
// Why this exists: the "genesis local control plane runtime state" section of
// .gitignore was an *enumeration* of four filenames. `genesis serve` then began
// writing a fifth, `.genesis/.trigger-state`, and nothing ignored it — so every
// local-mode run left an untracked file in the working tree. That is quiet but
// not harmless: the agents in this repo read `git status` to decide what a run
// has changed, and `git add -A` in any commit path would sweep per-machine
// runtime state into the repo.
//
// Nothing failed. `main` stayed green, CI never saw the file (a fresh checkout
// has no runtime state), and the only symptom was one line of `??` output that
// every run had to look past. Same shape as the concurrency group and the turn
// budget floors: a property that holds only for the members someone remembered
// to list, where the next member silently defaults to unsafe.
//
// So the fix is a pattern and the test pins the pattern rather than the names.
// The negative cases matter at least as much as the positive ones — an
// over-broad pattern that swallowed .genesis/config.toml would hide real
// dev-system configuration from the repo, which is a worse failure than the one
// being fixed.
package devsystem

import (
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
)

// repoRoot is where `git check-ignore` must run: gitignore patterns are
// resolved relative to the repository, not to this package's directory.
func repoRoot() string { return filepath.Join("..", "..") }

// isIgnored asks git itself whether path is matched by an ignore rule.
// Reparsing .gitignore in Go would test a reimplementation of the matcher, not
// the matcher; git's exit status is the real thing.
//
// The path need not exist — check-ignore is a pattern match, so this covers the
// runtime files a fresh CI checkout has never written.
//
// `--no-index` is load-bearing and was added after the negative control below
// was found to be vacuous without it. By default check-ignore consults the
// index and reports every *tracked* path as un-ignored, because gitignore
// genuinely has no effect on tracked files. That is true and useless here:
// every path TestTrackedGenesisFilesAreNotIgnored asks about is tracked, so the
// default would answer "not ignored" no matter how over-broad the pattern got —
// a check that cannot fail. --no-index measures the pattern instead of the
// index, which is the property under test, and it is also the question that
// matters in practice: the next *new* file added under .genesis/scripts/ is not
// yet tracked, and an over-broad rule would silently swallow it.
func isIgnored(t *testing.T, path string) bool {
	t.Helper()
	cmd := exec.Command("git", "check-ignore", "-q", "--no-index", "--", path)
	cmd.Dir = repoRoot()
	err := cmd.Run()
	if err == nil {
		return true // exit 0: ignored
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false // exit 1: not ignored
	}
	t.Fatalf("git check-ignore %s: %v", path, err)
	return false
}

// TestGenesisRuntimeStateIsIgnoredByPattern is the load-bearing case: a runtime
// file this repo has never seen must already be ignored. Reverting .gitignore
// to the old four-name enumeration fails here and nowhere else, which is the
// point — the enumeration passes every test written about the names it lists.
func TestGenesisRuntimeStateIsIgnoredByPattern(t *testing.T) {
	// Neither of these exists on disk, and that is deliberate: the guard has to
	// hold for the *next* file genesis serve invents, not just today's set.
	for _, name := range []string{
		".genesis/.some-runtime-file-genesis-has-not-invented-yet",
		".genesis/.another-one",
	} {
		if !isIgnored(t, name) {
			t.Errorf("%s is not ignored: .gitignore enumerates genesis runtime "+
				"state instead of matching the class, so the next file genesis "+
				"serve writes will dirty the working tree the same way "+
				".trigger-state did", name)
		}
	}
}

// TestKnownGenesisRuntimeFilesAreIgnored is the regression half: every runtime
// file observed in the wild, including the one the enumeration missed.
func TestKnownGenesisRuntimeFilesAreIgnored(t *testing.T) {
	for _, name := range []string{
		".genesis/.disabled-by-genesis", // written when genesis disables the workflows
		".genesis/.orchestrator.lock",   // local-mode serialization
		".genesis/.poll-etag",           // event poll cursor
		".genesis/.poll-highwater",      // event poll cursor
		".genesis/.trigger-state",       // the one the enumeration missed
	} {
		if !isIgnored(t, name) {
			t.Errorf("%s is not ignored: genesis serve writes it on every local run", name)
		}
	}
}

// TestTrackedGenesisFilesAreNotIgnored is the negative control, and it is the
// reason the pattern is `.genesis/.*` rather than `.genesis/*`. A pattern is
// only as good as what it declines to match: silently ignoring the dev system's
// own configuration and scripts would be a worse outcome than the untracked
// file this change removes.
func TestTrackedGenesisFilesAreNotIgnored(t *testing.T) {
	for _, name := range []string{
		".genesis/config.toml",
		".genesis/onboarding.md",
		".genesis/scripts/issues.sh",
		".genesis/scripts/escalate.sh",
		".genesis/design/agent-turn-budgets.md",
	} {
		if isIgnored(t, name) {
			t.Errorf("%s is ignored but is tracked dev-system content, not runtime state", name)
		}
	}
}
