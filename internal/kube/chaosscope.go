package kube

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"k8s.io/client-go/rest"

	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
)

// The one door the chaos write path enters through.
//
// Milestone 6 adds a second kind of write: MaKlaude deliberately breaking a
// cluster a human has marked eligible, so its own behaviour under fault can be
// measured rather than assumed. That path lives in its own package with its own
// identity ([internal/chaos]), because a fault injector and a remediator are
// different things with different blast radii and should not share a
// ServiceAccount.
//
// What it does NOT get is its own transport guard. [WriteScope] is a whole-request
// pin — one method, one exact path, optionally forced dry-run — and its exactness
// is the property chaos needs most. A second guard written beside it would be a
// second thing to keep correct, and the two would drift in exactly the way that
// matters least visibly: the copy that is not the one a reader audits.
//
// So chaos reuses the guard and enters through [ChaosRestConfig], whose signature
// carries the two narrowings the milestone promised:
//
//   - It takes a [cluster.ChaosTarget], a token only internal/cluster can mint and
//     only for a cluster whose human-written eligibility marker verified. A caller
//     holding no token cannot build a chaos-capable config, so "is this cluster
//     eligible?" is not a question any call site has to remember to ask.
//   - It refuses a mutating scope that is not a chaos CRD request. The narrowed
//     guarantee — "no mutating verb except chaos CRDs, on chaos-eligible clusters"
//     — is therefore enforced at the door rather than asserted in prose about the
//     callers behind it.
//
// Note what this function is not: it is not a general "build me a write config"
// export. [restConfigForScope] stays unexported, so the remediation write path is
// still reachable only through [Executor], and the only widening of this package's
// exported surface is a door that needs a capability no ineligible cluster can
// produce.

// ChaosAPIPathPrefix is the API path prefix of every Chaos Mesh custom resource:
// the group is chaos-mesh.org, so every CR request begins with this.
//
// It is exported and lives here, in the package that VERIFIES the prefix, so the
// composer and the verifier share one string. Two constants that must agree drift,
// and the failure would be silent in the worst direction — a chaos path the
// composer builds and the verifier does not recognise refuses a legitimate
// experiment (loud), while any mismatch in the other direction admits a mutating
// request the narrowing was written to exclude (silent).
const ChaosAPIPathPrefix = "/apis/chaos-mesh.org/"

// ErrNotChaosScope is returned by [ChaosRestConfig] when the requested scope
// would mutate something that is not a Chaos Mesh custom resource.
//
// It is the enforcement point of the narrowed no-writes guarantee. A chaos target
// is permission to break one cluster with chaos experiments; it is not permission
// to patch its deployments or delete its pods, and those are refused here even
// though the cluster is eligible and the chaos identity's RBAC would refuse them
// too. Two independent refusals, in-process and at the API server, because the
// RBAC bundle is applied by a human and this is not.
var ErrNotChaosScope = errors.New("kube: mutating scope is not a chaos-CRD request")

// ChaosRestConfig assembles a *rest.Config for a single scoped chaos request
// against a chaos-eligible cluster.
//
// The zero-value scope is permitted and grants nothing mutating: it yields a
// config that can read and refuses every write, which is what a constructor wants
// when it is checking that a handle can produce a usable client at all (see
// chaos.NewInjector) without granting authority to do so.
//
// A mutating scope must target a Chaos Mesh CR path. The check is deliberately
// three-part rather than a bare prefix test:
//
//  1. the path must be lexically clean — a path containing ".." would satisfy a
//     prefix test and resolve elsewhere, which is how a prefix guard gets walked
//     out of its own namespace;
//  2. it must begin with [ChaosAPIPathPrefix];
//  3. it must carry something after the prefix, so the group root itself is not a
//     target.
//
// Callers compose paths from validated DNS-1123 names, so (1) is unreachable from
// internal/chaos today. It is checked anyway because this function is exported and
// its whole job is to be the narrowing; a guard that holds only for its current
// callers is a convention, not a guard.
func ChaosRestConfig(target cluster.ChaosTarget, scope WriteScope) (*rest.Config, error) {
	if target == nil {
		return nil, fmt.Errorf("%w: nil chaos target", cluster.ErrChaosIneligible)
	}
	h := target.Handle()
	if h == nil {
		return nil, fmt.Errorf("%w: chaos target carries no cluster handle", cluster.ErrChaosIneligible)
	}

	if scope.isMutating() {
		if err := verifyChaosPath(scope.Path); err != nil {
			// WriteScope.String carries a method and a path and never a query string,
			// so this refusal is safe to log and to quote in an escalation.
			return nil, fmt.Errorf("%w for %s: scope %s targets no chaos custom resource (chaos writes must be under %s)",
				err, h.String(), scope.String(), ChaosAPIPathPrefix)
		}
	}

	return restConfigForScope(h, scope)
}

// verifyChaosPath reports whether p is a Chaos Mesh custom-resource path.
func verifyChaosPath(p string) error {
	if p != path.Clean(p) || strings.Contains(p, "..") {
		return ErrNotChaosScope
	}
	if !strings.HasPrefix(p, ChaosAPIPathPrefix) {
		return ErrNotChaosScope
	}
	if strings.TrimSpace(strings.TrimPrefix(p, ChaosAPIPathPrefix)) == "" {
		return ErrNotChaosScope
	}
	return nil
}
