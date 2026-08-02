package approve

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
)

// This file is the approval trail's READ-ONLY history: every artifact it ever opened,
// closed ones included. It exists for exactly one caller — a rebuild of the trust ledger
// from the artifacts — and it is deliberately not part of [ApprovalSink].
//
// # Why history is not on the sink interface
//
// [ApprovalSink] is the gate's surface, and every operation on it is scoped to OPEN
// artifacts on purpose: a closed artifact is a decided one, frozen at what was actually
// decided or done, and the gate must have no way to reach back into it. Adding an
// enumerate-everything method to that interface would hand every gate implementation a
// capability it must never use, and would oblige every test fake to grow one.
//
// So the capability lives on the concrete sinks, and the rebuild depends on a narrow
// interface of its own (see internal/rebuild). Membership is by having the method, not by
// being an [ApprovalSink] — which also means a deployment wired with a sink that cannot
// enumerate history simply cannot rebuild, rather than silently rebuilding from nothing.
//
// # A rebuild that reads a SHORTER history than the truth is dangerous in one direction
//
// A lost failure re-grants autonomy; a lost approval merely delays it. That asymmetry is
// why the reader fails loudly on a marker it cannot parse instead of skipping it — but the
// rule has to be applied where the marker is READ, not here. This returns bodies and lets
// the reader decide, so the sink has no way to quietly drop one.
//
// What IS filtered here is an artifact that was never this trail's: no proposal marker
// means no MaKlaude approval artifact, whatever labels a person hung on it. That is not
// history loss, because it was never history.

// ArchivedArtifact is one approval artifact as a rebuild reads it: the reference, so a
// failure can name what could not be read, and the body, which carries the markers.
//
// It carries neither the decision nor the labels. Everything a trust entry is derived
// from is inside the lifecycle marker, and re-deriving any of it from labels would create
// a second, disagreeing answer to a question the marker already answers.
type ArchivedArtifact struct {
	Ref  ActionRef
	Body string
}

// ListAll returns every MaKlaude approval artifact in the repository, open and closed,
// oldest first.
//
// Ordering is by issue number ascending, which on this trail is creation order. A rebuild
// does not depend on it — the ledger orders entries by their recorded instants — but a
// stable order makes a failure reproducible and makes two runs comparable.
func (g *GitHubSink) ListAll(ctx context.Context) ([]ArchivedArtifact, error) {
	var out []ArchivedArtifact
	page := 1
	for {
		q := url.Values{}
		// "all" is the whole point: a finished execution's artifact is CLOSED, so the
		// open-only query every other call uses would return exactly the history a
		// rebuild is missing — none of it.
		q.Set("state", "all")
		q.Set("labels", ManagedLabel)
		q.Set("sort", "created")
		q.Set("direction", "asc")
		q.Set("per_page", "100")
		q.Set("page", fmt.Sprintf("%d", page))

		var issues []ghIssue
		if err := g.do(ctx, http.MethodGet, g.issuesPath()+"?"+q.Encode(), nil, &issues); err != nil {
			return nil, fmt.Errorf("approve: reading the approval history: %w", err)
		}
		for i := range issues {
			// The issues endpoint also returns pull requests; skip those.
			if issues[i].PullRequest != nil {
				continue
			}
			if _, ok := ParseProposalMarker(issues[i].Body); !ok {
				continue
			}
			out = append(out, ArchivedArtifact{
				Ref:  ActionRef(fmt.Sprintf("%d", issues[i].Number)),
				Body: issues[i].Body,
			})
		}
		if len(issues) < 100 {
			break
		}
		page++
	}
	return out, nil
}

// ListAll returns every artifact the sink holds, closed ones included, in reference order
// so a test sees deterministic output.
func (s *MemorySink) ListAll(_ context.Context) ([]ArchivedArtifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]ArchivedArtifact, 0, len(s.artifacts))
	for _, a := range s.artifacts {
		if _, ok := ParseProposalMarker(a.body); !ok {
			continue
		}
		out = append(out, ArchivedArtifact{Ref: a.ref, Body: a.body})
	}
	sort.Slice(out, func(i, j int) bool { return refLess(out[i].Ref, out[j].Ref) })
	return out, nil
}

// refLess orders two references numerically when both are numbers, and lexically
// otherwise. A plain string comparison would put "10" before "9", which is only cosmetic
// here but makes a failure message read as though artifacts were returned out of order.
func refLess(a, b ActionRef) bool {
	na, aok := refNumber(string(a))
	nb, bok := refNumber(string(b))
	if aok && bok {
		return na < nb
	}
	return a < b
}

// refNumber parses a reference as a positive integer, ok=false for anything else.
func refNumber(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}
