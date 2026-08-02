package audit

import "testing"

// The property that matters here is not that each parser handles the tokens somebody
// remembered to list — it is that the parsers and the String methods cover the SAME set.
// A token a renderer can emit and no parser can read is history that silently fails to
// rebuild, so both round-trip tests below are driven by exhausting the enum from its zero
// value upward rather than by a hand-written table that would drift as the enum grows.

// TestParsePhase_RoundTripsEveryRenderableToken walks the enum until String stops
// producing a stable token, so a phase added later fails here rather than in a rebuild.
func TestParsePhase_RoundTripsEveryRenderableToken(t *testing.T) {
	seen := 0
	for p := PhaseUnknown; ; p++ {
		token := p.String()
		if token == "" || token[0] == 'p' && len(token) > 6 && token[:6] == "phase(" {
			break
		}
		got, ok := ParsePhase(token)
		if !ok {
			t.Fatalf("ParsePhase(%q) reported the token unreadable, but Phase(%d).String() emits it", token, int(p))
		}
		if got != p {
			t.Fatalf("ParsePhase(%q) = %v, want %v", token, got, p)
		}
		seen++
		if seen > 64 {
			t.Fatal("the phase enum did not terminate; String no longer falls through to phase(N)")
		}
	}
	if seen != int(PhaseRolledBack)+1 {
		t.Fatalf("round-tripped %d phases, the enum has %d", seen, int(PhaseRolledBack)+1)
	}
}

// TestParseAuthority_RoundTripsEveryRenderableToken is the same exhaustion over the
// authority enum.
func TestParseAuthority_RoundTripsEveryRenderableToken(t *testing.T) {
	seen := 0
	for a := AuthorityUnattributed; ; a++ {
		token := a.String()
		if len(token) > 10 && token[:10] == "authority(" {
			break
		}
		got, ok := ParseAuthority(token)
		if !ok {
			t.Fatalf("ParseAuthority(%q) reported the token unreadable, but Authority(%d).String() emits it", token, int(a))
		}
		if got != a {
			t.Fatalf("ParseAuthority(%q) = %v, want %v", token, got, a)
		}
		seen++
		if seen > 64 {
			t.Fatal("the authority enum did not terminate; String no longer falls through to authority(N)")
		}
	}
	if seen != int(AuthorityPolicy)+1 {
		t.Fatalf("round-tripped %d authorities, the enum has %d", seen, int(AuthorityPolicy)+1)
	}
}

// TestParsers_RejectRatherThanDefault is the safety half.
//
// A parser that fell back to its zero value would turn every unreadable token into a
// plausible answer: an unknown phase would read as "nothing happened" and an unknown
// authority as "nobody is named". Both are wrong in the direction that loses history
// quietly, so absence must be reported.
func TestParsers_RejectRatherThanDefault(t *testing.T) {
	for _, token := range []string{"", "Verified", "verified ", "rolledback", "human ", "HUMAN", "phase(9)", "authority(9)", "approved-by-policy"} {
		if p, ok := ParsePhase(token); ok && token != "verified" {
			t.Errorf("ParsePhase(%q) = %v, ok — an unrecognized token must be refused", token, p)
		}
		if a, ok := ParseAuthority(token); ok && token != "human" {
			t.Errorf("ParseAuthority(%q) = %v, ok — an unrecognized token must be refused", token, a)
		}
	}
}

// TestParseAuthority_NeverManufacturesHumanReview pins the one mistake this parser must
// not make. A garbled authority that resolved to a human would write "a person reviewed
// this" onto an action no person saw, which the package doc calls the worst thing a
// trail can do.
func TestParseAuthority_NeverManufacturesHumanReview(t *testing.T) {
	for _, token := range []string{"", "hum", "Human", "person", "policy:some-rule", "unattributed "} {
		got, ok := ParseAuthority(token)
		if ok {
			t.Fatalf("ParseAuthority(%q) accepted a token the enum does not emit", token)
		}
		if got.HumanReviewed() {
			t.Fatalf("ParseAuthority(%q) failed to %v, which reports HumanReviewed", token, got)
		}
	}
}
