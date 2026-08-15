package trust

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
)

// ledgerPath returns a path inside the test's own temporary directory, including one
// directory level that does not exist yet — Open is expected to create it, because
// the first run on a fresh install is the case where the parent is missing.
func ledgerPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state", "trust.jsonl")
}

// openLedger opens a ledger and fails the test if it cannot.
func openLedger(t *testing.T, path string) *Ledger {
	t.Helper()
	l, err := Open(path)
	if err != nil {
		t.Fatalf("opening ledger %s: %v", path, err)
	}
	return l
}

// The restart-survival criterion, stated as the thing it protects: a shape that
// earned autonomy before a restart still has it afterwards, and a shape that was
// demoted before a restart is still demoted.
func TestLedgerSurvivesAProcessRestart(t *testing.T) {
	path := ledgerPath(t)

	first := openLedger(t, path)
	for i := 0; i < PromotionThreshold; i++ {
		if err := first.Record(entryAt(t, shape, 'h', i)); err != nil {
			t.Fatalf("recording: %v", err)
		}
	}
	before := first.Trust(subject)
	if !before.Trusted {
		t.Fatalf("precondition failed: %s", first.Explain(subject))
	}

	// A second Open against the same path is what a restart looks like from here.
	second := openLedger(t, path)
	after := second.Trust(subject)

	if after != before {
		t.Fatalf("the verdict did not survive a restart:\nbefore: %+v\nafter:  %+v", before, after)
	}
	if got, want := second.Len(), PromotionThreshold; got != want {
		t.Errorf("Len after restart = %d, want %d", got, want)
	}

	// And the demotion survives too, which is the direction that matters more.
	if err := second.Record(entryAt(t, shape, 'f', PromotionThreshold)); err != nil {
		t.Fatalf("recording the failure: %v", err)
	}
	third := openLedger(t, path)
	if ev := third.Trust(subject); ev.Trusted {
		t.Fatalf("a recorded failure did not survive the restart: %+v", ev)
	}
}

// A ledger file that does not exist yet is an empty history, not an error: it is
// what every fresh install has.
func TestOpeningAMissingLedgerIsAnEmptyHistory(t *testing.T) {
	l := openLedger(t, ledgerPath(t))

	if l.Len() != 0 {
		t.Errorf("Len = %d, want 0", l.Len())
	}
	if ev := l.Trust(subject); ev.Trusted {
		t.Fatalf("a fresh install trusted something: %+v", ev)
	}
}

// A file that cannot be parsed must fail loudly. An empty ledger would be the
// safe-LOOKING failure and the wrong one: it is indistinguishable from a fresh
// install, so it would silently discard history on every start for as long as
// nobody noticed.
func TestACorruptLedgerFailsToOpen(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "truncated line from a crash mid-append",
			content: `{"key":"a","cluster":"prod","operation":"rollout`,
			want:    "line 1",
		},
		{
			name:    "an outcome token this build does not know",
			content: `{"key":"a","cluster":"prod","operation":"rolloutrestart","authority":"human","outcome":"probably-fine","at":"2026-07-01T09:00:00Z","ref":"x"}`,
			want:    "unknown outcome",
		},
		{
			name:    "an authority token this build does not know",
			content: `{"key":"a","cluster":"prod","operation":"rolloutrestart","authority":"vibes","outcome":"converged","at":"2026-07-01T09:00:00Z","ref":"x"}`,
			want:    "unknown authority",
		},
		{
			name:    "an entry that would not validate",
			content: `{"key":"a","cluster":"prod","operation":"rolloutrestart","authority":"human","outcome":"converged","at":"2026-07-01T09:00:00Z"}`,
			want:    "approval artifact",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := ledgerPath(t)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("preparing directory: %v", err)
			}
			if err := os.WriteFile(path, []byte(tc.content+"\n"), 0o600); err != nil {
				t.Fatalf("writing fixture: %v", err)
			}

			l, err := Open(path)
			if err == nil {
				t.Fatalf("a corrupt ledger opened cleanly with %d entries", l.Len())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// The rebuild path: the ledger is a cache of the approval artifacts, so replacing it
// wholesale from those artifacts is a supported operation and must be atomic.
func TestRebuildReplacesTheWholeHistory(t *testing.T) {
	path := ledgerPath(t)
	l := openLedger(t, path)

	// A history that has earned autonomy...
	for i := 0; i < PromotionThreshold; i++ {
		if err := l.Record(entryAt(t, shape, 'h', i)); err != nil {
			t.Fatalf("recording: %v", err)
		}
	}
	if !l.Trust(subject).Trusted {
		t.Fatalf("precondition failed: %s", l.Explain(subject))
	}

	// ...and a rebuild that finds the artifacts only support one approval.
	if err := l.Rebuild([]Entry{entryAt(t, shape, 'h', 0)}); err != nil {
		t.Fatalf("rebuilding: %v", err)
	}

	if got := l.Len(); got != 1 {
		t.Errorf("Len after rebuild = %d, want 1", got)
	}
	if ev := l.Trust(subject); ev.Trusted {
		t.Fatalf("trust survived a rebuild that removed its evidence: %+v", ev)
	}

	// And it survives the restart, which is what makes it a rebuild rather than an
	// in-memory opinion.
	if got := openLedger(t, path).Len(); got != 1 {
		t.Errorf("Len after rebuild and restart = %d, want 1", got)
	}
}

// The property that makes "cache, not authority" true rather than aspirational:
// replaying the artifacts produces exactly the ledger the live append path did, so
// the file can always be thrown away.
func TestRebuildFromTheArtifactsReproducesTheLiveLedger(t *testing.T) {
	spelling := "hhpihfhhhhh"

	live := openLedger(t, ledgerPath(t))
	var artifacts []Entry
	for i, r := range spelling {
		e := entryAt(t, shape, r, i)
		if err := live.Record(e); err != nil {
			t.Fatalf("recording: %v", err)
		}
		artifacts = append(artifacts, e)
	}

	// The artifacts come back from the API in an arbitrary order, and with the
	// overlap a paged read produces.
	shuffled := []Entry{artifacts[4], artifacts[0], artifacts[4]}
	for i := len(artifacts) - 1; i >= 0; i-- {
		shuffled = append(shuffled, artifacts[i])
	}

	rebuilt := openLedger(t, ledgerPath(t))
	if err := rebuilt.Rebuild(shuffled); err != nil {
		t.Fatalf("rebuilding: %v", err)
	}

	if got, want := rebuilt.Trust(subject), live.Trust(subject); got != want {
		t.Fatalf("a rebuild changed the verdict:\nlive:    %+v\nrebuilt: %+v", want, got)
	}
	if got, want := rebuilt.Standing(subject), live.Standing(subject); got != want {
		t.Fatalf("a rebuild changed the standing:\nlive:    %+v\nrebuilt: %+v", want, got)
	}
	if got, want := len(rebuilt.Entries()), len(live.Entries()); got != want {
		t.Fatalf("a rebuild changed the entry count: %d, want %d", got, want)
	}
	for i, got := range rebuilt.Entries() {
		if want := live.Entries()[i]; got != want {
			t.Errorf("entry %d differs after rebuild:\ngot:  %+v\nwant: %+v", i, got, want)
		}
	}
}

// A rebuild that would store an invalid entry must leave the existing ledger alone
// rather than half-replacing it. This is the recovery path, and a recovery that can
// corrupt the thing it is recovering is not one.
func TestARejectedRebuildLeavesTheLedgerIntact(t *testing.T) {
	path := ledgerPath(t)
	l := openLedger(t, path)
	for i := 0; i < PromotionThreshold; i++ {
		if err := l.Record(entryAt(t, shape, 'h', i)); err != nil {
			t.Fatalf("recording: %v", err)
		}
	}

	bad := entryAt(t, shape, 'h', 99)
	bad.Ref = ""
	err := l.Rebuild([]Entry{entryAt(t, shape, 'h', 0), bad})
	if err == nil {
		t.Fatal("a rebuild containing an invalid entry succeeded")
	}

	if got := l.Len(); got != PromotionThreshold {
		t.Errorf("Len after a rejected rebuild = %d, want %d", got, PromotionThreshold)
	}
	if !l.Trust(subject).Trusted {
		t.Errorf("a rejected rebuild demoted the shape: %s", l.Explain(subject))
	}
	if got := openLedger(t, path).Len(); got != PromotionThreshold {
		t.Errorf("Len on disk after a rejected rebuild = %d, want %d", got, PromotionThreshold)
	}
}

// The same execution written to the file twice is history recorded twice, not two
// executions, and must not double-count toward promotion after a restart.
func TestDuplicateKeysOnDiskCollapse(t *testing.T) {
	path := ledgerPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("preparing directory: %v", err)
	}

	line, err := marshal(entryAt(t, shape, 'h', 0))
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	var content []byte
	for i := 0; i < PromotionThreshold+2; i++ {
		content = append(content, line...)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	l := openLedger(t, path)
	if got := l.Len(); got != 1 {
		t.Errorf("Len = %d, want 1", got)
	}
	if ev := l.Trust(subject); ev.Trusted {
		t.Fatalf("one execution written %d times bought trust: %+v", PromotionThreshold+2, ev)
	}
}

// Blank lines are tolerated. A file an operator has looked at with an editor is the
// normal case, and a trailing newline is not corruption.
func TestBlankLinesAreTolerated(t *testing.T) {
	path := ledgerPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("preparing directory: %v", err)
	}

	line, err := marshal(entryAt(t, shape, 'h', 0))
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	content := append([]byte("\n\n"), line...)
	content = append(content, '\n')
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	if got := openLedger(t, path).Len(); got != 1 {
		t.Errorf("Len = %d, want 1", got)
	}
}

// The file records who approved what, so it is not world-readable, and a rebuild
// must not widen the mode by writing through a fresh temporary file.
func TestLedgerFileIsNotWorldReadable(t *testing.T) {
	path := ledgerPath(t)
	l := openLedger(t, path)
	if err := l.Record(entryAt(t, shape, 'h', 0)); err != nil {
		t.Fatalf("recording: %v", err)
	}

	assertMode := func(when string) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", when, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("mode %s = %04o, want 0600", when, got)
		}
	}
	assertMode("after append")

	if err := l.Rebuild([]Entry{entryAt(t, shape, 'h', 0)}); err != nil {
		t.Fatalf("rebuilding: %v", err)
	}
	assertMode("after rebuild")
}

// A rebuild leaves no temporary files behind, which matters because they would sit
// next to the ledger holding the same approval history under a mode nobody checked.
func TestRebuildLeavesNoTemporaryFiles(t *testing.T) {
	path := ledgerPath(t)
	l := openLedger(t, path)
	if err := l.Rebuild([]Entry{entryAt(t, shape, 'h', 0)}); err != nil {
		t.Fatalf("rebuilding: %v", err)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("reading directory: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(path) {
			t.Errorf("a stray file survived the rebuild: %s", entry.Name())
		}
	}
}

// A failed append must not leave the in-memory ledger believing something no restart
// will reproduce. The unwritable directory is the stand-in for a full or read-only
// filesystem.
func TestAFailedAppendDoesNotGrantTrustThisProcessCannotProve(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory does not deny root a write")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "trust.jsonl")
	l := openLedger(t, path)

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("making the directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := l.Record(entryAt(t, shape, 'h', 0))
	if err == nil {
		t.Fatal("an append to an unwritable ledger reported success")
	}
	if l.Len() != 0 {
		t.Errorf("Len = %d, want 0: a failed append was kept in memory", l.Len())
	}
}

// Open needs a path. An empty one is a configuration mistake and reads as such,
// rather than silently becoming a file called "" in the working directory.
func TestOpenRequiresAPath(t *testing.T) {
	if _, err := Open("  "); err == nil {
		t.Fatal("Open accepted a blank path")
	}
}

// Path is how an escalation tells an operator which file to look at.
func TestPathReportsTheBackingFile(t *testing.T) {
	path := ledgerPath(t)
	if got := openLedger(t, path).Path(); got != path {
		t.Errorf("Path = %q, want %q", got, path)
	}
	if got := NewMemory().Path(); got != "" {
		t.Errorf("in-memory Path = %q, want empty", got)
	}
}

// The wire format is what a future build reads, so a round trip has to be exact —
// including the instant, which JSON is perfectly capable of rounding.
func TestWireFormatRoundTripsExactly(t *testing.T) {
	cases := []Entry{
		entryAt(t, shape, 'h', 0),
		entryAt(t, shape, 'p', 1),
		entryAt(t, other, 'd', 2),
		{
			Key:       "sub-second",
			Shape:     shape,
			Authority: audit.AuthorityHuman,
			Outcome:   OutcomeInconclusive,
			At:        time.Date(2026, 7, 1, 9, 0, 0, 123456789, time.UTC),
			Ref:       "https://example.invalid/1",
		},
	}

	for _, want := range cases {
		line, err := marshal(want)
		if err != nil {
			t.Fatalf("marshalling %s: %v", want.Key, err)
		}
		got, err := unmarshal(line)
		if err != nil {
			t.Fatalf("unmarshalling %s: %v", want.Key, err)
		}
		if !got.At.Equal(want.At) {
			t.Errorf("%s: At = %s, want %s", want.Key, got.At, want.At)
		}
		got.At, want.At = time.Time{}, time.Time{}
		if got != want {
			t.Errorf("%s round-tripped differently:\ngot:  %+v\nwant: %+v", want.Key, got, want)
		}
	}
}

// The authority tokens the file stores are the audit package's own, so a rename
// there is caught here rather than by a ledger that silently stops parsing.
func TestAuthorityTokensMatchTheAuditPackage(t *testing.T) {
	for _, a := range []audit.Authority{
		audit.AuthorityUnattributed, audit.AuthorityHuman, audit.AuthorityPolicy,
	} {
		got, ok := parseAuthority(a.String())
		if !ok {
			t.Errorf("audit.Authority %q does not parse", a)
			continue
		}
		if got != a {
			t.Errorf("%q parsed as %q", a, got)
		}
	}
	if _, ok := parseAuthority("authority(99)"); ok {
		t.Error("the fallback rendering of an unknown authority parses as a real one")
	}
}
