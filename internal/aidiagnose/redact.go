package aidiagnose

import "github.com/Sayfan-AI/MaKlaude/internal/redact"

// redactionPlaceholder is the fixed token that replaces every piece of redacted
// material. It is a single, recognizable, secret-free string so a reader (or a
// test) can see that redaction happened without learning anything about what was
// removed.
const redactionPlaceholder = redact.Placeholder

// Redact strips sensitive material from a string before it is allowed to leave
// the process. It is the crux of the T5 safety boundary: [buildRequest] runs the
// fully-assembled evidence through it as the LAST step before a [Request] is
// handed to a [Provider], so no secret value, token, credential, or obvious PII
// egresses — even one that leaked into a free-text event message or container
// waiting message the collector captured verbatim.
//
// The patterns themselves live in [redact], which is where they moved once the
// audit trail needed the same posture for a different boundary (a record rendered
// into a world-readable GitHub issue). One boundary per caller, one implementation
// of the rules: a second copy would drift, and the drift would be invisible until
// something leaked. This function stays because it is the name this package's
// callers and tests already use, and because the doc above is about THIS egress
// path rather than about the patterns.
func Redact(s string) string { return redact.String(s) }
