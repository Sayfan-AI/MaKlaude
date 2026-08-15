package devsystem

import (
	"go/build/constraint"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// `task ci` is the pre-push gate every agent and human here runs, and it
// described itself as a mirror of CI while being unable to compile a single
// file in test/e2e: every file there is behind `//go:build e2e`, and both
// `go vet ./...` and `go test ./...` skip build-tagged files silently — as a
// no-op, not as a warning. So the local gate reported green on code that did
// not compile anywhere (issue #184; PR #183 paid a full CI round for it).
//
// The fix is one extra `go vet -tags e2e ./...` line in the vet target. The
// part worth guarding is not that line but the SET it belongs to: `e2e` is the
// only build tag in the tree today, and a second one added later would reopen
// the hole in exactly the same invisible way. Same shape as the turn-budget,
// concurrency-group and e2e-env guards — discover the members from the source
// rather than from a hand-maintained list, so a new member is covered the
// moment it is written.
const taskfilePath = "Taskfile.yml"

// goBuildTagsVetted matches a `-tags a,b` argument in the Taskfile's vet target.
var goBuildTagsVetted = regexp.MustCompile(`go vet\s+-tags[= ]([A-Za-z0-9_,]+)`)

// predeclaredConstraints are build terms the Go toolchain satisfies from the
// build environment rather than from `-tags`: GOOS, GOARCH, the go1.N release
// ladder, and the handful of toolchain-controlled terms. Passing any of these
// to `-tags` would be meaningless, so they are not something the vet target can
// or should cover. Anything NOT on this list is a custom tag whose files are
// invisible to an untagged vet, which is the defect this guard exists for.
var predeclaredConstraints = map[string]bool{
	// GOOS
	"aix": true, "android": true, "darwin": true, "dragonfly": true,
	"freebsd": true, "hurd": true, "illumos": true, "ios": true, "js": true,
	"linux": true, "nacl": true, "netbsd": true, "openbsd": true, "plan9": true,
	"solaris": true, "wasip1": true, "windows": true, "zos": true,
	// GOARCH
	"386": true, "amd64": true, "amd64p32": true, "arm": true, "arm64": true,
	"arm64be": true, "armbe": true, "loong64": true, "mips": true, "mips64": true,
	"mips64le": true, "mips64p32": true, "mips64p32le": true, "mipsle": true,
	"ppc": true, "ppc64": true, "ppc64le": true, "riscv": true, "riscv64": true,
	"s390": true, "s390x": true, "sparc": true, "sparc64": true, "wasm": true,
	// Toolchain-controlled
	"cgo": true, "gc": true, "gccgo": true, "race": true, "msan": true,
	"asan": true, "boringcrypto": true, "unix": true,
}

// repoRoot() is shared with gitignore_test.go in this package.

// buildTagsInTree walks every Go source file in the repository and returns the
// set of custom build tags constraining them, mapped to one example file each
// so a failure names something a reader can open.
func buildTagsInTree(t *testing.T) map[string]string {
	t.Helper()

	tags := map[string]string{}
	root := repoRoot()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Vendored and VCS trees are not ours to vet.
			if name := d.Name(); name == ".git" || name == "vendor" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			// Constraints must precede the package clause; stop there so a
			// `//go:build`-looking string inside the body can't confuse this.
			if strings.HasPrefix(line, "package ") {
				break
			}
			if !constraint.IsGoBuild(line) {
				continue
			}
			expr, perr := constraint.Parse(line)
			if perr != nil {
				t.Errorf("%s: unparseable build constraint %q: %v", rel, line, perr)
				continue
			}
			for tag := range constraintTags(expr) {
				if predeclaredConstraints[tag] || strings.HasPrefix(tag, "go1.") {
					continue
				}
				if _, seen := tags[tag]; !seen {
					tags[tag] = rel
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return tags
}

// constraintTags collects the tag identifiers appearing anywhere in a parsed
// build expression, including under negation — a file behind `!foo` is still a
// file whose compilation depends on whether `foo` is set.
func constraintTags(e constraint.Expr) map[string]bool {
	out := map[string]bool{}
	var walk func(constraint.Expr)
	walk = func(e constraint.Expr) {
		switch x := e.(type) {
		case *constraint.TagExpr:
			out[x.Tag] = true
		case *constraint.NotExpr:
			walk(x.X)
		case *constraint.AndExpr:
			walk(x.X)
			walk(x.Y)
		case *constraint.OrExpr:
			walk(x.X)
			walk(x.Y)
		}
	}
	walk(e)
	return out
}

func readTaskfile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(), taskfilePath))
	if err != nil {
		t.Fatalf("reading %s: %v", taskfilePath, err)
	}
	return string(b)
}

// taskTarget returns the body of a single target in the Taskfile: everything
// from `  <name>:` up to the next target at the same indentation.
func taskTarget(t *testing.T, name string) string {
	t.Helper()
	tf := readTaskfile(t)

	marker := "\n  " + name + ":\n"
	idx := strings.Index(tf, marker)
	if idx < 0 {
		t.Fatalf("%s has no %q target; this guard cannot locate it, and a renamed target "+
			"would silently disable the check rather than fail it", taskfilePath, name)
	}
	body := tf[idx+len(marker):]
	// Targets are indented two spaces; the next one at that depth ends this body.
	if end := regexp.MustCompile(`\n  [A-Za-z][A-Za-z0-9:_-]*:\n`).FindStringIndex(body); end != nil {
		body = body[:end[0]]
	}
	return body
}

// TestVetCoversEveryBuildTag is the guard the fix exists for. Every custom build
// tag in the tree must have a matching tagged vet pass, so no package can be
// invisible to the gate that claims to mirror CI.
func TestVetCoversEveryBuildTag(t *testing.T) {
	vet := taskTarget(t, "vet")

	vetted := map[string]bool{}
	for _, m := range goBuildTagsVetted.FindAllStringSubmatch(vet, -1) {
		for _, tag := range strings.Split(m[1], ",") {
			if tag = strings.TrimSpace(tag); tag != "" {
				vetted[tag] = true
			}
		}
	}

	found := buildTagsInTree(t)
	if len(found) == 0 {
		t.Fatal("found no custom build tags anywhere in the tree; test/e2e is supposed to carry " +
			"`//go:build e2e`, so this guard has stopped discovering its own subject matter")
	}

	for tag, example := range found {
		if !vetted[tag] {
			t.Errorf("build tag %q constrains %s, but the `vet` target never runs `go vet -tags %s ./...`; "+
				"that package can fail to compile while `task ci` reports green (issue #184). "+
				"Add `- go vet -tags %s ./...` to the vet target.", tag, example, tag, tag)
		}
	}
}

// TestVetTargetStillRunsUntaggedPass keeps the fix from being "simplified" into
// only the tagged pass. `-tags e2e` widens the set of files vetted, it does not
// replace it: a file behind `//go:build !e2e` would drop out.
func TestVetTargetStillRunsUntaggedPass(t *testing.T) {
	vet := taskTarget(t, "vet")
	if !strings.Contains(vet, "go vet ./...") {
		t.Error("the `vet` target no longer runs a plain `go vet ./...`; the tagged pass is an " +
			"addition to it, not a replacement for it")
	}
}

// TestCIGateRunsVet closes the other half. A vet target nobody invokes is the
// defect, not the fix — `task ci` is what humans and agents actually run before
// pushing, and CI's own Vet step shells out to the same target.
func TestCIGateRunsVet(t *testing.T) {
	ci := taskTarget(t, "ci")
	if !strings.Contains(ci, "task: vet") {
		t.Error("the `ci` target no longer runs `vet`; the pre-push gate would stop type-checking " +
			"build-tagged packages entirely")
	}

	wf := filepath.Join(repoRoot(), ".github", "workflows", "ci.yml")
	b, err := os.ReadFile(wf)
	if err != nil {
		t.Fatalf("reading %s: %v", wf, err)
	}
	if !strings.Contains(string(b), "task vet") {
		t.Error("ci.yml no longer runs `task vet`; the tagged compile check would only run in the " +
			"`e2e on kind` job, which is the slow feedback loop issue #184 was filed about")
	}
}

// TestTaggedVetIsNotRedundant records WHY the extra line is there by execution
// rather than by comment. It holds only while the untagged pass genuinely
// cannot see the e2e suite — i.e. while every file there is build-tagged. If
// that stops being true the tagged pass may no longer be load-bearing, and this
// failing is the prompt to check rather than a bug in itself.
func TestTaggedVetIsNotRedundant(t *testing.T) {
	dir := filepath.Join(repoRoot(), "test", "e2e")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	files := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		files++
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		tagged := false
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "package ") {
				break
			}
			if constraint.IsGoBuild(line) {
				tagged = true
				break
			}
		}
		if !tagged {
			t.Errorf("test/e2e/%s carries no build constraint, so a plain `go vet ./...` now compiles "+
				"part of this package; re-check whether the tagged vet pass is still the thing "+
				"catching e2e compile errors", e.Name())
		}
	}
	if files == 0 {
		t.Fatal("test/e2e contains no Go files; this guard has lost its subject matter")
	}
}
