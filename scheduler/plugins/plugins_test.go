package plugins

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

func pod(name string, labels, annotations map[string]string) *v1.Pod {
	return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "gym", UID: types.UID(name), Labels: labels, Annotations: annotations}}
}

func node(pods ...*v1.Pod) fwk.NodeInfo {
	ni := framework.NewNodeInfo(pods...)
	ni.SetNode(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}})
	return ni
}

func TestWeightClassKeepsASuperHeavyweightAlone(t *testing.T) {
	w := &WeightClass{}
	heavy := pod("heavy", map[string]string{WeightLabel: SuperHeavy}, nil)
	other := pod("other", nil, nil)
	ds := pod("ds", nil, nil)
	ds.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "agent"}}
	ctx := context.Background()
	if s := w.Filter(ctx, nil, heavy, node(ds)); !s.IsSuccess() {
		t.Fatalf("a node with a DaemonSet pod only: %v", s)
	}
	if s := w.Filter(ctx, nil, heavy, node(other)); s.IsSuccess() {
		t.Fatal("a super-heavyweight shares a node")
	}
	if s := w.Filter(ctx, nil, other, node(heavy)); s.IsSuccess() {
		t.Fatal("a pod joins a super-heavyweight")
	}
	if s := w.Filter(ctx, nil, other, node(ds)); !s.IsSuccess() {
		t.Fatalf("an ordinary pod: %v", s)
	}
}

func TestWeight(t *testing.T) {
	q := resource.MustParse
	c := func(req, lim v1.ResourceList) *v1.Pod {
		p := pod("p", nil, nil)
		p.Spec.Containers = []v1.Container{{Resources: v1.ResourceRequirements{Requests: req, Limits: lim}}}
		return p
	}
	both := v1.ResourceList{v1.ResourceCPU: q("1"), v1.ResourceMemory: q("1Gi")}
	for want, p := range map[string]*v1.Pod{
		"flyweight":    c(nil, nil),
		"middleweight": c(v1.ResourceList{v1.ResourceCPU: q("1")}, nil),
		"heavyweight":  c(both, both),
		SuperHeavy:     pod("s", map[string]string{WeightLabel: SuperHeavy}, nil),
	} {
		if got := Weight(p); got != want {
			t.Errorf("%s: %s", want, got)
		}
	}
}

func TestBoyNextDoorScoresPeers(t *testing.T) {
	b := &BoyNextDoor{}
	web := pod("web", nil, map[string]string{TalksToAnno: "app=api; app=db"})
	api := pod("api", map[string]string{"app": "api"}, nil)
	db := pod("db", map[string]string{"app": "db"}, nil)
	stranger := pod("x", map[string]string{"app": "cache"}, nil)
	ctx := context.Background()
	both, _ := b.Score(ctx, nil, web, node(api, db, stranger))
	one, _ := b.Score(ctx, nil, web, node(api))
	none, _ := b.Score(ctx, nil, web, node(stranger))
	if both != 2 || one != 1 || none != 0 {
		t.Fatalf("scores %d %d %d", both, one, none)
	}
	scores := fwk.NodeScoreList{{Name: "a", Score: 2}, {Name: "b", Score: 1}, {Name: "c", Score: 0}}
	_ = b.NormalizeScore(ctx, nil, web, scores)
	if scores[0].Score != fwk.MaxNodeScore || scores[1].Score != fwk.MaxNodeScore/2 || scores[2].Score != 0 {
		t.Fatalf("normalized %v", scores)
	}
}
