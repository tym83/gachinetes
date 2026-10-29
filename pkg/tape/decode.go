package tape

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/client-go/kubernetes/scheme"
)

// ErrEncrypted is a value the API server encrypted before storing it.
var ErrEncrypted = errors.New("encrypted by the API server")

var (
	protoMagic   = []byte("k8s\x00")
	encMagic     = []byte("k8s:enc:")
	protoDecoder = protobuf.NewSerializer(scheme.Scheme, scheme.Scheme)
)

// Decode turns a stored value into an object: built-in types are stored
// as protobuf, custom resources as JSON.
func Decode(value []byte) (*unstructured.Unstructured, error) {
	switch {
	case bytes.HasPrefix(value, encMagic):
		return nil, ErrEncrypted
	case bytes.HasPrefix(value, protoMagic):
		obj, gvk, err := protoDecoder.Decode(value, nil, nil)
		if err != nil {
			return nil, err
		}
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return nil, err
		}
		u := &unstructured.Unstructured{Object: m}
		u.SetGroupVersionKind(*gvk)
		return u, nil
	}
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal(value, &u.Object); err != nil {
		return nil, err
	}
	return u, nil
}

// Redact empties the data of a Secret, keeping its keys.
func Redact(u *unstructured.Unstructured) {
	if u.GetKind() != "Secret" || u.GroupVersionKind().Group != "" {
		return
	}
	for _, field := range []string{"data", "stringData"} {
		m, ok, _ := unstructured.NestedMap(u.Object, field)
		if !ok {
			continue
		}
		for k := range m {
			m[k] = ""
		}
		_ = unstructured.SetNestedMap(u.Object, m, field)
	}
}

// Resource is the resource a key belongs to, as the API server lays keys
// out: /registry/<resource>/... for most built-in types,
// /registry/<group>/<resource>/... for custom resources.
func Resource(key string) string {
	parts := strings.Split(strings.TrimPrefix(key, "/registry/"), "/")
	if len(parts) > 1 && strings.Contains(parts[0], ".") {
		return parts[1] + "." + parts[0]
	}
	return parts[0]
}
