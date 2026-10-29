package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestWithBaseImages(t *testing.T) {
	spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "count", Image: "busybox:1.37"}, {Name: "side", Image: "nginx:1.27"}}}
	withBaseImages(&spec, []corev1.ContainerStatus{{Name: "count", ImageID: "docker.io/library/busybox@sha256:abc"}})
	if len(spec.Volumes) != 2 {
		t.Fatalf("volumes: %+v", spec.Volumes)
	}
	if got := spec.Volumes[0].Image.Reference; got != "docker.io/library/busybox@sha256:abc" {
		t.Errorf("the digest that ran: %s", got)
	}
	if got := spec.Volumes[1].Image.Reference; got != "nginx:1.27" {
		t.Errorf("no digest, the spec's image: %s", got)
	}
	// Moved again, from a checkpoint image: the volumes stay as they were.
	spec.Containers[0].Image = "localhost:5001/gachinetes/checkpoint/default/counter/count:1"
	withBaseImages(&spec, []corev1.ContainerStatus{{Name: "count", ImageID: "localhost:5001/gachinetes/checkpoint/default/counter/count@sha256:def"}})
	if len(spec.Volumes) != 2 || spec.Volumes[0].Image.Reference != "docker.io/library/busybox@sha256:abc" {
		t.Errorf("moved again: %+v", spec.Volumes)
	}
}
