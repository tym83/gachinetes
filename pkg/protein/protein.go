// Package protein keeps capacity warm: placeholders that hold room for
// heavy pods until real ones push them out, and images pulled onto nodes
// before anything needs them.
package protein

import (
	"encoding/json"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
)

const (
	// PriorityClass is the placeholders' class: below every workload, so
	// that any pod pushes them out, and never pushing anything itself.
	PriorityClass = "gachinetes-protein"
	Priority      = -10
	// Pause holds a placeholder's room doing nothing.
	Pause = "registry.k8s.io/pause:3.10"
	// Sleeper carries a static sleep the image holders run, so that any
	// image, with a shell or without, can be kept on a node.
	Sleeper = "busybox:1.37-uclibc"
	// Label marks what a shake made.
	Label = "gym.gachinetes.dev/protein"
)

// Shake is a ProteinShake's spec.
type Shake struct {
	Name string `json:"-"`
	// Reserve holds Count placeholders of a size.
	Reserve *Reserve `json:"reserve,omitempty"`
	// Images are pulled onto every node NodeSelector picks.
	Images       []string          `json:"images,omitempty"`
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Tolerations let the placeholders and image holders onto tainted
	// pools, as the pods they keep room for are let.
	Tolerations []v1.Toleration `json:"tolerations,omitempty"`
}

// Reserve is room held for pods to come.
type Reserve struct {
	Count  int32  `json:"count"`
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	GPU    int64  `json:"gpu,omitempty"`
}

// From reads a ProteinShake object.
func From(u *unstructured.Unstructured) (Shake, error) {
	var s Shake
	raw, err := json.Marshal(u.Object["spec"])
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("ProteinShake %s: %w", u.GetName(), err)
	}
	s.Name = u.GetName()
	return s, nil
}

// PriorityClassObject is the placeholders' priority class.
func PriorityClassObject() *schedulingv1.PriorityClass {
	return &schedulingv1.PriorityClass{
		ObjectMeta:       metav1.ObjectMeta{Name: PriorityClass},
		Value:            Priority,
		PreemptionPolicy: ptr.To(v1.PreemptNever),
		Description:      "Gachinetes protein: placeholders any pod pushes out.",
	}
}

func labels(shake, part string) map[string]string {
	return map[string]string{Label: shake, Label + "-part": part}
}

// Placeholders is the Deployment that holds the reserve, in namespace.
func (s Shake) Placeholders(namespace string) (*appsv1.Deployment, error) {
	if s.Reserve == nil || s.Reserve.Count < 0 {
		return nil, nil
	}
	req := v1.ResourceList{}
	for name, v := range map[v1.ResourceName]string{v1.ResourceCPU: s.Reserve.CPU, v1.ResourceMemory: s.Reserve.Memory} {
		if v == "" {
			continue
		}
		q, err := resource.ParseQuantity(v)
		if err != nil {
			return nil, fmt.Errorf("ProteinShake %s: %s %q: %w", s.Name, name, v, err)
		}
		req[name] = q
	}
	if s.Reserve.GPU > 0 {
		req["nvidia.com/gpu"] = *resource.NewQuantity(s.Reserve.GPU, resource.DecimalSI)
	}
	l := labels(s.Name, "reserve")
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "protein-" + s.Name, Namespace: namespace, Labels: l},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(s.Reserve.Count),
			Selector: &metav1.LabelSelector{MatchLabels: l},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: l},
				Spec: v1.PodSpec{
					PriorityClassName:             PriorityClass,
					TerminationGracePeriodSeconds: ptr.To(int64(0)),
					NodeSelector:                  s.NodeSelector,
					Tolerations:                   append([]v1.Toleration{{Key: "nvidia.com/gpu", Operator: v1.TolerationOpExists}}, s.Tolerations...),
					AutomountServiceAccountToken:  ptr.To(false),
					Containers: []v1.Container{{
						Name: "protein", Image: Pause,
						Resources:       v1.ResourceRequirements{Requests: req, Limits: gpuOnly(req)},
						SecurityContext: restricted(),
					}},
				},
			},
		},
	}, nil
}

// gpuOnly: extended resources need their limits set too.
func gpuOnly(req v1.ResourceList) v1.ResourceList {
	if q, ok := req["nvidia.com/gpu"]; ok {
		return v1.ResourceList{"nvidia.com/gpu": q}
	}
	return nil
}

func restricted() *v1.SecurityContext {
	return &v1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false), RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)),
		Capabilities:   &v1.Capabilities{Drop: []v1.Capability{"ALL"}},
		SeccompProfile: &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault},
	}
}

// Holders is the DaemonSet that keeps the images on the nodes: each image
// runs as a container sleeping with a static sleep, copied in first.
func (s Shake) Holders(namespace string) *appsv1.DaemonSet {
	if len(s.Images) == 0 {
		return nil
	}
	l := labels(s.Name, "images")
	vol := v1.VolumeMount{Name: "sleeper", MountPath: "/protein"}
	small := v1.ResourceRequirements{Requests: v1.ResourceList{
		v1.ResourceCPU: resource.MustParse("1m"), v1.ResourceMemory: resource.MustParse("4Mi")}}
	var containers []v1.Container
	for i, img := range s.Images {
		containers = append(containers, v1.Container{
			Name: "image-" + strconv.Itoa(i), Image: img, ImagePullPolicy: v1.PullIfNotPresent,
			Command: []string{"/protein/sleep", "infinity"}, VolumeMounts: []v1.VolumeMount{vol},
			Resources: small, SecurityContext: restricted(),
		})
	}
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "protein-" + s.Name + "-images", Namespace: namespace, Labels: l},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: l},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: l},
				Spec: v1.PodSpec{
					PriorityClassName:             PriorityClass,
					TerminationGracePeriodSeconds: ptr.To(int64(0)),
					NodeSelector:                  s.NodeSelector,
					Tolerations:                   []v1.Toleration{{Operator: v1.TolerationOpExists}},
					AutomountServiceAccountToken:  ptr.To(false),
					Volumes:                       []v1.Volume{{Name: "sleeper", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}}},
					InitContainers: []v1.Container{{
						Name: "sleeper", Image: Sleeper,
						Command:      []string{"cp", "/bin/sleep", "/protein/sleep"},
						VolumeMounts: []v1.VolumeMount{vol}, Resources: small, SecurityContext: restricted(),
					}},
					Containers: containers,
				},
			},
		},
	}
}
