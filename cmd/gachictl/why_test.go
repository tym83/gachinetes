package main

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tym83/gachinetes/pkg/tape"
)

func TestExplainAPendingPod(t *testing.T) {
	pod := &unstructured.Unstructured{Object: map[string]any{
		"kind": "Pod",
		"status": map[string]any{
			"phase":      "Pending",
			"conditions": []any{map[string]any{"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": "0/3 nodes are available"}},
			"containerStatuses": []any{map[string]any{"name": "web",
				"state":     map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff", "message": "back-off"}},
				"lastState": map[string]any{"terminated": map[string]any{"reason": "Error", "exitCode": int64(1)}}}},
		},
	}}
	ev := corev1.Event{Type: corev1.EventTypeWarning, Reason: "FailedScheduling", Message: "0/3 nodes are available", Count: 4,
		LastTimestamp: metav1.Now()}
	got := strings.Join(explain(pod, []corev1.Event{ev, ev}), "\n")
	for _, want := range []string{"PodScheduled=False Unschedulable", "waits: CrashLoopBackOff", "exit code 1", "FailedScheduling", "(×4)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Count(got, "FailedScheduling") != 1 {
		t.Errorf("a warning told twice:\n%s", got)
	}
}

func TestCauseInHistory(t *testing.T) {
	at := time.Date(2026, 10, 29, 13, 0, 0, 0, time.UTC)
	ver := func(rev int64, sec int, image, manager string) tape.Object {
		u := &unstructured.Unstructured{Object: map[string]any{
			"kind": "Deployment", "metadata": map[string]any{"name": "web"},
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"image": image}}},
		}}
		u.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: manager, Operation: "Update", Time: &metav1.Time{Time: at.Add(time.Duration(sec) * time.Second)}}})
		return tape.Object{Revision: rev, Time: at.Add(time.Duration(sec) * time.Second).UnixMilli(), Object: u.Object}
	}
	status := ver(21, 0, "nginx:2", "kubectl-edit")
	status.Object["status"] = map[string]any{"observedGeneration": int64(2)}
	history := []tape.Object{ver(10, -60, "nginx:1", "kubectl-client-side-apply"), ver(20, -1, "nginx:2", "kubectl-edit"), status, ver(30, 60, "nginx:3", "argocd")}
	got := causeIn(history, at)
	if !strings.Contains(got, "spec.template.spec.image changed by kubectl-edit") || !strings.Contains(got, "revision 20") {
		t.Fatalf("cause: %q", got)
	}
	if got := causeIn(history[:1], at); got != "" {
		t.Fatalf("no earlier version: %q", got)
	}
}
