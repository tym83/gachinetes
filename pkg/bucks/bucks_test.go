package bucks

import (
	"math"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
)

func pod(name, cpu, mem string, gpus int64, model string) *v1.Pod {
	req := v1.ResourceList{v1.ResourceCPU: Quantity(cpu), v1.ResourceMemory: Quantity(mem)}
	if gpus > 0 {
		q := Quantity("1")
		q.Set(gpus)
		req["nvidia.com/gpu"] = q
	}
	p := &v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{{Resources: v1.ResourceRequirements{Requests: req}}}}}
	p.Name, p.Namespace = name, "shop"
	if model != "" {
		p.Spec.NodeSelector = map[string]string{GPULabel: model}
	}
	return p
}

var prices = Prices{Currency: "$", CPU: 0.04, MemoryGi: 0.005, GPU: map[string]float64{"NVIDIA-H100": 2.5, "": 1}}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestHourly(t *testing.T) {
	if h := prices.Hourly(pod("a", "2", "4Gi", 0, "")); !near(h, 2*0.04+4*0.005) {
		t.Fatalf("cpu and memory: %v", h)
	}
	if h := prices.Hourly(pod("g", "1", "1Gi", 2, "NVIDIA-H100")); !near(h, 0.04+0.005+5) {
		t.Fatalf("two H100s: %v", h)
	}
	if h := prices.Hourly(pod("g", "1", "1Gi", 1, "")); !near(h, 0.04+0.005+1) {
		t.Fatalf("a GPU of no model: %v", h)
	}
}

func TestCheck(t *testing.T) {
	existing := []*v1.Pod{pod("a", "4", "8Gi", 0, ""), pod("done", "100", "1Gi", 0, "")}
	existing[1].Status.Phase = v1.PodSucceeded
	// 4 CPU + 8 GiB = 0.2 $/h = 146 $/month.
	if ok, _ := Check(prices, 300, existing, pod("b", "4", "8Gi", 0, "")); !ok {
		t.Fatal("292 of 300 refused")
	}
	ok, msg := Check(prices, 300, existing, pod("c", "8", "8Gi", 0, ""))
	if ok || !strings.Contains(msg, "three hundred bucks") || !strings.Contains(msg, "of its $300.00 budget") {
		t.Fatalf("over the budget: %v %s", ok, msg)
	}
}
