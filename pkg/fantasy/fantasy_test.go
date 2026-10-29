package fantasy

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/tym83/gachinetes/pkg/tape"
)

func TestPlantable(t *testing.T) {
	for key, want := range map[string]bool{
		"/registry/configmaps/a/b":                                 true,
		"/registry/events/a/b":                                     false,
		"/registry/leases/kube-system/x":                           false,
		"/registry/masterleases/10.0.0.1":                          false,
		"/registry/apiregistration.k8s.io/apiservices/v1.tape.x.y": false,
	} {
		if got := Plantable(key, []byte("k8s\x00")); got != want {
			t.Errorf("%s: %v", key, got)
		}
	}
	if Plantable("/registry/secrets/a/b", []byte("k8s:enc:aescbc:v1:k:xx")) {
		t.Error("an encrypted value")
	}
}

func pod(name, node string) *tape.Object {
	spec := map[string]any{}
	if node != "" {
		spec["nodeName"] = node
	}
	return &tape.Object{Key: "/registry/pods/shop/" + name, Object: map[string]any{
		"kind": "Pod", "metadata": map[string]any{"name": name, "namespace": "shop"}, "spec": spec}}
}

func TestSummarize(t *testing.T) {
	old := pod("a", "n1")
	moved := pod("a", "n2")
	moved.Object["metadata"].(map[string]any)["resourceVersion"] = "9"
	d := tape.Diff{
		Added:   []tape.Change{{Key: "/registry/pods/shop/b", To: pod("b", "n1")}, {Key: "/registry/pods/shop/c", To: pod("c", "")}, {Key: "/registry/leases/x/y", To: pod("lease", "")}},
		Removed: []tape.Change{{Key: "/registry/pods/shop/d", From: pod("d", "n2")}},
		Changed: []tape.Change{{Key: old.Key, From: old, To: moved}},
	}
	r := Summarize(d)
	if len(r.Added) != 2 || len(r.Pending) != 1 || r.Pending[0].Name != "c" || len(r.Removed) != 1 || len(r.Changed) != 1 {
		t.Fatalf("report: %+v", r)
	}
	if f := r.Changed[0].Fields; len(f) != 1 || f[0] != "spec.nodeName" {
		t.Fatalf("fields: %v", f)
	}
	text := r.Text()
	for _, want := range []string{"+ Pod shop/b → n1", "! Pod shop/c", "− Pod shop/d", "[spec.nodeName]"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
}

func TestMakePKI(t *testing.T) {
	dir := t.TempDir()
	if err := makePKI(dir+"/pki", dir+"/kube", 23004); err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", dir+"/kube/super-admin.conf")
	if err != nil || cfg.Host != "https://127.0.0.1:23004" {
		t.Fatalf("kubeconfig: %v %v", cfg, err)
	}
	raw, _ := os.ReadFile(dir + "/pki/apiserver.crt")
	block, _ := pem.Decode(raw)
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil || len(c.IPAddresses) == 0 || !c.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("apiserver certificate: %v", err)
	}
}
