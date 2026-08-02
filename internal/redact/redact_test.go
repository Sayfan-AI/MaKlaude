package redact

import (
	"strings"
	"testing"
)

// TestString_RemovesEverySeededSecretShape covers the pattern set directly. The
// [aidiagnose] tests exercise these through the model-egress path; this exercises
// them where they now live, so a change to the table fails here rather than in a
// package that merely happens to call it.
func TestString_RemovesEverySeededSecretShape(t *testing.T) {
	cases := map[string]string{
		"a GitHub token":         "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		"a GitHub PAT":           "github_pat_11ABCDEFG0abcdefghijklmnop",
		"a Slack bot token":      "xoxb-1234567890-0987654321-abcdefghijkl",
		"a model API key":        "sk-abcdefghijklmnopqrstuvwxyz0123456789",
		"an AWS access key id":   "AKIAIOSFODNN7EXAMPLE",
		"a JWT":                  "eyJhbGciOi.eyJzdWIiOi.SflKxwRJSMeK",
		"an email address":       "operator@example.com",
		"a bearer header":        "Authorization: Bearer abc123def456ghi789jkl",
		"a labelled password":    "password=hunter2correcthorse",
		"a high-entropy blob":    "Zm9vYmFyYmF6cXV4Y29ycmVjdGhvcnNl",
		"a labelled client key":  "client_secret: s3cr3t-value-here",
		"a token inside a value": "could not authenticate with token ghp_ZYXWVUTSRQPONMLKJIHGFEDCBA987654",
	}

	for name, secret := range cases {
		t.Run(name, func(t *testing.T) {
			in := "the container said: " + secret + " and then exited"
			got := String(in)
			if strings.Contains(got, secret) {
				t.Fatalf("the secret survived: %q", got)
			}
			if !strings.Contains(got, Placeholder) {
				t.Fatalf("material was removed with no placeholder to show it: %q", got)
			}
			if !strings.Contains(got, "and then exited") {
				t.Fatalf("redaction ate the surrounding diagnostic context: %q", got)
			}
		})
	}
}

// TestString_IsIdempotent pins a property the audit trail relies on: records are
// redacted when they are appended AND again when they are rendered, so that
// "anything Lifecycle emits has been redacted" is true of the function rather than
// of the way it currently happens to be called. Two passes must produce the same
// text as one, or the second pass would mangle the first pass's output — most
// obviously by re-matching its own placeholder.
func TestString_IsIdempotent(t *testing.T) {
	inputs := []string{
		"token=ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 in the pod spec",
		"Authorization: Bearer abc123def456ghi789jkl",
		"reached out to operator@example.com about AKIAIOSFODNN7EXAMPLE",
		"deployment shop/web is mid-rollout: 1/3 ready",
	}
	for _, in := range inputs {
		once := String(in)
		if twice := String(once); twice != once {
			t.Errorf("redacting twice changed the result:\n in: %q\none: %q\ntwo: %q", in, once, twice)
		}
	}
}

// TestString_LeavesOrdinaryTextAlone is the counterweight to over-redaction. The
// posture is deliberately lossy, but a redactor that shreds normal Kubernetes
// diagnostics makes the trail useless in the ordinary case to protect against the
// rare one — so the strings an operator actually reads are pinned.
func TestString_LeavesOrdinaryTextAlone(t *testing.T) {
	ordinary := []string{
		`node "node-a" is cordoned; the scheduler will place no new pods on it`,
		"pod shop/web-abc restarted 7 times with exit code 137 (OOMKilled)",
		"deployment shop/web is still at revision 4; no new rollout has appeared yet",
		"image ghcr.io/acme/web:v1.2.3 could not be pulled",
		"PATCH /api/v1/nodes/node-a",
	}
	for _, in := range ordinary {
		if got := String(in); got != in {
			t.Errorf("redaction mangled ordinary text:\n in: %q\nout: %q", in, got)
		}
	}
}
