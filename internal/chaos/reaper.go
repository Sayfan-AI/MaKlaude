package chaos

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// The teardown guarantee, and why one mechanism is not enough.
//
// A leaked chaos experiment on an operator's cluster is the worst outcome this
// milestone can produce, and "the deferred cleanup runs" does not achieve the
// opposite. A `defer` is code in a process: it does not run on SIGKILL, on an OOM
// kill, on a panic in another goroutine, or on a runner that vanishes mid-job.
// Every one of those leaves a CR on the cluster that MaKlaude is no longer around
// to delete. So there are two mechanisms, and they are independent on purpose:
//
//  1. [SelfLimit] — every action declares how its FAULT ends without MaKlaude, and
//     for the actions whose fault persists that is a mandatory, bounded, server-side
//     spec.duration. The enforcing party is Chaos Mesh's controller, on the cluster,
//     so this one survives MaKlaude's death by construction. It is checked before a
//     request is composed (see [Experiment.durationProblems]).
//  2. This file — a reaper that removes the OBJECTS MaKlaude left behind. It is the
//     mechanism that runs on the next cycle rather than at the moment of death, and
//     it is what makes a killed run's residue finite rather than permanent.
//
// Neither one subsumes the other. Duration bounds the fault and leaves the object;
// the reaper removes the object and cannot un-inject a fault that is still running.
// Together they say: the fault is over within [maxDuration] no matter what happens
// to MaKlaude, and the record of it is gone on the next sweep.
//
// # The property that is easy to get wrong
//
// A reaper that deletes chaos experiments is, mechanically, a bulk delete of other
// people's objects waiting for a bug. Chaos Mesh is a shared installation: a human
// running their own experiment on the same cluster, in the same namespace, has a CR
// that looks exactly like MaKlaude's to anything that filters loosely. Deleting one
// of those is its own incident — and worse than the leak this exists to prevent,
// because a leak is MaKlaude's own fault expiring on its own while a wrong delete is
// MaKlaude reaching into work it was never authorised for.
//
// So ownership is decided by three INDEPENDENT signals, all of which must agree, and
// the first of them is deliberately re-checked client-side even though the server was
// asked to filter on it. A label selector is a filter, and a filter is an
// optimisation: any proxy, any stub, any server-side bug that ignores it returns
// more than was asked for, and code that trusts the filter deletes whatever came
// back. See [Reaper.ownershipProblem].

// DefaultOrphanGrace is the recommended minimum age for an experiment object before
// a sweep will remove it.
//
// It is [maxDuration] plus slack rather than a round number chosen by taste, because
// the floor is load-bearing and the slack is not. See [NewReaper] for the argument.
const DefaultOrphanGrace = maxDuration + 5*time.Minute

// Sentinel errors the reaper produces.
var (
	// ErrReaperMisconfigured is returned by [NewReaper] when a reaper would be unsafe
	// to run: no injector, or a grace period short enough to reach an experiment that
	// could still be live.
	ErrReaperMisconfigured = errors.New("chaos: reaper configuration would be unsafe")

	// ErrReapFailed is returned when a sweep could not enumerate what is on the
	// cluster. It is the more serious of the two sweep failures: a sweep that cannot
	// list has no idea whether anything leaked, so a caller must not read it as
	// "nothing to do".
	ErrReapFailed = errors.New("chaos: sweep could not list experiments")

	// ErrReapIncomplete is returned when a sweep enumerated the cluster and failed to
	// remove at least one orphan it found. The [Sweep] is still returned and still
	// lists what WAS removed — see [Reaper.Reap].
	ErrReapIncomplete = errors.New("chaos: sweep left at least one orphan behind")
)

// derivedNameShape matches the names [Experiment.ObjectName] produces, and nothing
// else. It is the third ownership signal: MaKlaude's names are a digest of an
// experiment's own shape under a fixed prefix, so a hand-written `my-test-chaos` or
// a Chaos-Mesh-generated name cannot match it. The digest length is pinned to
// [nameDigestChars] at init rather than written as {12} here, so changing that
// constant cannot leave this pattern matching the old shape only.
var derivedNameShape = regexp.MustCompile(
	fmt.Sprintf(`^maklaude-[a-z0-9]+-[0-9a-f]{%d}$`, nameDigestChars))

// Reaper removes MaKlaude's own leftover chaos experiments from one chaos-eligible
// cluster.
//
// It holds an [Injector] rather than a target and a mode of its own, and that is a
// safety property rather than convenience. A reaper built from an injector is bound
// to the same cluster the injector may break and to the same kill switch, both fixed
// at the injector's construction — so there is no way to aim a reaper at a cluster
// nothing is allowed to inject into, and no second place where "is MaKlaude's write
// path on?" gets decided. It also means teardown here goes through
// [Injector.Remove]: the same UID precondition, the same scope-pinned single-request
// client, the same finalizer-respecting delete. A second delete path would be a
// second thing to keep correct, and the copy that drifts is always the one nobody
// audits.
type Reaper struct {
	// inj is the injector whose cluster this reaper sweeps, and whose Remove performs
	// every delete.
	inj *Injector

	// grace is how old an owned object must be before a sweep will remove it. Never
	// below [maxDuration] on a constructed Reaper.
	grace time.Duration

	// now is the clock ages are measured against. Injected so a test can pin it: a
	// sweep's whole decision is a comparison against wall-clock time, and a test that
	// reads the real clock is a test that passes at a different rate than it asserts.
	now func() time.Time
}

// NewReaper builds a [Reaper] for the cluster inj injects into.
//
// # Why the grace period has a floor, and why there is no exclusion list
//
// The obvious design for "don't reap what is still in use" is a set of names the
// current process considers live, passed in by the caller. That is a convention: it
// works exactly as long as every call site remembers to pass it, and the failure mode
// when one forgets is a sweep deleting a fault that is currently running — including
// one belonging to a DIFFERENT MaKlaude process, which no in-process set could ever
// have known about.
//
// The age floor replaces it with an argument. No fault this package asks for can
// outlive [maxDuration], because that ceiling is enforced before any request is
// composed and there is no unbounded action in the catalog (see [SelfLimit]). So an
// owned object older than maxDuration cannot belong to a live experiment under any
// MaKlaude, running anywhere. A grace of at least maxDuration is therefore
// structurally incapable of reaching a live fault, and needs to be told nothing about
// what this process is doing. A grace BELOW it is refused here rather than clamped,
// because the caller asked for something whose safety argument does not exist and
// silently substituting a different value would hide that.
//
// A zero grace is refused by the same check, which is the case worth naming: zero is
// the value a forgotten field, an unset config key, or a new call site gets, and it is
// the one value whose behaviour ("reap everything owned, however young") is both
// plausible-looking and destructive. The legitimate "remove it now" case is not a
// zero-grace sweep at all — it is [Injector.Remove], which the caller can use because
// it holds the UID of what it created.
//
// A nil clock defaults to [time.Now].
func NewReaper(inj *Injector, grace time.Duration, now func() time.Time) (*Reaper, error) {
	if inj == nil {
		return nil, fmt.Errorf("%w: nil injector", ErrReaperMisconfigured)
	}
	if grace < maxDuration {
		return nil, fmt.Errorf(
			"%w: orphan grace %s is below the %s ceiling on a fault's own lifetime, so a sweep could delete an experiment that is still running (use %s, or at least %s)",
			ErrReaperMisconfigured, grace, maxDuration, DefaultOrphanGrace, maxDuration)
	}
	if now == nil {
		now = time.Now
	}
	return &Reaper{inj: inj, grace: grace, now: now}, nil
}

// Cluster returns the registered name of the cluster this reaper sweeps.
func (r *Reaper) Cluster() string { return r.inj.Name() }

// Grace returns the minimum age an owned object must reach before a sweep removes it.
func (r *Reaper) Grace() time.Duration { return r.grace }

// Skipped records one object a sweep looked at and left alone, and why.
//
// The reasons are recorded rather than counted because they answer two different
// questions with the same shape. "Left alone: not MaKlaude's" on a cluster where
// MaKlaude is leaking would be a bug in the ownership test, and "left alone: too
// young" repeated across consecutive sweeps is a fault that is not expiring. Neither
// is visible from a number.
type Skipped struct {
	// Kind, Namespace and Name identify the object.
	Kind      Kind
	Namespace string
	Name      string

	// Reason says why the sweep did not remove it, in a form safe to log: it names
	// objects and label values the operator configured, never object contents.
	Reason string

	// Age is how old the object was when the sweep looked, or zero if the object
	// carried no creation timestamp.
	Age time.Duration
}

// ReapFailure records one orphan a sweep tried and failed to remove.
type ReapFailure struct {
	// Kind, Namespace, Name and UID identify what was not removed.
	Kind      Kind
	Namespace string
	Name      string
	UID       string

	// Err is why the delete failed, wrapping the same sentinels [Injector.Remove]
	// produces — so a caller can tell a precondition conflict (somebody else's object
	// now wears this name; MaKlaude's is already gone) from a denial or an outage.
	Err error
}

// Sweep records one pass of the reaper over one namespace.
//
// Like [Injected] it is a plain, fully-resolved value: the question it answers is
// "what did this sweep see, remove, and refuse to remove?", and answering it must not
// depend on re-reading a cluster that is by then deliberately broken.
type Sweep struct {
	// Cluster is the registered name of the cluster swept.
	Cluster string

	// Namespace is where experiment OBJECTS live — MaKlaude's own chaos namespace, not
	// where any fault landed.
	Namespace string

	// Scanned is how many objects the sweep examined, including ones it did not own.
	// A number well above len(Reaped)+len(Skipped) is impossible; a Scanned of zero
	// where an orphan was expected means the list came back empty, which is different
	// from the list failing (that is an error).
	Scanned int

	// Reaped lists the orphans removed, in the order they were removed.
	Reaped []Removal

	// Skipped lists every object left alone, with the reason.
	Skipped []Skipped

	// Failed lists the orphans this sweep could not remove. A non-empty Failed always
	// accompanies an [ErrReapIncomplete] error; it is on the record too so a caller
	// that logs the sweep does not have to parse the error to say which objects are
	// still out there.
	Failed []ReapFailure

	// DryRun reports whether the deletes were server-side previews, in which case
	// every entry in Reaped is still on the cluster.
	DryRun bool
}

// Reap removes MaKlaude's orphaned experiment objects from one namespace on the
// reaper's cluster.
//
// The sequence is list, decide, delete-one-by-one:
//
//   - The LIST goes through a client built with the ZERO scope, so the client that
//     decides what to delete provably cannot delete anything. That mirrors
//     [Injector.assertAbsent] and for the same reason: a read that informs a write
//     should not carry the write's authority.
//   - Ownership and age are decided locally, per object, from the object's own
//     labels, annotations, name shape and creation timestamp. See
//     [Reaper.ownershipProblem].
//   - Each delete is its own [Injector.Remove] call with the UID the list returned,
//     so a name recycled between the list and the delete is refused by the API server
//     rather than followed.
//
// One failed delete does not abort the sweep. Five leaked experiments where the first
// delete is denied is exactly the situation where the other four matter most, so
// failures are collected and the sweep continues. The returned [Sweep] is ALWAYS
// non-nil, including alongside an error: a partial sweep's record of what it did
// remove is the thing a caller most needs when part of it failed, and returning nil
// would throw that away to satisfy a convention.
//
// Errors: [ErrReapFailed] if the cluster could not be enumerated (in which case
// nothing was deleted and nothing is known about what leaked), [ErrReapIncomplete] if
// it was enumerated and at least one orphan survived.
func (r *Reaper) Reap(ctx context.Context, namespace string) (*Sweep, error) {
	sweep := &Sweep{
		Cluster:   r.inj.Name(),
		Namespace: namespace,
		DryRun:    r.inj.dryRun(),
	}

	if err := validateName("namespace", namespace); err != nil {
		return sweep, fmt.Errorf("%w: %w: %w", ErrReapFailed, ErrInvalidExperiment, err)
	}

	// Sorted so a sweep over a multi-kind catalog visits kinds in a stable order and
	// two sweeps of the same cluster produce comparable records.
	kinds := make([]Kind, 0, len(kindResource))
	for k := range kindResource {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(a, b int) bool { return kinds[a] < kinds[b] })

	var listErrs []error
	for _, kind := range kinds {
		found, err := r.list(ctx, kind, namespace)
		if err != nil {
			listErrs = append(listErrs, err)
			continue
		}
		sweep.Scanned += len(found)
		for _, obj := range found {
			r.consider(ctx, sweep, kind, namespace, obj)
		}
	}

	switch {
	case len(listErrs) > 0:
		// A list failure is reported even when other kinds listed fine and their
		// orphans were removed: the sweep is not a complete answer to "did anything
		// leak?", and a caller that treats a partial enumeration as a clean bill of
		// health is the failure this error exists to prevent.
		return sweep, fmt.Errorf("%w on cluster %q namespace %q: %w",
			ErrReapFailed, r.inj.Name(), namespace, errors.Join(listErrs...))
	case len(sweep.Failed) > 0:
		errs := make([]error, 0, len(sweep.Failed))
		for _, f := range sweep.Failed {
			errs = append(errs, f.Err)
		}
		return sweep, fmt.Errorf("%w: %d of %d orphan(s) on cluster %q namespace %q: %w",
			ErrReapIncomplete, len(sweep.Failed), len(sweep.Failed)+len(sweep.Reaped),
			r.inj.Name(), namespace, errors.Join(errs...))
	}
	return sweep, nil
}

// list enumerates one kind's objects in a namespace, asking the server to filter on
// MaKlaude's ownership labels.
//
// The selector is an optimisation, not the ownership test — see
// [Reaper.ownershipProblem] for why every returned object is re-checked locally.
// It is sent anyway because on a cluster where a human runs many experiments of
// their own, filtering server-side is the difference between one page and several.
func (r *Reaper) list(ctx context.Context, kind Kind, namespace string) ([]unstructured.Unstructured, error) {
	client, err := r.inj.clientForScope(kube.WriteScope{}, kind, namespace)
	if err != nil {
		return nil, err
	}

	selector := labels.Set(ownershipLabels).String()
	list, err := client.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("listing %s in %q with selector %q: %w", kind, namespace, selector, err)
	}
	return list.Items, nil
}

// consider decides what to do with one listed object and records the outcome on the
// sweep.
func (r *Reaper) consider(ctx context.Context, sweep *Sweep, kind Kind, namespace string, obj unstructured.Unstructured) {
	name := obj.GetName()

	if problem := r.ownershipProblem(kind, &obj); problem != "" {
		sweep.Skipped = append(sweep.Skipped, Skipped{
			Kind: kind, Namespace: namespace, Name: name, Reason: problem,
		})
		return
	}

	created := obj.GetCreationTimestamp().Time
	if created.IsZero() {
		// An unknown age is not an old age. A real API server always stamps this, so
		// reaching here means something between MaKlaude and the object dropped it —
		// and guessing "old enough" from a missing field is how a live experiment gets
		// deleted. Skipping is safe: the object is still there on the next sweep.
		sweep.Skipped = append(sweep.Skipped, Skipped{
			Kind: kind, Namespace: namespace, Name: name,
			Reason: "no creation timestamp, so its age cannot be established",
		})
		return
	}

	age := r.now().Sub(created)
	if age < r.grace {
		sweep.Skipped = append(sweep.Skipped, Skipped{
			Kind: kind, Namespace: namespace, Name: name, Age: age,
			Reason: fmt.Sprintf("age %s is under the %s orphan grace, so it may still be a live experiment",
				age.Round(time.Second), r.grace),
		})
		return
	}

	uid := string(obj.GetUID())
	removal, err := r.inj.Remove(ctx, Injected{
		Kind:      kind,
		Namespace: namespace,
		Name:      name,
		UID:       uid,
	})
	if err != nil {
		sweep.Failed = append(sweep.Failed, ReapFailure{
			Kind: kind, Namespace: namespace, Name: name, UID: uid, Err: err,
		})
		return
	}
	sweep.Reaped = append(sweep.Reaped, *removal)
}

// ownershipLabels are the labels [Experiment.object] stamps that identify an object
// as MaKlaude's chaos, and the ones the list selector asks the server to filter on.
//
// They deliberately exclude app.kubernetes.io/name, which the injector also sets:
// "managed-by=maklaude" plus "component=chaos" is the pair that says who created it
// and in which role, and a third redundant term in the selector would suggest the
// selector is the ownership test. It is not.
var ownershipLabels = map[string]string{
	"app.kubernetes.io/managed-by": "maklaude",
	"app.kubernetes.io/component":  "chaos",
}

// ownershipProblem reports why an object is not MaKlaude's chaos experiment on this
// cluster, or "" if it is.
//
// Three independent signals, all required, chosen so that no single mistake by
// anyone else produces a match:
//
//  1. The ownership LABELS, re-checked here rather than trusted from the server-side
//     selector. The selector is a filter; a filter that is ignored — by a proxy, by a
//     stub, by a server-side bug, by a caller that one day passes a different one —
//     returns MORE than was asked for, and code that trusts it deletes whatever came
//     back. Re-checking costs a map lookup and removes the whole class.
//  2. The cluster ANNOTATION, which must name this reaper's cluster. Labels are
//     copied when a human clones a manifest to try it themselves; the annotation
//     names the cluster the experiment was authorised for, so a CR carried over from
//     another cluster's MaKlaude is not swept here.
//  3. The NAME SHAPE. MaKlaude's object names are derived — a fixed prefix, the
//     lowercased kind, and a digest of the experiment's own fields (see
//     [Experiment.ObjectName]) — so no hand-written or server-generated name can
//     match. This is the signal a human cannot accidentally reproduce by copying
//     metadata, which is exactly what makes it worth having alongside (1) and (2).
//
// The reason strings quote label values, annotation values and names, all of which
// are operator-configured Kubernetes identifiers rather than object contents, so a
// sweep record is safe to log or attach to an escalation.
func (r *Reaper) ownershipProblem(kind Kind, obj *unstructured.Unstructured) string {
	objLabels := obj.GetLabels()
	for _, key := range sortedKeys(ownershipLabels) {
		if got := objLabels[key]; got != ownershipLabels[key] {
			return fmt.Sprintf("label %s is %q, not %q, so it was not created by MaKlaude's chaos path",
				key, got, ownershipLabels[key])
		}
	}

	const clusterKey = keyPrefix + "cluster"
	if got := obj.GetAnnotations()[clusterKey]; got != r.inj.Name() {
		return fmt.Sprintf("annotation %s is %q, not this cluster %q",
			clusterKey, got, r.inj.Name())
	}

	name := obj.GetName()
	if !derivedNameShape.MatchString(name) {
		return fmt.Sprintf("name %q is not a MaKlaude-derived experiment name", name)
	}
	if want := strings.ToLower(string(kind)); !strings.HasPrefix(name, "maklaude-"+want+"-") {
		return fmt.Sprintf("name %q does not carry the kind %s it was listed under", name, kind)
	}

	return ""
}

// sortedKeys returns the map's keys in stable order, so an ownership refusal names
// the same label every time rather than whichever one map iteration reached first.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
