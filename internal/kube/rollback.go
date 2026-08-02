package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// podTemplateHashLabel is the label the Deployment controller adds to every
// ReplicaSet it creates, and to that ReplicaSet's pod template, to distinguish one
// revision's pods from another's. Its value is derived from the template itself, so
// it must be REMOVED from a template being restored: writing it back into the
// Deployment's own template would pin the Deployment to one revision's hash and make
// every subsequent rollout's hash disagree with it. `kubectl rollout undo` strips the
// same label for the same reason.
const podTemplateHashLabel = "pod-template-hash"

// deploymentRevisionAnnotation holds a ReplicaSet's revision number. It is the same
// annotation the health collector reads to report revisions, restated here rather
// than shared because the two layers must be able to disagree loudly: if the
// collector ever changed how it identified a revision, a rollback that silently
// followed it would restore a different template than the one a human approved.
const deploymentRevisionAnnotation = "deployment.kubernetes.io/revision"

// podTemplatePointer is the JSON pointer a revision rollback replaces. Replacing the
// whole template — rather than merging into it — is the entire reason this operation
// needs a JSON patch; see [Executor.PatchDeploymentJSON].
const podTemplatePointer = "/spec/template"

// RollbackDeploymentToRevision restores the pod template of one numbered revision of
// a Deployment, conditioned on resourceVersion. It is the API-level equivalent of
// `kubectl rollout undo --to-revision`.
//
// # Why this is one method rather than a patch the caller composes
//
// The other primitives take a body the caller built. This one cannot: the body IS the
// target revision's pod template, which only the cluster knows. So the composition
// lives here, next to the guard that bounds it, and the caller supplies the one thing
// a cluster read must not be trusted to decide — WHICH revision, taken from what a
// human approved.
//
// # It reads before it writes, under a scope that cannot write
//
// Finding the template takes two reads (the Deployment, for its identity and
// selector; then its ReplicaSets). They run on a clientset built from the ZERO
// [WriteScope], which admits every read verb and refuses every mutating one, so the
// reads cannot become a write by way of a bug in the code that composes the patch.
// The patch itself then goes through [Executor.PatchDeploymentJSON] and its
// single-request scope, unchanged.
//
// # What it refuses
//
// A revision whose ReplicaSet has been pruned yields [ErrRevisionNotFound] with
// nothing sent — the drift a caller should re-propose against rather than escalate. A
// revision number that is not positive, an invalid target, or a missing
// resourceVersion are refused before any read happens at all.
func (e *Executor) RollbackDeploymentToRevision(ctx context.Context, namespace, name string, revision int64, resourceVersion string) (*Outcome, error) {
	if err := validateNamespacedTarget(namespace, name); err != nil {
		return nil, err
	}
	if revision <= 0 {
		return nil, fmt.Errorf("%w: %d is not a deployment revision (revisions start at 1)", ErrInvalidTarget, revision)
	}
	// Checked here as well as in the patch path, so an action that could never be
	// conditioned does not first read a cluster.
	if strings.TrimSpace(resourceVersion) == "" {
		return nil, fmt.Errorf("%w: rolling back deployment %s/%s to revision %d",
			ErrMissingPrecondition, namespace, name, revision)
	}

	template, err := e.podTemplateOfRevision(ctx, namespace, name, revision)
	if err != nil {
		return nil, err
	}
	ops, err := replaceTemplateOps(template)
	if err != nil {
		return nil, err
	}
	return e.PatchDeploymentJSON(ctx, namespace, name, ops, resourceVersion)
}

// podTemplateOfRevision resolves the pod template a numbered revision was running.
//
// The ReplicaSet it reads is identified by the Deployment's OWNER REFERENCE — the
// controller UID — and not by the "<deployment>-<hash>" naming convention. A
// mutating action must not guess its target from a string: a name-based match would
// happily accept a ReplicaSet that merely looks related, and the template it carries
// becomes the Deployment's spec.
func (e *Executor) podTemplateOfRevision(ctx context.Context, namespace, name string, revision int64) (*corev1.PodTemplateSpec, error) {
	// The zero WriteScope is inert: reads pass through it and every mutating request
	// is refused (see WriteScope). So this half of the operation holds no authority to
	// change anything, whatever the code below does with the objects it reads.
	cs, err := e.clientsetForScope(WriteScope{})
	if err != nil {
		return nil, err
	}

	dep, err := cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: reading deployment %s/%s on cluster %q to resolve revision %d: %w",
			ErrExecute, namespace, name, e.Name(), revision, err)
	}

	rs, err := replicaSetForRevision(ctx, cs, dep, revision)
	if err != nil {
		return nil, err
	}

	// DeepCopy first: the label deletion below must not reach into the object the
	// client returned, which a caller may still be holding.
	template := rs.Spec.Template.DeepCopy()
	delete(template.Labels, podTemplateHashLabel)
	return template, nil
}

// replicaSetForRevision finds the Deployment's own ReplicaSet carrying the given
// revision.
//
// The list is narrowed by the Deployment's selector server-side and then filtered by
// controller UID here, because neither check subsumes the other: the selector keeps
// the response small on a busy namespace, and the UID is what makes the match exact
// (a hand-made ReplicaSet can carry any labels it likes).
func replicaSetForRevision(ctx context.Context, cs kubernetes.Interface, dep *appsv1.Deployment, revision int64) (*appsv1.ReplicaSet, error) {
	selector, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("%w: deployment %s/%s has an unusable selector: %w",
			ErrExecute, dep.Namespace, dep.Name, err)
	}

	list, err := cs.AppsV1().ReplicaSets(dep.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, fmt.Errorf("%w: listing replicasets of deployment %s/%s to resolve revision %d: %w",
			ErrExecute, dep.Namespace, dep.Name, revision, err)
	}

	var surviving []string
	for i := range list.Items {
		rs := &list.Items[i]
		if !ownedBy(rs.OwnerReferences, dep.UID) {
			continue
		}
		got, ok := revisionOf(rs.Annotations)
		if !ok {
			continue
		}
		if got == revision {
			return rs, nil
		}
		surviving = append(surviving, strconv.FormatInt(got, 10))
	}

	return nil, fmt.Errorf("%w: revision %d of deployment %s/%s has no surviving replicaset (surviving revisions: %s)",
		ErrRevisionNotFound, revision, dep.Namespace, dep.Name, renderSurviving(surviving))
}

// ownedBy reports whether the CONTROLLER owner reference points at uid. Only the
// controller reference counts: a plain owner reference expresses a lifecycle link,
// while the controller reference is the one that says which Deployment's rollouts
// this ReplicaSet represents.
func ownedBy(owners []metav1.OwnerReference, uid types.UID) bool {
	for i := range owners {
		if owners[i].Controller != nil && *owners[i].Controller && owners[i].UID == uid {
			return true
		}
	}
	return false
}

// revisionOf reads a ReplicaSet's revision annotation, reporting false for an absent
// or unparseable one rather than defaulting to a number. A ReplicaSet whose revision
// cannot be read is not a candidate for a rollback target: silently treating it as
// revision 0 — or as the requested one — would restore a template nobody asked for.
func revisionOf(annotations map[string]string) (int64, bool) {
	raw, ok := annotations[deploymentRevisionAnnotation]
	if !ok {
		return 0, false
	}
	revision, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, false
	}
	if revision <= 0 {
		return 0, false
	}
	return revision, true
}

// renderSurviving formats the revisions that DO exist for a not-found error, saying
// "none" explicitly rather than leaving an empty list a reader has to interpret. The
// list is what turns "revision 3 is gone" into something a human can act on: it says
// whether the history was pruned or the revision never existed.
func renderSurviving(revisions []string) string {
	if len(revisions) == 0 {
		return "none"
	}
	return strings.Join(revisions, ", ")
}

// replaceTemplateOps renders the one-operation JSON patch a rollback consists of: a
// `replace` of the Deployment's whole pod template.
//
// `replace` rather than `add` or a merge is the point of the operation. RFC 6902
// replace substitutes the value at a pointer outright, so a container or an
// environment variable the current revision introduced is GONE afterwards — which is
// exactly what a strategic-merge patch cannot express, and why restoring a revision
// needs this primitive rather than the merge one.
func replaceTemplateOps(template *corev1.PodTemplateSpec) ([]byte, error) {
	value, err := json.Marshal(template)
	if err != nil {
		return nil, fmt.Errorf("%w: encoding the target revision's pod template: %w", ErrInvalidPatch, err)
	}

	ops := []struct {
		Op    string          `json:"op"`
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	}{{Op: "replace", Path: podTemplatePointer, Value: value}}

	body, err := json.Marshal(ops)
	if err != nil {
		return nil, fmt.Errorf("%w: encoding the rollback patch: %w", ErrInvalidPatch, err)
	}
	return body, nil
}
