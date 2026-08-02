package devsystem

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The Actions loop was dead for 3.5 days and nothing in the repo could tell.
//
// `secrets.ANTHROPIC_API_KEY` went invalid on 2026-07-30T01:47:18Z. Every
// Claude-invoking run since — 18 of them across four workflows — retried
// `401 authentication_failed` through the SDK's full ten-attempt ladder and
// died before the agent's first turn: `num_turns: 1`, `total_cost_usd: 0`, no
// work, no diagnosis. `main` stayed green the whole time, because a workflow
// that cannot authenticate is not a workflow that fails a test. See #150.
//
// The credential itself lives in repo settings and no test can reach it. What a
// test *can* pin is the shape of the reference — which is where the recoverable
// half of this failure lives. Two properties, and the second matters more than
// the first:
//
//  1. Every Claude-invoking workflow supplies exactly one credential input.
//     Zero means the run dies at validate-env; two means the action's precedence
//     silently picks one and the other is decoration.
//
//  2. The credential is the SAME one across all of them. A partial swap is the
//     realistic mistake here — five workflows, one edit each, and the one that
//     gets missed keeps 401ing on a schedule nobody watches. This repo has
//     learned that shape three times now (turn-budget floors, concurrency-group
//     membership, escalate-step membership): when a safety property depends on
//     every member of a set opting in, test the SET, not the member.
//
// Deliberately NOT asserted: which mechanism is correct. Swapping the whole set
// to workload identity federation (`anthropic_federation_rule_id` +
// `anthropic_organization_id`) is a legitimate future move, and this test should
// pass on that day without an edit — it constrains agreement, not choice.

// credentialInputs are the mutually-exclusive ways claude-code-action can
// authenticate to the Anthropic API, per its validate-env.ts. Federation is one
// mechanism spelled with two inputs, so it is matched as a unit below.
var credentialInputs = []*regexp.Regexp{
	regexp.MustCompile(`(?m)^\s*anthropic_api_key:`),
	regexp.MustCompile(`(?m)^\s*claude_code_oauth_token:`),
	regexp.MustCompile(`(?m)^\s*anthropic_federation_rule_id:`),
}

var credentialNames = []string{
	"anthropic_api_key",
	"claude_code_oauth_token",
	"anthropic_federation_rule_id",
}

// deadCredential is the specific secret that 401'd for 3.5 days. Re-introducing
// a reference to it is not automatically wrong — the human may rotate it and
// swap back — but it must then be swapped everywhere, which property 2 covers.
// This exists so the failure message names the outage rather than making a
// reader rediscover it.
const deadCredential = "secrets.ANTHROPIC_API_KEY"

// TestClaudeWorkflowsDeclareExactlyOneCredential is property 1.
func TestClaudeWorkflowsDeclareExactlyOneCredential(t *testing.T) {
	for name, body := range claudeWorkflows(t) {
		var present []string
		for i, re := range credentialInputs {
			if re.MatchString(body) {
				present = append(present, credentialNames[i])
			}
		}

		switch len(present) {
		case 1:
			// The only acceptable shape.
		case 0:
			t.Errorf("%s invokes claude-code-action with no credential input (one of %s). "+
				"The action fails at validate-env before the agent's first turn, which looks "+
				"identical to the #150 outage: one turn, zero cost, no diagnosis.",
				name, strings.Join(credentialNames, ", "))
		default:
			t.Errorf("%s declares %d credential inputs (%s). The action's precedence picks one "+
				"silently — a static credential beats federation — so the others are decoration "+
				"that reads like a working fallback. Declare exactly one.",
				name, len(present), strings.Join(present, ", "))
		}
	}
}

// TestClaudeWorkflowsAgreeOnOneCredential is property 2, and the one that
// catches a partial swap.
func TestClaudeWorkflowsAgreeOnOneCredential(t *testing.T) {
	byMechanism := make(map[string][]string)
	for name, body := range claudeWorkflows(t) {
		for i, re := range credentialInputs {
			if re.MatchString(body) {
				byMechanism[credentialNames[i]] = append(byMechanism[credentialNames[i]], name)
			}
		}
	}

	if len(byMechanism) <= 1 {
		return
	}

	var detail []string
	for _, mech := range credentialNames {
		if wfs := byMechanism[mech]; len(wfs) > 0 {
			detail = append(detail, mech+": "+strings.Join(sorted(wfs), ", "))
		}
	}
	t.Errorf("Claude-invoking workflows disagree on how to authenticate — %s. "+
		"This is the partial-swap failure: the workflows left behind keep dying on a "+
		"schedule while the swapped ones work, and a green main says nothing about it "+
		"(see #150, where %s 401'd for 3.5 days unnoticed). Move the whole set together.",
		strings.Join(detail, "; "), deadCredential)
}

// credentialSecretRE pulls the secret NAME out of a credential input line, e.g.
// `claude_code_oauth_token: ${{ secrets.CLAUDE_CODE_OAUTH_TOKEN }}` → the name.
var credentialSecretRE = regexp.MustCompile(
	`(?m)^\s*(?:anthropic_api_key|claude_code_oauth_token):\s*\$\{\{\s*secrets\.([A-Z0-9_]+)\s*\}\}`)

// activateSecretRE matches the seeding calls in activate.sh.
var activateSecretRE = regexp.MustCompile(`gh secret set ([A-Z0-9_]+)`)

// TestActivateSeedsTheCredentialWorkflowsRead is property 3: the bootstrap
// script and the workflows must name the SAME secret.
//
// These are two files deriving one shared fact, which this repo has already been
// bitten by once (checkpoint.sh --path exists precisely so escalate.sh doesn't
// recompute the location and drift). Here the drift is worse than a wrong path:
// activate.sh seeds a secret nothing reads, the operator sees "Dev system
// activated" and a green secrets page, and every Claude run dies before its
// first turn — the exact silent shape of #150, reached by a different route.
//
// Federation uses no `secrets.` credential of this form, so a set of workflows
// on federation yields no names and this test is vacuously satisfied. That is
// intended: activate.sh has nothing to seed in that world.
func TestActivateSeedsTheCredentialWorkflowsRead(t *testing.T) {
	needed := make(map[string][]string)
	for name, body := range claudeWorkflows(t) {
		for _, m := range credentialSecretRE.FindAllStringSubmatch(body, -1) {
			needed[m[1]] = append(needed[m[1]], name)
		}
	}
	if len(needed) == 0 {
		return
	}

	path := filepath.Join("..", "..", ".genesis", "scripts", "activate.sh")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v — activation is how the secrets get set at all, so "+
			"if this script moved, this guard must follow it", path, err)
	}
	seeded := make(map[string]bool)
	for _, m := range activateSecretRE.FindAllStringSubmatch(string(b), -1) {
		seeded[m[1]] = true
	}

	for secret, wfs := range needed {
		if !seeded[secret] {
			t.Errorf("workflows %s authenticate with secrets.%s, but activate.sh never runs "+
				"`gh secret set %s`. Activation reports success, the workflows are enabled, and "+
				"every Claude run then dies at validate-env with no credential — silently, on a "+
				"schedule (see #150).", strings.Join(sorted(wfs), ", "), secret, secret)
		}
	}
}

// sorted returns a sorted copy, so the failure message above is stable across
// runs — map iteration order is not.
func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
