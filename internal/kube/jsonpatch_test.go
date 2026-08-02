package kube

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestWithResourceVersionOp_RefusesWhatTheExecutorOwns is the JSON-patch guard's
// central test, and the cases are chosen for REACH rather than for coverage.
//
// An exact hit on a protected pointer is the case anyone would write. The ones that
// matter are the pointers that reach a protected field without naming it: replacing
// /metadata rewrites all four at once, replacing the empty pointer replaces the whole
// document, and a `move` names its source in `from` — never in `path` — so a guard that
// only reads `path` lets `{"op":"move","from":"/metadata/uid",...}` delete the object's
// identity while looking clean.
func TestWithResourceVersionOp_RefusesWhatTheExecutorOwns(t *testing.T) {
	cases := map[string]struct {
		patch   string
		wantErr error
	}{
		"names the object": {
			patch:   `[{"op":"replace","path":"/metadata/name","value":"other"}]`,
			wantErr: ErrInvalidPatch,
		},
		"renames the namespace": {
			patch:   `[{"op":"replace","path":"/metadata/namespace","value":"kube-system"}]`,
			wantErr: ErrInvalidPatch,
		},
		"relaxes the precondition": {
			patch:   `[{"op":"remove","path":"/metadata/resourceVersion"}]`,
			wantErr: ErrInvalidPatch,
		},
		"replaces all of metadata, naming none of it": {
			patch:   `[{"op":"replace","path":"/metadata","value":{}}]`,
			wantErr: ErrInvalidPatch,
		},
		"replaces the whole document": {
			patch:   `[{"op":"replace","path":"","value":{}}]`,
			wantErr: ErrInvalidPatch,
		},
		"replaces the root pointer": {
			patch:   `[{"op":"replace","path":"/","value":{}}]`,
			wantErr: ErrInvalidPatch,
		},
		"reaches below a protected scalar": {
			patch:   `[{"op":"add","path":"/metadata/uid/0","value":"x"}]`,
			wantErr: ErrInvalidPatch,
		},
		"moves identity out through from": {
			patch:   `[{"op":"move","from":"/metadata/uid","path":"/spec/paused"}]`,
			wantErr: ErrInvalidPatch,
		},
		"copies identity out through from": {
			patch:   `[{"op":"copy","from":"/metadata/name","path":"/spec/paused"}]`,
			wantErr: ErrInvalidPatch,
		},
		"is not a JSON pointer": {
			patch:   `[{"op":"replace","path":"spec/template","value":{}}]`,
			wantErr: ErrInvalidPatch,
		},
		"is an object, not an array of operations": {
			patch:   `{"op":"replace","path":"/spec/paused","value":true}`,
			wantErr: ErrInvalidPatch,
		},
		"is an empty array": {
			patch:   `[]`,
			wantErr: ErrInvalidPatch,
		},
		"is JSON null": {
			patch:   `null`,
			wantErr: ErrInvalidPatch,
		},
		"has an operation with no op": {
			patch:   `[{"path":"/spec/paused","value":true}]`,
			wantErr: ErrInvalidPatch,
		},
		"has an operation that is not an object": {
			patch:   `["replace /spec/paused"]`,
			wantErr: ErrInvalidPatch,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := withResourceVersionOp([]byte(tc.patch), "1234"); !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got: %v", tc.wantErr, err)
			}
		})
	}
}

// TestWithResourceVersionOp_RequiresAPrecondition proves there is no unconditional
// variant of a JSON patch, matching the strategic-merge path: a missing resourceVersion
// is refused before the body is even parsed.
func TestWithResourceVersionOp_RequiresAPrecondition(t *testing.T) {
	for name, version := range map[string]string{"empty": "", "whitespace": "   "} {
		t.Run(name, func(t *testing.T) {
			_, err := withResourceVersionOp([]byte(`[{"op":"replace","path":"/spec/paused","value":true}]`), version)
			if !errors.Is(err, ErrMissingPrecondition) {
				t.Fatalf("expected ErrMissingPrecondition, got: %v", err)
			}
		})
	}
}

// TestWithResourceVersionOp_AdmitsANeighbourOfAProtectedPath proves the containment
// check compares whole pointer SEGMENTS. "/metadata/names" shares a textual prefix with
// "/metadata/name" and is a different field; a guard using plain string prefixes would
// refuse it, and a guard that got the direction wrong would admit "/metadata/name/x".
func TestWithResourceVersionOp_AdmitsANeighbourOfAProtectedPath(t *testing.T) {
	body, err := withResourceVersionOp([]byte(`[{"op":"add","path":"/metadata/labels/team","value":"platform"}]`), "1234")
	if err != nil {
		t.Fatalf("a label under metadata was refused: %v", err)
	}
	if !strings.Contains(string(body), "/metadata/labels/team") {
		t.Fatalf("the admitted operation is missing from the body: %s", body)
	}
}

// TestWithResourceVersionOp_PrependsThePreconditionAndPreservesTheBody covers the two
// properties the injection rests on.
//
// The precondition must come FIRST, so it is asserted against the object as the server
// found it rather than against one the caller's own operations already rewrote.
//
// And the caller's `value` must survive byte-for-byte. It is re-emitted as a raw message
// rather than decoded and re-encoded, because round-tripping through Go's JSON defaults
// reorders object keys — so a preview a human read and the request that executes would
// stop being the same bytes. The value below is deliberately in non-alphabetical order:
// a decode/encode cycle sorts it, this does not.
func TestWithResourceVersionOp_PrependsThePreconditionAndPreservesTheBody(t *testing.T) {
	const value = `{"zeta":1,"alpha":2}`
	body, err := withResourceVersionOp([]byte(`[{"op":"replace","path":"/spec/template","value":`+value+`}]`), "1234")
	if err != nil {
		t.Fatalf("composing the patch: %v", err)
	}

	var ops []struct {
		Op    string          `json:"op"`
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(body, &ops); err != nil {
		t.Fatalf("the composed patch is not a JSON array of operations: %v (%s)", err, body)
	}
	if len(ops) != 2 {
		t.Fatalf("the composed patch has %d operations, want the precondition plus the caller's one: %s", len(ops), body)
	}
	if ops[0].Op != "replace" || ops[0].Path != resourceVersionPointer {
		t.Fatalf("the first operation is %+v, want the resourceVersion precondition", ops[0])
	}
	if string(ops[0].Value) != `"1234"` {
		t.Fatalf("the precondition carries %s, want the expected resourceVersion", ops[0].Value)
	}
	if ops[1].Path != "/spec/template" {
		t.Fatalf("the caller's operation is %+v, want it preserved after the precondition", ops[1])
	}
	if string(ops[1].Value) != value {
		t.Fatalf("the caller's value was re-encoded as %s, want the bytes it supplied (%s)", ops[1].Value, value)
	}
}
