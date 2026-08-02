package kube

import (
	"encoding/json"
	"fmt"
	"strings"
)

// jsonPatchOp is one RFC 6902 operation, decoded far enough to guard it and no
// further.
//
// Only the three fields the guard reasons about are named. A patch's `value` is
// deliberately not decoded here: it is the caller's payload, it can be an
// arbitrarily-shaped object, and re-encoding it would risk changing the bytes the
// API server sees. The guard's question is "where does this op point?", which is
// answered entirely by op, path, and from.
type jsonPatchOp struct {
	Op   string `json:"op"`
	Path string `json:"path"`
	From string `json:"from"`
}

// resourceVersionPointer is where the optimistic-concurrency precondition lives in
// a JSON-patch document. It is a constant rather than a literal because it appears
// twice — the op this file authors and the pointer callers are refused — and those
// two must never drift apart.
const resourceVersionPointer = "/metadata/resourceVersion"

// protectedPatchPaths are the JSON pointers a caller may not touch, because the
// executor owns what they mean.
//
// The first three are identity: the [WriteScope] admits exactly one request path,
// and an op that rewrote metadata.name, .namespace or .uid would be describing a
// different object than the one that path names — the same refusal
// [withResourceVersion] makes on the strategic-merge side, restated in pointer
// form. The fourth is the precondition itself: it is injected below, and a caller
// that also set it would be relaxing the optimistic-concurrency check that makes
// an approval mean anything.
var protectedPatchPaths = []string{
	"/metadata/name",
	"/metadata/namespace",
	"/metadata/uid",
	resourceVersionPointer,
}

// withResourceVersionOp validates a caller-supplied JSON-patch document and
// prepends the optimistic-concurrency precondition to it.
//
// The precondition travels as a `replace` of /metadata/resourceVersion rather than
// as an RFC 6902 `test`, and the choice is about which error a stale approval
// produces. A failed `test` is rejected by the patch engine before the object ever
// reaches the update path, which surfaces as 422 Unprocessable Entity — a class
// [Executor.act] would report as [ErrExecute], indistinguishable from an admission
// rejection. A `replace` instead leaves the patched object carrying the version the
// proposal was reasoned about, and the API server's normal update path compares it
// against the live one and answers 409 Conflict. That is the same mechanism the
// strategic-merge path already relies on, so a stale rollback and a stale restart
// fail identically, as [ErrPreconditionConflict], and the retry policy that already
// knows not to re-drive a conflict needs no new case.
//
// The op is PREPENDED so the precondition is asserted against the object as the
// server found it. Appending would assert it against the object the caller's own
// ops had already rewritten, which is a check on the patch rather than on the
// target.
func withResourceVersionOp(patch []byte, resourceVersion string) ([]byte, error) {
	if strings.TrimSpace(resourceVersion) == "" {
		return nil, fmt.Errorf("%w: patch requires a resourceVersion", ErrMissingPrecondition)
	}
	if len(patch) == 0 {
		return nil, fmt.Errorf("%w: empty patch", ErrInvalidPatch)
	}

	// Decoded twice on purpose: once into typed ops to guard them, and once into
	// raw messages to re-emit them byte-for-byte. Re-encoding a decoded `value`
	// would round-trip the caller's payload through Go's JSON defaults — reordering
	// object keys, renormalizing numbers — and the request a human previewed would
	// then not be the request that executes.
	var raw []json.RawMessage
	if err := json.Unmarshal(patch, &raw); err != nil {
		return nil, fmt.Errorf("%w: patch is not a JSON array of operations: %w", ErrInvalidPatch, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: patch contains no operations", ErrInvalidPatch)
	}

	var ops []jsonPatchOp
	if err := json.Unmarshal(patch, &ops); err != nil {
		return nil, fmt.Errorf("%w: patch operations are not objects: %w", ErrInvalidPatch, err)
	}
	for i, op := range ops {
		if err := validatePatchOp(i, op); err != nil {
			return nil, err
		}
	}

	precondition, err := json.Marshal(struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value string `json:"value"`
	}{Op: "replace", Path: resourceVersionPointer, Value: resourceVersion})
	if err != nil {
		return nil, fmt.Errorf("%w: encoding resourceVersion precondition: %w", ErrInvalidPatch, err)
	}

	out := make([]json.RawMessage, 0, len(raw)+1)
	out = append(out, precondition)
	out = append(out, raw...)

	body, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("%w: re-encoding patch: %w", ErrInvalidPatch, err)
	}
	return body, nil
}

// validatePatchOp refuses an operation that is malformed or that points somewhere
// the executor owns.
//
// `from` is checked exactly as `path` is, and forgetting it would be the whole
// hole: `{"op":"move","from":"/metadata/uid","path":"/spec/x"}` never names a
// protected path in its `path` and removes one anyway.
func validatePatchOp(i int, op jsonPatchOp) error {
	if strings.TrimSpace(op.Op) == "" {
		return fmt.Errorf("%w: operation %d has no op", ErrInvalidPatch, i)
	}
	if err := checkPatchPointer(i, "path", op.Path); err != nil {
		return err
	}
	if op.From == "" {
		return nil
	}
	return checkPatchPointer(i, "from", op.From)
}

// checkPatchPointer refuses a JSON pointer that reaches a protected field.
//
// "Reaches" is the substance. An exact match is the obvious case and the least
// interesting one: `replace /metadata` rewrites the whole metadata object,
// including all four protected fields, while naming none of them, and `replace ""`
// replaces the entire document. So a pointer is refused when it IS a protected
// path, when it is an ANCESTOR of one, and when it is a DESCENDANT of one — the
// last because a protected field is a scalar, so anything below it is a caller
// confusion rather than a legitimate target.
func checkPatchPointer(i int, field, pointer string) error {
	if pointer == "" || pointer == "/" {
		return fmt.Errorf("%w: operation %d targets the whole document (%s %q); the executor owns the object's identity",
			ErrInvalidPatch, i, field, pointer)
	}
	if !strings.HasPrefix(pointer, "/") {
		return fmt.Errorf("%w: operation %d has a %s that is not a JSON pointer: %q",
			ErrInvalidPatch, i, field, pointer)
	}
	for _, protected := range protectedPatchPaths {
		if pointersOverlap(pointer, protected) {
			return fmt.Errorf("%w: operation %d may not target %s (%s %q); the target and its precondition are fixed by the approved scope",
				ErrInvalidPatch, i, protected, field, pointer)
		}
	}
	return nil
}

// pointersOverlap reports whether two JSON pointers name the same location or one
// contains the other. Comparison is per-segment (via the trailing separator) so
// "/metadata/names" is not mistaken for a relative of "/metadata/name".
func pointersOverlap(a, b string) bool {
	switch {
	case a == b:
		return true
	case strings.HasPrefix(b, a+"/"):
		return true
	case strings.HasPrefix(a, b+"/"):
		return true
	default:
		return false
	}
}
