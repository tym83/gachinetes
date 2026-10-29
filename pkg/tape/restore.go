package tape

import (
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// Step is one object of a restore, as it will be created.
type Step struct {
	Object *unstructured.Unstructured
	// OldUID is the UID the object had; owner references to it are
	// rewritten to the UID it gets.
	OldUID types.UID
	// DeletedAt is the revision that deleted it.
	DeletedAt int64
}

// Skipped is an object a restore leaves out, and why.
type Skipped struct {
	Key, Why string
}

// Plan orders the graves for a restore and cleans them for creation:
// namespaces and definitions first, owners before what they own. It
// leaves out what the cluster makes again by itself: objects a restored
// controller owns (pods of a ReplicaSet, ReplicaSets of a Deployment),
// events, leases, endpoints, service account tokens and the objects every
// namespace gets.
func Plan(graves []Object) ([]Step, []Skipped) {
	uids := map[types.UID]bool{}
	for _, g := range graves {
		if g.Object != nil {
			uids[(&unstructured.Unstructured{Object: g.Object}).GetUID()] = true
		}
	}
	var steps []Step
	var skipped []Skipped
	for _, g := range graves {
		if g.Object == nil {
			why := g.Error
			if why == "" {
				why = "the tape no longer holds what it was"
			}
			skipped = append(skipped, Skipped{g.Key, why})
			continue
		}
		u := (&unstructured.Unstructured{Object: g.Object}).DeepCopy()
		if why := regenerated(u, uids); why != "" {
			skipped = append(skipped, Skipped{g.Key, why})
			continue
		}
		steps = append(steps, Step{Object: u, OldUID: u.GetUID(), DeletedAt: g.DeletedAt})
	}
	depth := map[types.UID]int{}
	for i := range steps {
		depth[steps[i].OldUID] = 0
	}
	// An owner goes before what it owns: depth is the longest chain of
	// restored owners above an object.
	for changed := true; changed; {
		changed = false
		for _, s := range steps {
			for _, ref := range s.Object.GetOwnerReferences() {
				if d, ok := depth[ref.UID]; ok && depth[s.OldUID] < d+1 && d < len(steps) {
					depth[s.OldUID], changed = d+1, true
				}
			}
		}
	}
	rank := func(s Step) int {
		switch s.Object.GetKind() {
		case "Namespace":
			return 0
		case "CustomResourceDefinition":
			return 1
		}
		return 2
	}
	sort.SliceStable(steps, func(i, j int) bool {
		a, b := steps[i], steps[j]
		if rank(a) != rank(b) {
			return rank(a) < rank(b)
		}
		if depth[a.OldUID] != depth[b.OldUID] {
			return depth[a.OldUID] < depth[b.OldUID]
		}
		return a.DeletedAt < b.DeletedAt
	})
	for i := range steps {
		clean(steps[i].Object, uids)
	}
	return steps, skipped
}

// regenerated tells why the cluster makes an object again by itself, or "".
func regenerated(u *unstructured.Unstructured, restored map[types.UID]bool) string {
	gvk := u.GroupVersionKind()
	switch {
	case gvk.Kind == "Event":
		return "an event"
	case gvk.Kind == "Lease" && gvk.Group == "coordination.k8s.io":
		return "a lease"
	case gvk.Kind == "Endpoints" && gvk.Group == "", gvk.Kind == "EndpointSlice":
		return "endpoints follow their Service"
	case gvk.Kind == "Secret" && gvk.Group == "" && u.Object["type"] == "kubernetes.io/service-account-token":
		return "a service account token"
	case gvk.Kind == "ConfigMap" && u.GetName() == "kube-root-ca.crt":
		return "every namespace gets it"
	case gvk.Kind == "ServiceAccount" && u.GetName() == "default":
		return "every namespace gets it"
	}
	for _, ref := range u.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller && restored[ref.UID] {
			return "its controller " + ref.Kind + "/" + ref.Name + " is restored and makes it again"
		}
	}
	return ""
}

// clean drops what the API server sets and what ties the object to its
// old life: its UID, versions and times, references to owners not
// restored, a Service's cluster IPs.
func clean(u *unstructured.Unstructured, restored map[types.UID]bool) {
	md, _ := u.Object["metadata"].(map[string]any)
	for _, k := range []string{"uid", "resourceVersion", "creationTimestamp", "deletionTimestamp",
		"deletionGracePeriodSeconds", "generation", "managedFields", "selfLink"} {
		delete(md, k)
	}
	refs := u.GetOwnerReferences()
	kept := refs[:0]
	for _, ref := range refs {
		if restored[ref.UID] {
			kept = append(kept, ref)
		}
	}
	u.SetOwnerReferences(kept)
	delete(u.Object, "status")
	if u.GetKind() == "Service" && u.GroupVersionKind().Group == "" {
		if ip, _, _ := unstructured.NestedString(u.Object, "spec", "clusterIP"); ip != "None" {
			unstructured.RemoveNestedField(u.Object, "spec", "clusterIP")
			unstructured.RemoveNestedField(u.Object, "spec", "clusterIPs")
		}
	}
}

// Rewire points owner references at the UIDs restored owners got.
func Rewire(u *unstructured.Unstructured, newUIDs map[types.UID]types.UID) {
	refs := u.GetOwnerReferences()
	for i := range refs {
		if n, ok := newUIDs[refs[i].UID]; ok {
			refs[i].UID = n
		}
	}
	u.SetOwnerReferences(refs)
}
