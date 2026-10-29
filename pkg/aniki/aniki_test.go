package aniki

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func obj(t *testing.T, y string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(y), &u.Object); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestReview(t *testing.T) {
	sloppy := obj(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: web}
spec:
  template:
    spec:
      volumes: [{name: logs, hostPath: {path: /var/log}}]
      containers:
      - name: app
        image: nginx
        securityContext: {privileged: true}
`)
	got := Review(sloppy)
	text := ""
	for _, f := range got {
		text += f.String() + "\n"
	}
	for _, want := range []string{"one-man gym", "host path", "right version", "without a plan", "no spotter", "leather", "ropes"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	if len(Warnings(got)) != 4 {
		t.Errorf("serious: %v", Warnings(got))
	}
	tidy := obj(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: web}
spec:
  replicas: 3
  template:
    spec:
      topologySpreadConstraints: [{maxSkew: 1, topologyKey: kubernetes.io/hostname, whenUnsatisfiable: ScheduleAnyway}]
      containers:
      - name: app
        image: registry.example.org:5000/web:1.2.3
        resources: {requests: {cpu: 100m}}
        readinessProbe: {httpGet: {path: /, port: 80}}
        securityContext: {allowPrivilegeEscalation: false}
`)
	if got := Review(tidy); len(got) != 0 {
		t.Errorf("a tidy deployment: %v", got)
	}
	if got := Review(obj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n")); got != nil {
		t.Errorf("a config map: %v", got)
	}
}
