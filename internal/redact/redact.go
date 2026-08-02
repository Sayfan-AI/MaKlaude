// Package redact strips credentials, tokens, and obvious PII out of strings
// before they are written anywhere MaKlaude does not control.
//
// # Why this is its own package
//
// The rules below were written for one boundary — [aidiagnose] sending
// cluster-derived evidence to a third-party model — and they are exactly the rules
// the audit trail needs for a different boundary: a record that is rendered into a
// GitHub issue anyone with read access can see. Two boundaries with the same
// requirement must not have two implementations, because the failure mode is
// silent: the second copy drifts, misses the pattern the first one learned, and
// nobody notices until a credential is already in a public artifact.
//
// So the patterns live here, in a package with no dependencies beyond regexp, and
// both callers use them. The alternative — importing [aidiagnose] from the audit
// layer — would have dragged an HTTP client and the entire diagnosis chain into the
// dependency graph of the one component whose job is to be boringly trustworthy.
//
// # The posture
//
// Redaction is deliberately lossy and errs toward over-redaction. It is far better
// to blank an innocuous high-entropy string than to leak a single credential, since
// by the time a value reaches this function it is about to leave the process or
// land in a durable, human-readable artifact. [String] never adds information,
// never fails, and is idempotent: running it twice produces the same output as
// running it once, so a caller that cannot be sure whether a value was already
// sanitized may safely sanitize it again.
package redact

import "regexp"

// Placeholder is the fixed token that replaces every piece of redacted material.
// It is a single, recognizable, secret-free string so a reader (or a test) can see
// that redaction happened without learning anything about what was removed.
const Placeholder = "[REDACTED]"

// redactors is the ordered set of patterns [String] applies. Each entry either
// blanks a whole match or keeps a leading label and blanks only the value after it.
//
// The patterns are applied in order; earlier, more specific rules (keyed secrets,
// known token shapes) run before the broad high-entropy sweep so a labelled secret
// keeps its label for context while its value is destroyed.
var redactors = []struct {
	// re matches the sensitive material.
	re *regexp.Regexp
	// repl is the replacement, using $-references into re's submatches. Rules that
	// preserve a label reference $1 (or a named group); rules that blank the whole
	// match use the bare placeholder.
	repl string
}{
	// Authorization/Bearer headers written as "Bearer <token>". This runs BEFORE
	// the keyed rule so a full "Authorization: Bearer <token>" has its token
	// destroyed rather than the keyed rule stopping at the word "Bearer".
	{
		re:   regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*`),
		repl: "Bearer " + Placeholder,
	},
	// key=value / key: value where the key names a credential-ish field. Keeps the
	// key (and the delimiter) so the text still reads "password: [REDACTED]",
	// which is useful context, while the value is destroyed. Case-insensitive; the
	// value runs to whitespace, quote, comma, or end.
	{
		re:   regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|secret[_-]?key|client[_-]?secret|credential|auth|authorization|bearer|session|cookie|private[_-]?key)\b(\s*[:=]\s*)("?)([^\s"',;]+)`),
		repl: `$1$2$3` + Placeholder,
	},
	// Well-known provider token shapes, matched whole so they never survive even
	// unlabelled: GitHub (ghp_/gho_/ghs_/ghr_/github_pat_), Slack (xox[baprs]-…),
	// Anthropic/OpenAI (sk-…), AWS access key ids (AKIA…), and JWTs (three
	// base64url segments separated by dots).
	{re: regexp.MustCompile(`\bgh[posru]_[A-Za-z0-9]{20,}\b`), repl: Placeholder},
	{re: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`), repl: Placeholder},
	{re: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`), repl: Placeholder},
	{re: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`), repl: Placeholder},
	{re: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), repl: Placeholder},
	{re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\b`), repl: Placeholder},
	// Email addresses — obvious PII.
	{re: regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`), repl: Placeholder},
	// High-entropy blobs: long unbroken runs of base64/hex-ish characters that no
	// human message would contain but a leaked key/cert/data value would. The 24+
	// length keeps ordinary words, image tags, and object names intact.
	{re: regexp.MustCompile(`\b[A-Za-z0-9+/_-]{24,}={0,2}\b`), repl: Placeholder},
}

// String returns s with every pattern in [redactors] replaced. It removes labelled
// secrets (keeping the label for context), known provider token shapes, bearer
// tokens, email addresses, and high-entropy blobs.
//
// The worst case is that an innocuous high-entropy string is blanked, which is a
// safe trade for a layer whose entire justification is that the value is leaving
// MaKlaude's control.
func String(s string) string {
	for _, r := range redactors {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}
