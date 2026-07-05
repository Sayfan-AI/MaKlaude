package aidiagnose

import "regexp"

// redactionPlaceholder is the fixed token that replaces every piece of redacted
// material. It is a single, recognizable, secret-free string so a reader (or a
// test) can see that redaction happened without learning anything about what was
// removed.
const redactionPlaceholder = "[REDACTED]"

// redactors is the ordered set of patterns [Redact] applies to strip sensitive
// material before any egress. The set is deliberately conservative-in-one-direction:
// it is far better to over-redact an innocuous string than to leak a single
// credential, since the redacted evidence is heading OUT of the process to a
// third-party model. Each entry either blanks a whole match or keeps a leading
// label and blanks only the value after it.
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
		repl: "Bearer " + redactionPlaceholder,
	},
	// key=value / key: value where the key names a credential-ish field. Keeps the
	// key (and the delimiter) so the evidence still reads "password: [REDACTED]",
	// which is useful context, while the value is destroyed. Case-insensitive; the
	// value runs to whitespace, quote, comma, or end.
	{
		re:   regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|secret[_-]?key|client[_-]?secret|credential|auth|authorization|bearer|session|cookie|private[_-]?key)\b(\s*[:=]\s*)("?)([^\s"',;]+)`),
		repl: `$1$2$3` + redactionPlaceholder,
	},
	// Well-known provider token shapes, matched whole so they never survive even
	// unlabelled: GitHub (ghp_/gho_/ghs_/ghr_/github_pat_), Slack (xox[baprs]-…),
	// Anthropic/OpenAI (sk-…), AWS access key ids (AKIA…), and JWTs (three
	// base64url segments separated by dots).
	{re: regexp.MustCompile(`\bgh[posru]_[A-Za-z0-9]{20,}\b`), repl: redactionPlaceholder},
	{re: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`), repl: redactionPlaceholder},
	{re: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`), repl: redactionPlaceholder},
	{re: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`), repl: redactionPlaceholder},
	{re: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), repl: redactionPlaceholder},
	{re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\b`), repl: redactionPlaceholder},
	// Email addresses — obvious PII.
	{re: regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`), repl: redactionPlaceholder},
	// High-entropy blobs: long unbroken runs of base64/hex-ish characters that no
	// human message would contain but a leaked key/cert/data value would. The 24+
	// length keeps ordinary words, image tags, and object names intact.
	{re: regexp.MustCompile(`\b[A-Za-z0-9+/_-]{24,}={0,2}\b`), repl: redactionPlaceholder},
}

// Redact strips sensitive material from a string before it is allowed to leave
// the process. It is the crux of the T5 safety boundary: [buildRequest] runs the
// fully-assembled evidence through it as the LAST step before a [Request] is
// handed to a [Provider], so no secret value, token, credential, or obvious PII
// egresses — even one that leaked into a free-text event message or container
// waiting message the collector captured verbatim.
//
// Redaction is intentionally lossy and errs toward over-redaction: it removes
// labelled secrets (keeping the label for context), known provider token shapes,
// bearer tokens, email addresses, and high-entropy blobs. It never adds
// information and never fails; the worst case is that an innocuous high-entropy
// string is blanked, which is a safe trade for a layer whose entire justification
// is that data is leaving the process.
func Redact(s string) string {
	for _, r := range redactors {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}
