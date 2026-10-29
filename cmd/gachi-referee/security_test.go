package main

import (
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// guardWith answers SubjectAccessReviews: who may bind which role.
func guardWith(grants map[string]bool) *guard {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sar := a.(k8stesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview)
		key := sar.Spec.User + ":" + sar.Spec.ResourceAttributes.Verb + ":" + sar.Spec.ResourceAttributes.Name
		sar.Status.Allowed = grants[key]
		return true, sar, nil
	})
	return &guard{cs: cs}
}

func contractRaw(requested, approved string, spotter bool) []byte {
	ann := map[string]string{"gym.gachinetes.dev/requested-by": requested}
	if approved != "" {
		ann["gym.gachinetes.dev/approved-by"] = approved
	}
	b, _ := json.Marshal(map[string]any{
		"apiVersion": "gym.gachinetes.dev/v1", "kind": "Contract",
		"metadata": map[string]any{"name": "fix", "annotations": ann},
		"spec":     map[string]any{"user": requested, "clusterRole": "cluster-admin", "duration": "1h", "spotter": spotter},
	})
	return b
}

func review(g *guard, op admissionv1.Operation, user string, old, cur []byte) *admissionv1.AdmissionResponse {
	return g.review(&admissionv1.AdmissionRequest{
		UID: "u", Operation: op, Kind: metav1.GroupVersionKind{Group: "gym.gachinetes.dev", Version: "v1", Kind: "Contract"},
		UserInfo: authnv1.UserInfo{Username: user}, OldObject: runtime.RawExtension{Raw: old}, Object: runtime.RawExtension{Raw: cur},
	})
}

func TestContracts(t *testing.T) {
	g := guardWith(map[string]bool{"root:bind:cluster-admin": true, "carol:bind:cluster-admin": true})
	if r := review(g, admissionv1.Create, "alice", nil, contractRaw("bob", "", true)); r.Allowed {
		t.Fatal("a contract asked in someone else's name")
	}
	if r := review(g, admissionv1.Create, "alice", nil, contractRaw("alice", "", false)); r.Allowed {
		t.Fatal("self-service for a role alice may not grant")
	}
	if r := review(g, admissionv1.Create, "root", nil, contractRaw("root", "", false)); !r.Allowed {
		t.Fatalf("self-service for who may grant: %v", r.Result)
	}
	asked := contractRaw("alice", "", true)
	if r := review(g, admissionv1.Create, "alice", nil, asked); !r.Allowed {
		t.Fatalf("asking with a spotter: %v", r.Result)
	}
	if r := review(g, admissionv1.Update, "alice", asked, contractRaw("alice", "alice", true)); r.Allowed {
		t.Fatal("alice spotted herself")
	}
	if r := review(g, admissionv1.Update, "dave", asked, contractRaw("alice", "dave", true)); r.Allowed {
		t.Fatal("a spotter who may not grant the role")
	}
	if r := review(g, admissionv1.Update, "carol", asked, contractRaw("alice", "carol", true)); !r.Allowed {
		t.Fatalf("carol spots: %v", r.Result)
	}
	if r := review(g, admissionv1.Update, "carol", asked, contractRaw("alice", "dave", true)); r.Allowed {
		t.Fatal("carol approved in dave's name")
	}
	changed := contractRaw("alice", "", false)
	if r := review(g, admissionv1.Update, "alice", asked, changed); r.Allowed {
		t.Fatal("the terms changed")
	}
}

func TestSpottedDeleteWithoutSpot(t *testing.T) {
	g := guardWith(nil)
	g.dyn = fakeDynamic()
	ns, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": "prod", "labels": map[string]string{SpottedLabel: "true"}}})
	r := g.review(&admissionv1.AdmissionRequest{UID: "u", Operation: admissionv1.Delete, Name: "prod",
		Kind: metav1.GroupVersionKind{Version: "v1", Kind: "Namespace"}, Resource: metav1.GroupVersionResource{Version: "v1", Resource: "namespaces"},
		UserInfo: authnv1.UserInfo{Username: "alice"}, OldObject: runtime.RawExtension{Raw: ns}})
	if r.Allowed {
		t.Fatal("a spotted namespace deleted without a spot")
	}
}

func fakeDynamic() *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		spotsGVR: "SpotList", contractsGVR: "ContractList", lockersGVR: "LockerList",
	})
}
