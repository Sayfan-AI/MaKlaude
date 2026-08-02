//go:build e2e

// This file holds a stub GitHub issues API — the durable approval trail that
// TestE2E_BinaryTwoPassGatedRemediation drives the shipped binary against.
//
// # Why a stub server rather than the MemorySink the sibling test uses
//
// The gate's shape is two-pass: MaKlaude proposes on one pass, a human labels the
// artifact, and MaKlaude acts on a LATER pass. remediation_test.go collapses that into
// one process because it drives the packages directly, so approve.MemorySink.Decide is
// enough. The binary test cannot: each pass is a separate `maklaude remediate` process,
// and an in-memory trail dies with the process that made it. The one property the
// two-pass gate rests on — the decision outlives the run that asked for it — is exactly
// the property an in-process sink cannot have.
//
// # Why this is not a degraded path
//
// escalate.GitHubConfig.APIBase is an arbitrary REST base URL, not a
// GitHub-Enterprise-only knob: GitHubConfig.Configured() tests owner, repo and token
// only, and neither approve.NewGitHubSink nor escalate.NewGitHubSink validates the
// scheme or host. So pointing MAKLAUDE_GITHUB_API at loopback routes the entire LIVE
// approve.GitHubSink here — real net/http, real Authorization header, real JSON
// encode/decode, real label-plus-events decision recovery, and approve.SinkFromEnv
// reporting live=true. Nothing is substituted. The state simply lives in this process
// instead of on github.com, which is what lets it survive between the binary's runs.
//
// # What it deliberately does not prove
//
// A stub answers "does MaKlaude's client drive the gate correctly across processes".
// It does not answer "does api.github.com behave the way this stub does": pagination
// edges, secondary rate limits, and the events endpoint's eventual consistency are all
// outside it. That is the same trade kind makes against a real cluster, and it is
// stated here rather than left to be discovered later.
//
// # The surface
//
// Seven request shapes, which is the whole of what approve.GitHubSink and
// escalate.GitHubSink issue between them:
//
//	GET    /repos/{o}/{r}/issues                    (state, labels, per_page, page)
//	POST   /repos/{o}/{r}/issues
//	PATCH  /repos/{o}/{r}/issues/{n}
//	POST   /repos/{o}/{r}/issues/{n}/comments
//	POST   /repos/{o}/{r}/issues/{n}/labels
//	DELETE /repos/{o}/{r}/issues/{n}/labels/{label}
//	GET    /repos/{o}/{r}/issues/{n}/events
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubEvent is one entry of an issue's timeline. Only the label events matter:
// approve.GitHubSink reads this endpoint solely to recover WHO applied a decision
// label and WHEN, which the issue payload does not carry.
type stubEvent struct {
	kind      string // "labeled" or "unlabeled"
	label     string
	actor     string
	actorType string // "User" or "Bot"
	createdAt time.Time
}

// stubIssue is one artifact in the trail.
type stubIssue struct {
	number   int
	title    string
	body     string
	labels   []string
	state    string // "open" or "closed"
	comments []string
	events   []stubEvent
}

func (i *stubIssue) hasLabel(name string) bool {
	for _, l := range i.labels {
		if l == name {
			return true
		}
	}
	return false
}

// githubStub is an in-memory GitHub issues API over httptest.
//
// Every field behind the mutex is touched from two directions — the binary's HTTP
// requests on the server's goroutines, and the test's own inspection and simulated
// human approvals on the test goroutine — so the lock is not decorative.
type githubStub struct {
	srv *httptest.Server

	owner, repo string
	token       string

	// selfLogin is who the stub attributes any label change made THROUGH THE API to.
	// That is MaKlaude itself: it is the account the binary authenticates as, and the
	// point of recording it is that approve.GitHubSink.isSelfActor must be able to tell
	// MaKlaude's own labels from a person's. Simulated human decisions go through
	// decideAs instead, which names a different login.
	selfLogin string

	mu     sync.Mutex
	next   int
	issues map[int]*stubIssue

	// lastStamp is the most recent timestamp handed out. See now() for the two
	// properties it enforces and why both are load-bearing.
	lastStamp time.Time

	// unauthorized counts requests that arrived without the expected bearer token. The
	// test asserts it is zero — a stub that answered unauthenticated requests would
	// pass whether or not the binary sends its credential.
	unauthorized int
}

// newGitHubStub starts a stub trail and registers its shutdown with the test.
func newGitHubStub(t *testing.T, owner, repo, token, selfLogin string) *githubStub {
	t.Helper()
	s := &githubStub{
		owner:     owner,
		repo:      repo,
		token:     token,
		selfLogin: selfLogin,
		next:      1,
		issues:    map[int]*stubIssue{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

// apiBase is the value MAKLAUDE_GITHUB_API is set to.
func (s *githubStub) apiBase() string { return s.srv.URL }

// now returns the timestamp for the next event. Caller holds the lock.
//
// It enforces two properties, and dropping either one breaks a different thing:
//
//   - It never runs BEHIND the real clock. approve.Body stamps its preview instant from
//     the binary's own time.Now, and approve.disqualify refuses an approval recorded
//     before the body it decides (ReasonApprovalPredatesPreview). An invented clock
//     sitting in the past therefore makes every simulated approval a stale one, and no
//     amount of re-approving clears it — the gate is right and the harness is lying about
//     when the human decided. TestStubTrailDecisionsCannotPredateTheArtifactTheyDecide
//     holds this, because the symptom (a two-pass test that refuses four times and gives
//     up) points nowhere near the clock.
//   - It strictly increases, at whole-second resolution. Real GitHub timestamps are
//     RFC3339 with no sub-second part and listEvents serializes them that way, so two
//     events stamped within the same second would arrive indistinguishable. Running the
//     stamp AHEAD of real time when events come in faster than one per second is the
//     right direction to fail: a decision in the near future still postdates every
//     artifact rendered before it, and nothing in the gate refuses a decision for being
//     recent.
func (s *githubStub) now() time.Time {
	t := time.Now().UTC().Truncate(time.Second)
	if !t.After(s.lastStamp) {
		t = s.lastStamp.Add(time.Second)
	}
	s.lastStamp = t
	return t
}

func (s *githubStub) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.token {
		s.mu.Lock()
		s.unauthorized++
		s.mu.Unlock()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}

	prefix := fmt.Sprintf("/repos/%s/%s/issues", s.owner, s.repo)
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, prefix), "/")

	s.mu.Lock()
	defer s.mu.Unlock()

	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			s.listIssues(w, r)
		case http.MethodPost:
			s.createIssue(w, r)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": r.Method})
		}
		return
	}

	parts := strings.Split(rest, "/")
	number, err := strconv.Atoi(parts[0])
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	issue, ok := s.issues[number]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}

	switch {
	case len(parts) == 1 && r.Method == http.MethodPatch:
		s.patchIssue(w, r, issue)
	case len(parts) == 2 && parts[1] == "comments" && r.Method == http.MethodPost:
		s.addComment(w, r, issue)
	case len(parts) == 2 && parts[1] == "labels" && r.Method == http.MethodPost:
		s.addLabels(w, r, issue)
	case len(parts) == 3 && parts[1] == "labels" && r.Method == http.MethodDelete:
		s.removeLabel(w, issue, parts[2])
	case len(parts) == 2 && parts[1] == "events" && r.Method == http.MethodGet:
		s.listEvents(w, issue)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

// listIssues answers the collection endpoint, honoring the three query parameters
// approve.GitHubSink and escalate.GitHubSink actually send.
//
// Pagination is real but trivial: both clients stop paging when a page comes back
// shorter than per_page, and this trail never holds enough artifacts to fill one.
// Answering page 2 with an empty array (rather than repeating page 1) is what keeps a
// client that DID page from looping forever.
func (s *githubStub) listIssues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	wantState := q.Get("state")
	wantLabel := q.Get("labels")

	numbers := make([]int, 0, len(s.issues))
	for n := range s.issues {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)

	out := []map[string]any{}
	for _, n := range numbers {
		issue := s.issues[n]
		if wantState != "" && wantState != "all" && issue.state != wantState {
			continue
		}
		if wantLabel != "" && !issue.hasLabel(wantLabel) {
			continue
		}
		out = append(out, issueJSON(issue))
	}

	if page, _ := strconv.Atoi(q.Get("page")); page > 1 {
		out = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *githubStub) createIssue(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
		return
	}

	issue := &stubIssue{
		number: s.next,
		title:  payload.Title,
		body:   payload.Body,
		state:  "open",
	}
	s.next++
	s.issues[issue.number] = issue
	for _, l := range payload.Labels {
		s.applyLabel(issue, l, s.selfLogin, "User")
	}
	writeJSON(w, http.StatusCreated, issueJSON(issue))
}

// patchIssue applies a title/body/labels/state update.
//
// The labels field is a full replacement, matching GitHub, and the diff against the
// current set is what produces the label events. Getting that wrong in the lenient
// direction — recording a `labeled` event for a label that was already there — would
// re-stamp an existing approval's attribution on every pass and quietly refresh a
// consent that should have expired.
func (s *githubStub) patchIssue(w http.ResponseWriter, r *http.Request, issue *stubIssue) {
	var payload struct {
		Title  *string   `json:"title"`
		Body   *string   `json:"body"`
		Labels *[]string `json:"labels"`
		State  *string   `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
		return
	}
	if payload.Title != nil {
		issue.title = *payload.Title
	}
	if payload.Body != nil {
		issue.body = *payload.Body
	}
	if payload.State != nil {
		issue.state = *payload.State
	}
	if payload.Labels != nil {
		want := map[string]bool{}
		for _, l := range *payload.Labels {
			want[l] = true
		}
		for _, l := range append([]string(nil), issue.labels...) {
			if !want[l] {
				s.dropLabel(issue, l, s.selfLogin, "User")
			}
		}
		for _, l := range *payload.Labels {
			s.applyLabel(issue, l, s.selfLogin, "User")
		}
	}
	writeJSON(w, http.StatusOK, issueJSON(issue))
}

func (s *githubStub) addComment(w http.ResponseWriter, r *http.Request, issue *stubIssue) {
	var payload struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
		return
	}
	issue.comments = append(issue.comments, payload.Body)
	writeJSON(w, http.StatusCreated, map[string]any{"id": len(issue.comments)})
}

func (s *githubStub) addLabels(w http.ResponseWriter, r *http.Request, issue *stubIssue) {
	var payload struct {
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
		return
	}
	for _, l := range payload.Labels {
		s.applyLabel(issue, l, s.selfLogin, "User")
	}
	writeJSON(w, http.StatusOK, labelsJSON(issue))
}

// removeLabel deletes one label, answering 404 when it is not present — the status
// approve.GitHubSink.RemoveLabel deliberately treats as success.
func (s *githubStub) removeLabel(w http.ResponseWriter, issue *stubIssue, label string) {
	if !issue.hasLabel(label) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Label does not exist"})
		return
	}
	s.dropLabel(issue, label, s.selfLogin, "User")
	writeJSON(w, http.StatusOK, labelsJSON(issue))
}

func (s *githubStub) listEvents(w http.ResponseWriter, issue *stubIssue) {
	out := make([]map[string]any, 0, len(issue.events))
	for _, ev := range issue.events {
		// An actorless event is serialised as a JSON null, not as an object with empty
		// strings in it. That is the shape api.github.com actually returns when the
		// account behind an event is deleted or anonymised, and it is the shape
		// approve.GitHubSink's `ev.Actor != nil` branch exists to survive — an
		// {"login":""} object would take the other branch and prove the wrong thing.
		var actor any
		if ev.actor != "" {
			actor = map[string]any{"login": ev.actor, "type": ev.actorType}
		}
		out = append(out, map[string]any{
			"event":      ev.kind,
			"actor":      actor,
			"label":      map[string]any{"name": ev.label},
			"created_at": ev.createdAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// applyLabel adds a label and records the event. A label already present is a no-op
// in both — see patchIssue for why the second half matters.
// Caller holds the lock.
func (s *githubStub) applyLabel(issue *stubIssue, label, actor, actorType string) {
	if issue.hasLabel(label) {
		return
	}
	issue.labels = append(issue.labels, label)
	issue.events = append(issue.events, stubEvent{
		kind: "labeled", label: label, actor: actor, actorType: actorType, createdAt: s.now(),
	})
}

// dropLabel removes a label and records the `unlabeled` event that retires its
// attribution. Caller holds the lock.
func (s *githubStub) dropLabel(issue *stubIssue, label, actor, actorType string) {
	kept := issue.labels[:0]
	found := false
	for _, l := range issue.labels {
		if l == label {
			found = true
			continue
		}
		kept = append(kept, l)
	}
	issue.labels = kept
	if !found {
		return
	}
	issue.events = append(issue.events, stubEvent{
		kind: "unlabeled", label: label, actor: actor, actorType: actorType, createdAt: s.now(),
	})
}

// --- The test's own view of the trail. None of this goes over HTTP. ---

// decideAs applies a decision label attributed to an arbitrary login: the simulated
// human.
//
// It does NOT go through the HTTP surface, and that is the point rather than a
// shortcut. Every request the binary can make is attributed to selfLogin, so a
// decision the binary itself could produce is precisely the one the gate must refuse.
// Reaching past the API is the only way to record a label event MaKlaude has no way of
// forging — which is what a person clicking a button in GitHub's UI actually is.
func (s *githubStub) decideAs(t *testing.T, number int, label, login string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	issue, ok := s.issues[number]
	if !ok {
		t.Fatalf("stub trail: no issue #%d to label %q", number, label)
	}
	s.applyLabel(issue, label, login, "User")
}

// decideAsNobody applies a decision label whose `labeled` event names no actor at
// all.
//
// This is not a malformed event: GitHub serves `"actor": null` on the timeline once
// the account behind an entry is deleted or anonymised, so a label really can arrive
// with a valid timestamp and no recoverable identity. The gate's contract is that such
// an approval is refused rather than honoured anonymously
// ([approve.ReasonUnattributedApproval]) — an approval nobody can be named for defeats
// the reason the signal is a label EVENT rather than the label alone.
func (s *githubStub) decideAsNobody(t *testing.T, number int, label string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	issue, ok := s.issues[number]
	if !ok {
		t.Fatalf("stub trail: no issue #%d to label %q", number, label)
	}
	s.applyLabel(issue, label, "", "")
}

// snapshotIssue copies one artifact out for assertions.
func (s *githubStub) snapshotIssue(t *testing.T, number int) stubIssue {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	issue, ok := s.issues[number]
	if !ok {
		t.Fatalf("stub trail: no issue #%d", number)
	}
	cp := *issue
	cp.labels = append([]string(nil), issue.labels...)
	cp.comments = append([]string(nil), issue.comments...)
	cp.events = append([]stubEvent(nil), issue.events...)
	return cp
}

// openIssuesMentioning returns the numbers of open artifacts whose body names a
// substring — how the test finds the artifact for its own target without duplicating
// approve's marker format.
func (s *githubStub) openIssuesMentioning(needle string) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int
	for n, issue := range s.issues {
		if issue.state == "open" && strings.Contains(issue.body, needle) {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// unauthorizedCount reports requests that arrived without the bearer token.
func (s *githubStub) unauthorizedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unauthorized
}

// --- JSON rendering. Only the fields the two sinks decode. ---

func issueJSON(i *stubIssue) map[string]any {
	labels := make([]map[string]any, 0, len(i.labels))
	for _, l := range i.labels {
		labels = append(labels, map[string]any{"name": l})
	}
	return map[string]any{
		"number": i.number,
		"title":  i.title,
		"body":   i.body,
		"labels": labels,
		"state":  i.state,
	}
}

func labelsJSON(i *stubIssue) []map[string]any {
	out := make([]map[string]any, 0, len(i.labels))
	for _, l := range i.labels {
		out = append(out, map[string]any{"name": l})
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
