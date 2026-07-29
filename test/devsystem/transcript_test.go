package devsystem

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// Why these tests: six max-turns deaths produced six fixes, and each recovered a
// different human-facing output from the dying run — gate staleness before the
// agent step (#84), landed artifacts after it (#97), intent at the front of it
// (#102), the failure's class in the escalation (#104). None of them recover the
// MIDDLE of the run: which approaches the agent tried, which dead ends it burned
// turns on, where the turns went. Every budget decision so far was made from a
// run log holding `init` + `result` and zero tool calls, and #104's whole finding
// was that the evidence feeding those decisions was the wrong evidence (#106).
//
// The transcript is not missing, it is discarded: claude-code-action writes every
// SDK message to $RUNNER_TEMP/claude-execution-output.json on BOTH its success and
// error paths, and the runner is then torn down. So the property to guard is that
// every Claude-invoking workflow copies it to a private sink — membership, not one
// instance, the rule the turn-budget floors and the concurrency group each learned
// by failing as opt-in properties. A new runner must fail this test rather than
// silently defaulting to no retention.
//
// The second property is the one that made this hard rather than cheap: this repo
// is PUBLIC. Run logs and artifacts are world-readable and Actions masks only
// registered secrets, so retention must add nothing world-readable — which is a
// claim worth testing by execution, not by review.

const retainScript = ".genesis/scripts/retain-transcript.sh"

var (
	retainInvocationRE = regexp.MustCompile(`retain-transcript\.sh`)
	ifAlwaysRE         = regexp.MustCompile(`if:\s*\$?\{?\{?\s*always\(\)`)
	showFullOutputRE   = regexp.MustCompile(`show_full_output\s*:\s*(?i:true|"true"|'true')`)
	retainPathRE       = regexp.MustCompile(`retain-transcript\.sh"?\s+--path`)
)

func retainPathFor(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", retainScript)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("stat %s: %v", retainScript, err)
	}
	return p
}

// retainWithinLines is how far above the retain-transcript.sh call an
// `if: always()` may sit and still be its guard: a step is `if:` + `name:` +
// `run:` with a comment block above it, so a small window rejects an unrelated
// `always()` elsewhere in the file while tolerating either step-key order.
const retainWithinLines = 6

// TestEveryClaudeWorkflowRetainsItsTranscript is the membership guard.
//
// Scope is every workflow that spends an agent turn budget, with no exemptions:
// unlike the intent checkpoint (which a narrow fixed-procedure runner can skip
// because its intent IS its prompt), a transcript is exactly as diagnostic for
// genesis-merge.yml as for the orchestrator — "where did the turns go" is a
// question about execution, not about scope.
func TestEveryClaudeWorkflowRetainsItsTranscript(t *testing.T) {
	checked := 0
	for name, body := range claudeWorkflows(t) {
		checked++
		lines := strings.Split(body, "\n")

		claudeAt, retainAt, escalateAt := -1, -1, -1
		for i, line := range lines {
			// Comments name these scripts on purpose (the rationale for the step
			// order is the most valuable thing in the file to keep); position is
			// a property of the executable lines only.
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			switch {
			case claudeAt < 0 && strings.Contains(line, claudeActionMarker):
				claudeAt = i
			case retainAt < 0 && retainInvocationRE.MatchString(line):
				retainAt = i
			case escalateAt < 0 && escalateRE.MatchString(line):
				escalateAt = i
			}
		}

		if retainAt < 0 {
			t.Errorf("%s runs %s but never invokes %s — this runner's transcript dies with the runner, so a max-turns death there is diagnosed from `init`+`result` and zero tool calls, which is what #106 exists to end",
				name, claudeActionMarker, retainScript)
			continue
		}

		// After the agent step: the execution file does not exist until the SDK
		// has written it.
		if retainAt < claudeAt {
			t.Errorf("%s invokes %s at line %d, BEFORE the agent step at line %d — the execution file does not exist yet at that point, so retention would silently find nothing every run",
				name, retainScript, retainAt+1, claudeAt+1)
		}

		// Before the escalation: escalate.sh renders the retention status, so a
		// retention step ordered after it reports "unknown" on exactly the runs
		// that matter.
		if escalateAt >= 0 && retainAt > escalateAt {
			t.Errorf("%s invokes %s at line %d, AFTER %s at line %d — the escalation renders the retention status, so this ordering makes every escalation say the transcript is unknown",
				name, retainScript, retainAt+1, escalateScript, escalateAt+1)
		}

		// `always()`, not `failure()`: a green run's transcript is the baseline a
		// dying one gets read against, and `failure()` would also skip the
		// cancelled case.
		guarded := false
		for i := max(0, retainAt-retainWithinLines); i < retainAt; i++ {
			if ifAlwaysRE.MatchString(lines[i]) {
				guarded = true
				break
			}
		}
		if !guarded {
			t.Errorf("%s invokes %s without an `if: always()` within %d lines above it — a step that defaults to skipping on failure retains transcripts only for the runs nobody needs one for",
				name, retainScript, retainWithinLines)
		}
	}

	if checked == 0 {
		t.Fatalf("no workflow invokes %s — layout changed, so this test guards nothing", claudeActionMarker)
	}
}

// TestTranscriptRetentionAddsNothingWorldReadable pins the constraint that made
// #106 a design problem rather than a one-line change.
//
// The repo is public. `show_full_output: true` would stream tool RESULTS into
// the run log, and an uploaded artifact is equally world-readable; Actions masks
// only registered secrets, and a miss is permanent and public. The retention path
// must therefore be the private sink and nothing else — so both cheap-but-unsafe
// shortcuts are blocked here rather than left to reviewer memory.
func TestTranscriptRetentionAddsNothingWorldReadable(t *testing.T) {
	for name, body := range claudeWorkflows(t) {
		for i, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue // the comments explain why it stays off; keep them
			}
			if showFullOutputRE.MatchString(line) {
				t.Errorf("%s:%d enables show_full_output — that streams tool results into a world-readable run log on a PUBLIC repo, which is the exposure #106 chose the private Loki sink to avoid",
					name, i+1)
			}
			if strings.Contains(line, "upload-artifact") {
				t.Errorf("%s:%d uploads an artifact from an agent-running workflow — artifacts are world-readable on this public repo, so a transcript must not travel that way (#106)",
					name, i+1)
			}
		}
	}

	src := readFileString(t, retainPathFor(t))
	if !strings.Contains(src, "/loki/api/v1/push") {
		t.Errorf("%s no longer pushes to Loki — the private sink IS the safety argument; any other destination needs the exposure question answered again (#106)", retainScript)
	}
}

// TestTranscriptStatusReachesTheEscalation: a transcript retained where nobody is
// told to look is worse than none, because it looks like the gap is closed. The
// escalation is the one thing a human reads after a death, so it must carry the
// outcome — and must ask the script for its status path rather than recomputing
// the default, the drift bug #102 already paid for once with the checkpoint.
func TestTranscriptStatusReachesTheEscalation(t *testing.T) {
	src := readFileString(t, escalatePathFor(t))

	if !retainPathRE.MatchString(src) {
		t.Errorf("%s does not call `%s --path` — recomputing the status path independently is how one script reports \"nothing retained\" while the file sits on disk (#102)",
			escalateScript, retainScript)
	}
	if !strings.Contains(src, "Where the transcript is") {
		t.Errorf("%s builds an escalation body with no transcript section — the retention would be invisible to the only human who needs it (#106)", escalateScript)
	}
	if !strings.Contains(src, "Transcript: unknown") {
		t.Errorf("%s does not handle a MISSING retention status — absence must be reported, not omitted: no status means the `always()` step never ran, which is itself the finding (#102, #106)", escalateScript)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// fixtureTranscript is a miniature claude-execution-output.json: the SDK message
// array the action writes. Every marker below is a distinct thing #106 requires
// to survive — the model's reasoning, the tool it called, the arguments it called
// it with, and what came back — so a partial implementation fails on the specific
// piece it dropped rather than on a vague "content missing".
const (
	markerReasoning  = "REASONING-MARKER-considering-the-flaky-path"
	markerToolName   = "Bash"
	markerToolInput  = "TOOL-INPUT-MARKER-go-test-./..."
	markerToolResult = "TOOL-RESULT-MARKER-FAIL-devsystem"
)

func writeFixture(t *testing.T, dir string) string {
	t.Helper()
	events := []any{
		map[string]any{"type": "system", "subtype": "init", "session_id": "sess-abc123"},
		map[string]any{"type": "assistant", "session_id": "sess-abc123", "message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": markerReasoning},
				map[string]any{"type": "tool_use", "name": markerToolName, "input": map[string]any{"command": markerToolInput}},
			},
		}},
		map[string]any{"type": "user", "session_id": "sess-abc123", "message": map[string]any{
			"content": []any{
				map[string]any{"type": "tool_result", "is_error": true, "content": markerToolResult},
			},
		}},
		map[string]any{"type": "result", "subtype": "error_max_turns", "num_turns": 61, "is_error": true},
	}
	b, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	p := filepath.Join(dir, "claude-execution-output.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

type sink struct {
	mu   sync.Mutex
	body strings.Builder
	hits int
}

func (s *sink) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits++
	s.body.Write(b)
	if r.URL.Path != "/loki/api/v1/push" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func runRetain(t *testing.T, env map[string]string) (stdout, stderr, status string) {
	t.Helper()
	cmd := exec.Command("bash", retainPathFor(t))
	cmd.Env = append(os.Environ(), "GENESIS_LOKI_USER=", "GENESIS_LOKI_TOKEN=")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	// Never non-zero: retention is diagnostics, and a failing step here would
	// turn a green run red or hand a live agent an error to investigate.
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s exited non-zero (%v)\nstdout: %s\nstderr: %s", retainScript, err, out.String(), errb.String())
	}
	if p := env["GENESIS_TRANSCRIPT_STATUS"]; p != "" {
		if b, err := os.ReadFile(p); err == nil {
			status = string(b)
		}
	}
	return out.String(), errb.String(), status
}

// TestRetainTranscriptShipsToPrivateSinkAndNotStdout is the behavioural proof of
// both halves of #106's done criteria, and the reason it runs the script rather
// than reading it: "the transcript reaches a private place" and "nothing
// world-readable gains content" are claims about what the process emits, and
// reviewing shell for that is exactly how a leak on a public repo happens.
func TestRetainTranscriptShipsToPrivateSinkAndNotStdout(t *testing.T) {
	s := &sink{}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()

	tmp := t.TempDir()
	fixture := writeFixture(t, tmp)
	statusPath := filepath.Join(tmp, "status.md")

	stdout, stderr, status := runRetain(t, map[string]string{
		"GENESIS_EXECUTION_FILE":    fixture,
		"GENESIS_TRANSCRIPT_STATUS": statusPath,
		"GENESIS_LOKI_URL":          srv.URL,
		"GITHUB_RUN_ID":             "30353938690",
	})

	s.mu.Lock()
	got := s.body.String()
	hits := s.hits
	s.mu.Unlock()

	if hits == 0 {
		t.Fatalf("the sink received no push at all\nstdout: %s\nstderr: %s", stdout, stderr)
	}

	// Half one: the middle of the run actually arrives.
	for _, want := range []string{markerReasoning, markerToolName, markerToolInput, markerToolResult} {
		if !strings.Contains(got, want) {
			t.Errorf("the private sink never received %q — that is one of the four things #106 requires to survive (reasoning, tool name, tool input, tool result), so a transcript missing it still cannot answer where the turns went", want)
		}
	}

	// Half two: and arrives ONLY there. stdout/stderr land in a world-readable
	// Actions log on this public repo.
	for _, forbidden := range []string{markerReasoning, markerToolInput, markerToolResult} {
		if strings.Contains(stdout, forbidden) || strings.Contains(stderr, forbidden) {
			t.Errorf("%s printed transcript content (%q) to stdout/stderr — that is the world-readable Actions log on a PUBLIC repo, and it re-creates exactly the exposure the private sink was chosen to avoid (#106)",
				retainScript, forbidden)
		}
	}

	// The push must be a well-formed Loki payload with unique, ordered
	// timestamps: Loki silently drops an entry duplicating an existing
	// (timestamp, line) in a stream and acks it 204 — the data loss log.sh
	// already ate once.
	assertLokiPayload(t, got)

	if !strings.Contains(status, "retained") {
		t.Errorf("status file does not report a successful retention, got: %q", status)
	}
	if !strings.Contains(status, "30353938690") {
		t.Errorf("status file omits the run id, so the LogQL query it prints cannot select this run's transcript; got: %q", status)
	}
}

func assertLokiPayload(t *testing.T, raw string) {
	t.Helper()
	var payload struct {
		Streams []struct {
			Stream map[string]string `json:"stream"`
			Values [][]string        `json:"values"`
		} `json:"streams"`
	}
	// Batches are separate requests concatenated by the sink; the fixture is
	// small enough to be one, which keeps this assertion exact.
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("sink did not receive valid Loki JSON: %v", err)
	}
	if len(payload.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(payload.Streams))
	}
	st := payload.Streams[0]
	if st.Stream["kind"] != "transcript" {
		t.Errorf("stream labels %v lack kind=\"transcript\" — the escalation's LogQL query selects on it", st.Stream)
	}
	for _, label := range []string{"session", "run_id", "i"} {
		if _, isLabel := st.Stream[label]; isLabel {
			t.Errorf("%q is a stream LABEL — high-cardinality labels mint a new Loki stream per run forever; it belongs in the line, where `| json` promotes it at query time (log.sh made this call already)", label)
		}
	}

	seen := map[string]bool{}
	var prev string
	for i, v := range st.Values {
		if len(v) != 2 {
			t.Fatalf("value %d is not [ts, line]", i)
		}
		if seen[v[0]] {
			t.Errorf("duplicate timestamp %s at entry %d — Loki drops duplicate (timestamp, line) within a stream and ACKS IT 204, so this is silent transcript loss", v[0], i)
		}
		seen[v[0]] = true
		if prev != "" && len(v[0]) == len(prev) && v[0] <= prev {
			t.Errorf("timestamp %s at entry %d does not increase — turn order is the point of reading a transcript", v[0], i)
		}
		prev = v[0]
		var entry map[string]any
		if err := json.Unmarshal([]byte(v[1]), &entry); err != nil {
			t.Errorf("entry %d is not JSON, so `| json` cannot parse it in Grafana: %v", i, err)
		}
	}
}

// TestRetainTranscriptReportsAbsenceRatherThanFailingSilently covers the failure
// mode this entire line of work keeps re-hitting: something that quietly does
// nothing is indistinguishable from something that worked. Both ways retention
// can be unavailable must produce a status a human can act on.
func TestRetainTranscriptReportsAbsenceRatherThanFailingSilently(t *testing.T) {
	t.Run("no execution file", func(t *testing.T) {
		tmp := t.TempDir()
		statusPath := filepath.Join(tmp, "status.md")
		_, _, status := runRetain(t, map[string]string{
			"GENESIS_EXECUTION_FILE":    filepath.Join(tmp, "absent.json"),
			"GENESIS_TRANSCRIPT_STATUS": statusPath,
			"GENESIS_LOKI_URL":          "http://127.0.0.1:1/unused",
		})
		if !strings.Contains(status, "NOT retained") {
			t.Errorf("a missing execution file produced status %q — the action writes that file on both its success and error paths, so its absence means the agent step died before producing any messages, and that is a finding worth stating", status)
		}
	})

	t.Run("no sink configured", func(t *testing.T) {
		tmp := t.TempDir()
		fixture := writeFixture(t, tmp)
		statusPath := filepath.Join(tmp, "status.md")
		_, _, status := runRetain(t, map[string]string{
			"GENESIS_EXECUTION_FILE":    fixture,
			"GENESIS_TRANSCRIPT_STATUS": statusPath,
			"GENESIS_LOKI_URL":          "",
		})
		if !strings.Contains(status, "NOT retained") {
			t.Errorf("an unconfigured sink produced status %q — silently retaining nothing is how months of dropped Loki lines went unnoticed before log.sh started reporting push failures", status)
		}
		if strings.Contains(status, markerReasoning) {
			t.Errorf("the status file embeds transcript content — escalate.sh renders it verbatim into a PUBLIC issue (#106)")
		}
	})

	t.Run("sink rejects the push", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		tmp := t.TempDir()
		fixture := writeFixture(t, tmp)
		statusPath := filepath.Join(tmp, "status.md")
		_, _, status := runRetain(t, map[string]string{
			"GENESIS_EXECUTION_FILE":    fixture,
			"GENESIS_TRANSCRIPT_STATUS": statusPath,
			"GENESIS_LOKI_URL":          srv.URL,
		})
		if !strings.Contains(status, "NOT retained") || !strings.Contains(status, "401") {
			t.Errorf("a rejected push produced status %q — a 401 that reads as success is precisely the silent-loss shape log.sh was fixed for; the code must reach the human", status)
		}
	})
}
