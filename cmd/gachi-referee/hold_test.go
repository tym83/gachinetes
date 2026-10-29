package main

import (
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tym83/gachinetes/pkg/belts"
)

func TestReviewRefusesAChallenger(t *testing.T) {
	h := &holder{policies: []belts.Policy{{
		Name:  "replicas",
		Match: belts.Match{Group: "apps", Kind: "Deployment"},
		Belts: []belts.Belt{{Path: "spec.replicas", Holders: belts.Holders{Managers: []string{"hpa"}}}},
	}}}
	req := func(manager string) *admissionv1.AdmissionRequest {
		return &admissionv1.AdmissionRequest{
			UID:       "u",
			Kind:      metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
			Name:      "web",
			Namespace: "shop",
			Operation: admissionv1.Update,
			UserInfo:  authv1.UserInfo{Username: "alice"},
			OldObject: runtime.RawExtension{Raw: []byte(`{"spec":{"replicas":2}}`)},
			Object:    runtime.RawExtension{Raw: []byte(`{"spec":{"replicas":7}}`)},
			Options:   runtime.RawExtension{Raw: []byte(`{"kind":"UpdateOptions","fieldManager":"` + manager + `"}`)},
		}
	}
	if r := h.review(req("hpa")); !r.Allowed {
		t.Fatalf("holder refused: %+v", r.Result)
	}
	r := h.review(req("kubectl-edit"))
	if r.Allowed || r.Result == nil || r.Result.Code != 403 || r.UID != "u" {
		t.Fatalf("challenger let through: %+v", r)
	}
}
