package aidiagnose

import (
	"fmt"
	"strings"

	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
)

// systemPrompt is the static task instruction and OUTPUT CONTRACT handed to the
// model on every call. It is provider-agnostic (the [ClaudeProvider] parser and
// [buildRequest] share it) and contains no cluster data. It pins the model to the
// one job this layer allows — proposing better ROOT-CAUSE EXPLANATIONS — and
// forbids it from proposing actions, reinforcing the read-only boundary in the
// prompt itself. The strict JSON contract is what lets a [Provider] parse the
// reply back into [Suggestion]s deterministically.
const systemPrompt = `You are a senior Kubernetes SRE assisting an automated, READ-ONLY diagnosis system.
You are given a correlated incident: a set of findings and the deterministic root-cause hypotheses the system already produced.
Your ONLY job is to propose better or additional ROOT-CAUSE EXPLANATIONS for cases the deterministic rules handled poorly.

Hard rules:
- You are read-only. NEVER propose, describe, or imply any command, mutation, remediation, or action on the cluster. Explanations only.
- Only use the evidence provided. Do not invent object names, values, or facts that are not present.
- If the deterministic hypotheses already explain the incident well, return an empty suggestions list.
- The evidence has been redacted; treat "[REDACTED]" as an intentionally removed secret and never speculate about its value.

Respond with STRICT JSON and nothing else, in exactly this shape:
{"suggestions":[{"cause":"<short lowercase slug, no spaces>","title":"<short label>","message":"<one-paragraph explanation>","confidence":"low|medium|high"}]}
If you have no refinement, respond with: {"suggestions":[]}`

// refinePurpose is the fixed, human-readable "why" recorded in every audit record
// (see [AuditRecord.Purpose]). It states plainly what the model is consulted for,
// so the trail is self-explanatory.
const refinePurpose = "refine root-cause hypotheses"

// evidence-shaping caps. They bound how much of the (already redacted) incident is
// described to the model, complementing the byte cap: without per-item caps a
// pathological incident could pack the whole byte budget with one giant field.
const (
	maxEvidenceFindings   = 12
	maxEvidenceHypotheses = 6
	maxEvidencePods       = 8
	maxEvidenceEvents     = 8
	maxFieldChars         = 400
)

// buildRequest assembles the redacted, cost-bounded [Request] for one incident.
// It is the EGRESS BOUNDARY of the T5 layer: it gathers the incident's findings,
// the deterministic hypotheses, and the relevant per-pod and event signals from
// the snapshot into a plain-text description, then runs the WHOLE assembled
// payload through [Redact] before size-capping it. Redaction happens here, last,
// so nothing sensitive can reach a [Provider] regardless of which field it came
// from — including free-text messages the collector captured verbatim.
//
// The snapshot and incident are read but never mutated. The returned request
// carries only text (system instruction + redacted, truncated evidence) and the
// response-token cap.
func buildRequest(cfg Config, snap health.Snapshot, incident correlate.Incident, base []diagnose.Hypothesis) Request {
	var b strings.Builder

	fmt.Fprintf(&b, "Cluster: %s\n", incident.Cluster)
	fmt.Fprintf(&b, "Incident primary: %s — %s (severity %s)\n\n",
		incident.Primary.Object.String(), incident.Primary.Title, incident.Severity())

	b.WriteString("Findings in this incident:\n")
	for i, f := range incident.Findings() {
		if i >= maxEvidenceFindings {
			break
		}
		fmt.Fprintf(&b, "- [%s] %s: %s — %s\n",
			f.Severity, f.Object.String(), f.Title, clip(f.Message))
	}

	b.WriteString("\nDeterministic hypotheses already produced:\n")
	for i, h := range base {
		if i >= maxEvidenceHypotheses {
			break
		}
		fmt.Fprintf(&b, "- cause=%s confidence=%s: %s — %s\n",
			h.Cause, h.Confidence, h.Title, clip(h.Message))
	}

	writePodSignals(&b, snap, incident)
	writeEventSignals(&b, snap, incident)

	// The single, authoritative egress step: redact everything, THEN size-cap.
	// Redacting before truncation means a secret straddling the cap can never be
	// left half-exposed by the cut.
	evidence := truncateRunes(Redact(b.String()), cfg.maxEvidenceBytes())

	return Request{
		System:    systemPrompt,
		Evidence:  evidence,
		MaxTokens: cfg.maxResponseTokens(),
	}
}

// writePodSignals appends the container-level facts (waiting reasons/messages and
// termination reasons) for the incident's pods — the free-text fields most likely
// to both help the model and, before redaction, carry a leaked secret.
func writePodSignals(b *strings.Builder, snap health.Snapshot, incident correlate.Incident) {
	pods := incidentPods(snap, incident)
	if len(pods) == 0 {
		return
	}
	b.WriteString("\nPod container details:\n")
	for i := range pods {
		if i >= maxEvidencePods {
			break
		}
		p := pods[i]
		fmt.Fprintf(b, "- pod %s/%s phase=%s restarts=%d\n", p.Namespace, p.Name, p.Phase, p.RestartCount)
		for j := range p.Containers {
			c := p.Containers[j]
			if c.WaitingReason != "" {
				fmt.Fprintf(b, "    container %q waiting: %s — %s\n", c.Name, c.WaitingReason, clip(c.WaitingMessage))
			}
			if c.CurrentTermination != nil {
				fmt.Fprintf(b, "    container %q terminated: reason=%s exit=%d\n", c.Name, c.CurrentTermination.Reason, c.CurrentTermination.ExitCode)
			} else if c.LastTermination != nil {
				fmt.Fprintf(b, "    container %q last-terminated: reason=%s exit=%d\n", c.Name, c.LastTermination.Reason, c.LastTermination.ExitCode)
			}
		}
	}
}

// writeEventSignals appends the recent warning-event messages for the incident's
// objects. Event messages are cluster narration captured verbatim, so they are a
// prime redaction target as well as useful diagnostic context.
func writeEventSignals(b *strings.Builder, snap health.Snapshot, incident correlate.Incident) {
	names := incidentObjectNames(incident)
	var written int
	for i := range snap.WarningEvents {
		e := snap.WarningEvents[i]
		if !names[strings.ToLower(e.InvolvedObject)] {
			continue
		}
		if written == 0 {
			b.WriteString("\nRecent warning events:\n")
		}
		fmt.Fprintf(b, "- %s on %s: %s\n", e.Reason, e.InvolvedObject, clip(e.Message))
		written++
		if written >= maxEvidenceEvents {
			break
		}
	}
}

// incidentPods returns the [health.PodSignal]s for the pod findings in the
// incident, in the incident's own stable finding order. It reads the snapshot
// read-only.
func incidentPods(snap health.Snapshot, incident correlate.Incident) []health.PodSignal {
	byKey := make(map[string]health.PodSignal, len(snap.Pods))
	for i := range snap.Pods {
		byKey[snap.Pods[i].Namespace+"/"+snap.Pods[i].Name] = snap.Pods[i]
	}
	var out []health.PodSignal
	for _, f := range incident.Findings() {
		if f.Object.Kind != "pod" {
			continue
		}
		if p, ok := byKey[f.Object.Namespace+"/"+f.Object.Name]; ok {
			out = append(out, p)
		}
	}
	return out
}

// incidentObjectNames returns the set of lowercase "kind/namespace/name" keys for
// the incident's findings, matching the form [health.EventSignal.InvolvedObject]
// is stored under, so events can be attributed to the incident's objects.
func incidentObjectNames(incident correlate.Incident) map[string]bool {
	out := make(map[string]bool)
	for _, f := range incident.Findings() {
		o := f.Object
		key := o.Kind + "/"
		if o.Namespace != "" {
			key += o.Namespace + "/"
		}
		key += o.Name
		out[strings.ToLower(key)] = true
	}
	return out
}

// clip bounds a single free-text field so no one message can dominate the
// evidence budget. It cuts at a rune boundary and marks the elision.
func clip(s string) string {
	return truncateRunes(strings.TrimSpace(s), maxFieldChars)
}

// truncateRunes returns s bounded to at most limit runes, appending an elision
// marker when it had to cut. It cuts on rune boundaries so multibyte text is never
// split mid-character. A non-positive limit returns s unchanged.
func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + " …[truncated]"
}
