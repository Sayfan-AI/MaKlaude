package escalate

import (
	"sort"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
)

// Reconcile computes the deterministic plan of [Action]s that brings the
// external comms trail (the currently-open tracked issues) into agreement with
// the current set of findings. It is the pure heart of this package: it performs
// no I/O, reads no clock, and never touches GitHub or a cluster — given the same
// inputs it always returns the same plan in the same order.
//
// The diff is a three-way set comparison keyed on [detect.Identity]:
//
//   - An identity present in findings but with NO tracked issue -> [ActionOpen].
//   - An identity present in findings AND already tracked -> [ActionUpdate]
//     (recurrence becomes an update, never a duplicate issue).
//   - An identity tracked but ABSENT from findings -> [ActionClose] (the problem
//     has cleared; close the trail so it stays auditable).
//
// Duplicates are handled defensively so the function is robust against a messy
// external system. If two findings share an identity (they should not within a
// single analysis, but a caller could concatenate sloppily), only the first is
// acted on. If two open issues claim the same identity (possible if a previous
// run crashed mid-open, or a human opened a colliding issue), the first is
// updated and the rest are closed as duplicates — the trail self-heals toward
// exactly one issue per problem.
//
// The returned plan is ordered deterministically: closes first (clear stale
// noise before adding new), then opens and updates, with each group sorted by
// identity. The ordering is part of the contract so tests and audit logs see
// stable output.
func Reconcile(findings []detect.Finding, tracked []TrackedIssue) []Action {
	// Index current findings by identity, keeping the first occurrence of each so
	// a duplicated identity in the input cannot produce two actions.
	currentByID := make(map[detect.Identity]detect.Finding, len(findings))
	for i := range findings {
		id := findings[i].Identity
		if _, seen := currentByID[id]; !seen {
			currentByID[id] = findings[i]
		}
	}

	// Walk the tracked issues, deciding update vs close, and remember which
	// identities are already covered by a surviving (first) issue so we do not
	// also open a fresh one for them.
	seenTrackedID := make(map[detect.Identity]bool, len(tracked))
	var closes, updates []Action

	for i := range tracked {
		ti := tracked[i]
		_, stillActive := currentByID[ti.Identity]

		switch {
		case !stillActive:
			// The problem has cleared (or this is an extra duplicate issue handled
			// below) — close it.
			closes = append(closes, Action{
				Kind:     ActionClose,
				Identity: ti.Identity,
				Ref:      ti.Ref,
			})
		case !seenTrackedID[ti.Identity]:
			// First open issue for an active problem — update it in place.
			seenTrackedID[ti.Identity] = true
			updates = append(updates, Action{
				Kind:     ActionUpdate,
				Identity: ti.Identity,
				Finding:  currentByID[ti.Identity],
				Ref:      ti.Ref,
			})
		default:
			// A second (duplicate) open issue for the same active problem. Collapse
			// the trail back to one by closing the extra.
			closes = append(closes, Action{
				Kind:     ActionClose,
				Identity: ti.Identity,
				Ref:      ti.Ref,
			})
		}
	}

	// Any current finding with no surviving tracked issue is newly seen — open
	// one. Iterating findings (not the map) is not order-stable because map reads
	// are random, so collect then sort below.
	var opens []Action
	for i := range findings {
		id := findings[i].Identity
		if seenTrackedID[id] {
			continue
		}
		if _, alreadyPlanned := findingPlanned(opens, id); alreadyPlanned {
			continue
		}
		opens = append(opens, Action{
			Kind:     ActionOpen,
			Identity: id,
			Finding:  currentByID[id],
		})
	}

	sortActions(closes)
	sortActions(opens)
	sortActions(updates)

	// Closes first so the trail sheds stale issues before new ones are recorded;
	// then opens and updates. Each group is identity-sorted for stable output.
	plan := make([]Action, 0, len(closes)+len(opens)+len(updates))
	plan = append(plan, closes...)
	plan = append(plan, opens...)
	plan = append(plan, updates...)
	return plan
}

// findingPlanned reports whether an open action for the given identity is
// already in the slice, so a duplicated finding identity yields a single open.
func findingPlanned(opens []Action, id detect.Identity) (int, bool) {
	for i := range opens {
		if opens[i].Identity == id {
			return i, true
		}
	}
	return 0, false
}

// sortActions orders a group of actions by identity ascending. Identity is fully
// deterministic, so the resulting order is reproducible for any given input.
func sortActions(actions []Action) {
	sort.Slice(actions, func(i, j int) bool {
		return actions[i].Identity < actions[j].Identity
	})
}
