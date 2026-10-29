package protein

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestShake(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "gpu"},
		"spec": map[string]any{
			"reserve":      map[string]any{"count": int64(2), "cpu": "3", "memory": "8Gi", "gpu": int64(1)},
			"images":       []any{"vllm/vllm-openai:v0.11.0", "nvcr.io/nvidia/pytorch:25.09-py3"},
			"nodeSelector": map[string]any{"nvidia.com/gpu.present": "true"},
		},
	}}
	s, err := From(u)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Placeholders("gachinetes")
	if err != nil {
		t.Fatal(err)
	}
	c := d.Spec.Template.Spec.Containers[0]
	if *d.Spec.Replicas != 2 || d.Spec.Template.Spec.PriorityClassName != PriorityClass || c.Resources.Requests.Cpu().String() != "3" {
		t.Fatalf("placeholders: %+v", d.Spec)
	}
	if g := c.Resources.Limits["nvidia.com/gpu"]; g.Value() != 1 {
		t.Fatalf("gpu limit: %v", c.Resources.Limits)
	}
	ds := s.Holders("gachinetes")
	if len(ds.Spec.Template.Spec.Containers) != 2 || ds.Spec.Template.Spec.Containers[1].Command[0] != "/protein/sleep" {
		t.Fatalf("holders: %+v", ds.Spec.Template.Spec.Containers)
	}
	if ds.Spec.Template.Spec.NodeSelector["nvidia.com/gpu.present"] != "true" {
		t.Fatal("holders ignore the node selector")
	}
	none := Shake{Name: "x"}
	if d, _ := none.Placeholders("ns"); d != nil || none.Holders("ns") != nil {
		t.Fatal("an empty shake makes things")
	}
}
