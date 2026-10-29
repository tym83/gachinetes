// Package bucks prices what pods ask for: three hundred bucks. A
// PriceList says what a core, a GiB of memory and a GPU cost an hour; a
// Budget says what a namespace may spend a month. A pod that would take a
// namespace over its budget is refused.
package bucks

import (
	"encoding/json"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// HoursPerMonth is a month as clouds bill it.
const HoursPerMonth = 730

// GPULabel is the node label that names a GPU model, as NVIDIA's GPU
// feature discovery sets it.
const GPULabel = "nvidia.com/gpu.product"

// Prices are a PriceList's spec: per hour.
type Prices struct {
	Currency string  `json:"currency,omitempty"`
	CPU      float64 `json:"cpu"`
	MemoryGi float64 `json:"memoryGi"`
	// GPU is the price of a GPU of a model; "" is any other.
	GPU map[string]float64 `json:"gpu,omitempty"`
}

// PricesFrom reads a PriceList object.
func PricesFrom(u *unstructured.Unstructured) (Prices, error) {
	var p Prices
	raw, err := json.Marshal(u.Object["spec"])
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("PriceList %s: %w", u.GetName(), err)
	}
	if p.Currency == "" {
		p.Currency = "$"
	}
	return p, nil
}

// Hourly is what a pod costs an hour, by what it requests: the larger of
// its containers' requests and its init containers' (as the scheduler
// counts), and GPUs at the price of the model its node selector names.
func (p Prices) Hourly(pod *v1.Pod) float64 {
	req := requests(pod)
	cpu := req[v1.ResourceCPU]
	mem := req[v1.ResourceMemory]
	gpu := req["nvidia.com/gpu"]
	cost := float64(cpu.MilliValue())/1000*p.CPU + float64(mem.Value())/(1<<30)*p.MemoryGi
	if n := gpu.Value(); n > 0 {
		price, ok := p.GPU[pod.Spec.NodeSelector[GPULabel]]
		if !ok {
			price = p.GPU[""]
		}
		cost += float64(n) * price
	}
	return cost
}

func requests(pod *v1.Pod) v1.ResourceList {
	sum := v1.ResourceList{}
	for _, c := range pod.Spec.Containers {
		for r, q := range c.Resources.Requests {
			v := sum[r]
			v.Add(q)
			sum[r] = v
		}
	}
	for _, c := range pod.Spec.InitContainers {
		for r, q := range c.Resources.Requests {
			if v, ok := sum[r]; !ok || q.Cmp(v) > 0 {
				sum[r] = q.DeepCopy()
			}
		}
	}
	return sum
}

// Counts tells whether a pod counts against its namespace: until it is
// over.
func Counts(pod *v1.Pod) bool {
	return pod.Status.Phase != v1.PodSucceeded && pod.Status.Phase != v1.PodFailed && pod.DeletionTimestamp == nil
}

// Monthly is what pods cost a month.
func (p Prices) Monthly(pods []*v1.Pod) float64 {
	var h float64
	for _, pod := range pods {
		if Counts(pod) {
			h += p.Hourly(pod)
		}
	}
	return h * HoursPerMonth
}

// Budget is a Budget's spec.
type Budget struct {
	Monthly float64 `json:"monthly"`
}

// BudgetFrom reads a Budget object.
func BudgetFrom(u *unstructured.Unstructured) (Budget, error) {
	var b Budget
	raw, err := json.Marshal(u.Object["spec"])
	if err != nil {
		return b, err
	}
	return b, json.Unmarshal(raw, &b)
}

// Check tells whether a new pod fits a namespace's budget, given what the
// namespace's pods cost already; the message says why not.
func Check(p Prices, budget float64, existing []*v1.Pod, pod *v1.Pod) (bool, string) {
	now := p.Monthly(existing)
	with := now + p.Hourly(pod)*HoursPerMonth
	if with <= budget {
		return true, ""
	}
	return false, fmt.Sprintf("♂ three hundred bucks: %s would cost %s%.2f a month, taking namespace %s to %s%.2f of its %s%.2f budget",
		pod.GenerateName+pod.Name, p.Currency, p.Hourly(pod)*HoursPerMonth, pod.Namespace, p.Currency, with, p.Currency, budget)
}

// Quantity is a helper for tests and tools.
func Quantity(s string) resource.Quantity { return resource.MustParse(s) }
