package disclose

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
)

// This file is the disclosure trail's READ-ONLY history: every artifact it ever opened,
// closed ones included. It is the counterpart of the approval trail's, exists for the same
// single caller — a rebuild of the trust ledger from the artifacts — and is deliberately
// not part of [Sink] for the same reasons. See internal/approve/history.go for the full
// argument; the one specific to this trail is below.
//
// # Closing means something different here, which is exactly why it must be readable
//
// On the approval trail MaKlaude closes an artifact when it withdraws a request. Here it
// closes one only when the action never ran ([Trail.Abandon]); a disclosed action's
// artifact is closed by a PERSON, as their acknowledgement. So on this trail "closed"
// is the normal end state of a successful unattended action and carries no signal about
// the action at all — which makes [Sink.ListOpen] precisely the wrong query for history,
// and makes an acknowledged action the single most likely thing a rebuild would lose.

// ArchivedArtifact is one disclosure artifact as a rebuild reads it: the reference, so a
// failure can name what could not be read, and the body, which carries the markers.
type ArchivedArtifact struct {
	Ref  Ref
	Body string
}

// ListAll returns every MaKlaude disclosure artifact in the repository, open and closed,
// oldest first.
func (g *GitHubSink) ListAll(ctx context.Context) ([]ArchivedArtifact, error) {
	var out []ArchivedArtifact
	page := 1
	for {
		q := url.Values{}
		q.Set("state", "all")
		q.Set("labels", ManagedLabel)
		q.Set("sort", "created")
		q.Set("direction", "asc")
		q.Set("per_page", "100")
		q.Set("page", fmt.Sprintf("%d", page))
		path := fmt.Sprintf("/repos/%s/%s/issues?%s", g.cfg.Owner, g.cfg.Repo, q.Encode())

		var issues []ghIssue
		if err := g.do(ctx, http.MethodGet, path, nil, &issues); err != nil {
			return nil, fmt.Errorf("disclose: reading the disclosure history: %w", err)
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
				Ref:  Ref(fmt.Sprintf("%d", issues[i].Number)),
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
// otherwise, so "9" sorts before "10".
func refLess(a, b Ref) bool {
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
