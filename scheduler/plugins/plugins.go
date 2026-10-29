// Package plugins are the gym's rules for the scheduler: weight classes,
// tag teams, and boys next door.
package plugins

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
)

// Labels and annotations the rules read.
const (
	WeightLabel   = "gym.gachinetes.dev/weight"
	SuperHeavy    = "super-heavyweight"
	TeamLabel     = "gym.gachinetes.dev/team"
	TeamSizeAnno  = "gym.gachinetes.dev/team-size"
	TalksToAnno   = "gym.gachinetes.dev/talks-to"
	teamWaitLimit = time.Minute
)

// --- Weight classes ---

// WeightClassName is the name of the weight class rule.
const WeightClassName = "WeightClass"

// WeightClass keeps a super-heavyweight alone on its node: no pod but
// those of DaemonSets shares it.
type WeightClass struct{}

var _ fwk.FilterPlugin = &WeightClass{}

func NewWeightClass(_ context.Context, _ runtime.Object, _ fwk.Handle) (fwk.Plugin, error) {
	return &WeightClass{}, nil
}

func (*WeightClass) Name() string { return WeightClassName }

func (*WeightClass) Filter(_ context.Context, _ fwk.CycleState, pod *v1.Pod, node fwk.NodeInfo) *fwk.Status {
	heavy := superHeavy(pod)
	for _, pi := range node.GetPods() {
		p := pi.GetPod()
		if p.UID == pod.UID || daemon(p) {
			continue
		}
		if heavy {
			return fwk.NewStatus(fwk.Unschedulable, fmt.Sprintf("♂ a super-heavyweight fights alone; %s/%s is on the node", p.Namespace, p.Name))
		}
		if superHeavy(p) {
			return fwk.NewStatus(fwk.Unschedulable, fmt.Sprintf("♂ the node belongs to super-heavyweight %s/%s", p.Namespace, p.Name))
		}
	}
	return nil
}

func superHeavy(p *v1.Pod) bool { return p.Labels[WeightLabel] == SuperHeavy }

func daemon(p *v1.Pod) bool {
	for _, o := range p.OwnerReferences {
		if o.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

// Weight is a pod's weight class: super-heavyweight when it says so, else
// by its QoS: heavyweight (Guaranteed), middleweight (Burstable) or
// flyweight (BestEffort).
func Weight(p *v1.Pod) string {
	if superHeavy(p) {
		return SuperHeavy
	}
	requests, limits, guaranteed := false, false, true
	for _, c := range p.Spec.Containers {
		if len(c.Resources.Requests) > 0 {
			requests = true
		}
		if len(c.Resources.Limits) > 0 {
			limits = true
		}
		for _, r := range []v1.ResourceName{v1.ResourceCPU, v1.ResourceMemory} {
			l, ok := c.Resources.Limits[r]
			q, qok := c.Resources.Requests[r]
			if !ok || (qok && q.Cmp(l) != 0) {
				guaranteed = false
			}
		}
	}
	switch {
	case !requests && !limits:
		return "flyweight"
	case guaranteed:
		return "heavyweight"
	}
	return "middleweight"
}

// --- Tag teams ---

// TagTeamName is the name of the tag team rule.
const TagTeamName = "TagTeam"

// TagTeam lets a team into the ring all at once or not at all: the pods
// with one team label in a namespace wait at the ropes until as many as
// the team's size have found places, then all go in together.
type TagTeam struct {
	h fwk.Handle
}

var (
	_ fwk.PreFilterPlugin   = &TagTeam{}
	_ fwk.PermitPlugin      = &TagTeam{}
	_ fwk.EnqueueExtensions = &TagTeam{}
)

// EventsToRegister brings a waiting member back to the queue as soon as
// a teammate comes into the gym, rather than at the next periodic retry.
func (*TagTeam) EventsToRegister(context.Context) ([]fwk.ClusterEventWithHint, error) {
	return []fwk.ClusterEventWithHint{{
		Event: fwk.ClusterEvent{Resource: fwk.Pod, ActionType: fwk.Add},
		QueueingHintFn: func(_ klog.Logger, pod *v1.Pod, _, newObj interface{}) (fwk.QueueingHint, error) {
			added, ok := newObj.(*v1.Pod)
			name, _, member := team(pod)
			if ok && member && added.Namespace == pod.Namespace && added.Labels[TeamLabel] == name {
				return fwk.Queue, nil
			}
			return fwk.QueueSkip, nil
		},
	}}, nil
}

func NewTagTeam(_ context.Context, _ runtime.Object, h fwk.Handle) (fwk.Plugin, error) {
	return &TagTeam{h: h}, nil
}

func (*TagTeam) Name() string { return TagTeamName }

// team is a pod's team and its size; ok is false for a pod on its own.
func team(p *v1.Pod) (name string, size int, ok bool) {
	name = p.Labels[TeamLabel]
	if name == "" {
		return "", 0, false
	}
	size, err := strconv.Atoi(p.Annotations[TeamSizeAnno])
	if err != nil || size < 1 {
		size = 1
	}
	return name, size, true
}

func (t *TagTeam) PreFilter(_ context.Context, _ fwk.CycleState, pod *v1.Pod, _ []fwk.NodeInfo) (*fwk.PreFilterResult, *fwk.Status) {
	name, size, ok := team(pod)
	if !ok {
		return nil, nil
	}
	members, err := t.h.SharedInformerFactory().Core().V1().Pods().Lister().Pods(pod.Namespace).
		List(labels.SelectorFromSet(labels.Set{TeamLabel: name}))
	if err != nil {
		return nil, fwk.AsStatus(err)
	}
	if len(members) < size {
		return nil, fwk.NewStatus(fwk.UnschedulableAndUnresolvable,
			fmt.Sprintf("♂ tag team %s: %d of %d in the gym", name, len(members), size))
	}
	return nil, nil
}

func (*TagTeam) PreFilterExtensions() fwk.PreFilterExtensions { return nil }

func (t *TagTeam) Permit(_ context.Context, _ fwk.CycleState, pod *v1.Pod, _ string) (*fwk.Status, time.Duration) {
	name, size, ok := team(pod)
	if !ok || size == 1 {
		return nil, 0
	}
	// Members that found a place already: bound, assumed, or waiting.
	placed := 1
	if infos, err := t.h.SnapshotSharedLister().NodeInfos().List(); err == nil {
		for _, ni := range infos {
			for _, pi := range ni.GetPods() {
				p := pi.GetPod()
				if p.UID != pod.UID && p.Namespace == pod.Namespace && p.Labels[TeamLabel] == name {
					placed++
				}
			}
		}
	}
	var waiting []fwk.WaitingPod
	t.h.IterateOverWaitingPods(func(w fwk.WaitingPod) {
		p := w.GetPod()
		if p.UID != pod.UID && p.Namespace == pod.Namespace && p.Labels[TeamLabel] == name {
			waiting = append(waiting, w)
		}
	})
	// Waiting members are assumed, so they are among those placed.
	if placed < size {
		return fwk.NewStatus(fwk.Wait, fmt.Sprintf("♂ tag team %s: %d of %d at the ropes", name, placed, size)), teamWaitLimit
	}
	for _, w := range waiting {
		w.Allow(TagTeamName)
	}
	return nil, 0
}

// --- Boys next door ---

// BoyNextDoorName is the name of the boy next door rule.
const BoyNextDoorName = "BoyNextDoor"

// BoyNextDoor places a pod next to the pods it talks to: the annotation
// gym.gachinetes.dev/talks-to holds label selectors of its peers in its
// namespace, separated by ";", and a node scores by how many it holds.
type BoyNextDoor struct{}

var (
	_ fwk.ScorePlugin = &BoyNextDoor{}
)

func NewBoyNextDoor(_ context.Context, _ runtime.Object, _ fwk.Handle) (fwk.Plugin, error) {
	return &BoyNextDoor{}, nil
}

func (*BoyNextDoor) Name() string { return BoyNextDoorName }

// Peers are the selectors a pod talks to.
func Peers(p *v1.Pod) []labels.Selector {
	var out []labels.Selector
	for _, s := range strings.Split(p.Annotations[TalksToAnno], ";") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if sel, err := labels.Parse(s); err == nil {
			out = append(out, sel)
		}
	}
	return out
}

func (*BoyNextDoor) Score(_ context.Context, _ fwk.CycleState, pod *v1.Pod, node fwk.NodeInfo) (int64, *fwk.Status) {
	peers := Peers(pod)
	if len(peers) == 0 {
		return 0, nil
	}
	var n int64
	for _, pi := range node.GetPods() {
		p := pi.GetPod()
		if p.Namespace != pod.Namespace || p.UID == pod.UID {
			continue
		}
		for _, sel := range peers {
			if sel.Matches(labels.Set(p.Labels)) {
				n++
				break
			}
		}
	}
	return n, nil
}

func (b *BoyNextDoor) ScoreExtensions() fwk.ScoreExtensions { return b }

// NormalizeScore scales the counts of peers to the scheduler's range.
func (*BoyNextDoor) NormalizeScore(_ context.Context, _ fwk.CycleState, _ *v1.Pod, scores fwk.NodeScoreList) *fwk.Status {
	var top int64
	for _, s := range scores {
		top = max(top, s.Score)
	}
	if top == 0 {
		return nil
	}
	for i := range scores {
		scores[i].Score = scores[i].Score * fwk.MaxNodeScore / top
	}
	return nil
}
