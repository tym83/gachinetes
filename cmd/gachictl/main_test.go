package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tym83/gachinetes/pkg/tape"
)

// gachictl's calls reach the tape's real handler: paths and parameters
// agree.
func TestCallsReachTheTape(t *testing.T) {
	thing := func(name string, deleting bool) []byte {
		md := `"name":"` + name + `","namespace":"gym","uid":"u-` + name + `"`
		if deleting {
			md += `,"deletionTimestamp":"2026-10-29T11:00:00Z"`
		}
		kind := "Thing"
		if name == "gym" {
			kind, md = "Namespace", strings.Replace(md, `"namespace":"gym",`, "", 1)
		}
		return []byte(`{"apiVersion":"v1","kind":"` + kind + `","metadata":{` + md + `}}`)
	}
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v any
		switch r.URL.Path {
		case "/tape/v1/graveyard":
			v = []tape.Grave{
				{Key: "/registry/namespaces/gym", DeletedAt: 30, Last: &tape.KV{Key: "/registry/namespaces/gym", Value: thing("gym", true), ModRevision: 20}},
				{Key: "/registry/x.io/things/gym/a", DeletedAt: 25, Last: &tape.KV{Key: "/registry/x.io/things/gym/a", Value: thing("a", false), ModRevision: 5}},
				{Key: "/registry/x.io/things/gym/old", DeletedAt: 3, Last: &tape.KV{Key: "/registry/x.io/things/gym/old", Value: thing("old", false), ModRevision: 2}},
			}
		case "/tape/v1/history":
			v = []tape.KV{
				{Key: "/registry/namespaces/gym", Value: thing("gym", false), ModRevision: 10},
				{Key: "/registry/namespaces/gym", Value: thing("gym", true), ModRevision: 20},
				{Key: "/registry/namespaces/gym", Deleted: true, ModRevision: 30},
			}
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}))
	defer store.Close()
	svc := httptest.NewServer(tape.Handler(&tape.Store{URL: store.URL}))
	defer svc.Close()
	a := &app{tapeURL: svc.URL}

	graves, err := a.gravesFor(context.Background(), target{kind: "Thing", name: "a", namespace: "gym"})
	if err != nil || len(graves) != 1 {
		t.Fatalf("one object: %+v %v", graves, err)
	}
	// A namespace brings back what was deleted since its deletion
	// started (revision 20), not what was deleted before.
	graves, err = a.gravesFor(context.Background(), target{kind: "Namespace", name: "gym", namespace: "gym"})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, g := range graves {
		keys = append(keys, g.Key)
	}
	if len(graves) != 2 || strings.Contains(strings.Join(keys, " "), "old") {
		t.Fatalf("namespace graves: %v", keys)
	}
}
