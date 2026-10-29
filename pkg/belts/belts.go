// Package belts holds fields for their owners. A BeltPolicy names the
// fields of matching objects and who holds each; a write of a held field
// by anyone else is a challenge, which the policy refuses or lets through
// with a warning.
package belts

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tym83/gachinetes/pkg/wrestling"
)

const (
	Group   = "gym.gachinetes.dev"
	Version = "v1"
)

// Action is what happens to a challenge.
type Action string

const (
	Deny Action = "Deny"
	Warn Action = "Warn"
)

// Policy is a BeltPolicy's spec, with its name.
type Policy struct {
	Name  string `json:"-"`
	Match Match  `json:"match"`
	Belts []Belt `json:"belts"`
}

// Match selects objects: by group and kind, and optionally namespaces
// and names.
type Match struct {
	Group      string   `json:"group,omitempty"`
	Kind       string   `json:"kind"`
	Namespaces []string `json:"namespaces,omitempty"`
	Names      []string `json:"names,omitempty"`
}

// Belt is a field and who holds it. Path is dotted, such as
// spec.replicas; it covers the fields under it.
type Belt struct {
	Path    string  `json:"path"`
	Holders Holders `json:"holders"`
	// Action: Deny (the default) or Warn.
	Action Action `json:"action,omitempty"`
}

// Holders are who may write a field: field managers, users (service
// accounts as system:serviceaccount:<ns>:<name>) or groups.
type Holders struct {
	Managers []string `json:"managers,omitempty"`
	Users    []string `json:"users,omitempty"`
	Groups   []string `json:"groups,omitempty"`
}

// FromObject reads a BeltPolicy object.
func FromObject(u *unstructured.Unstructured) (Policy, error) {
	var p Policy
	spec, ok := u.Object["spec"]
	if !ok {
		return p, fmt.Errorf("BeltPolicy %s has no spec", u.GetName())
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("BeltPolicy %s: %w", u.GetName(), err)
	}
	p.Name = u.GetName()
	return p, nil
}

// Write is a write as admission sees it.
type Write struct {
	Group, Kind, Namespace, Name string
	// Manager is the field manager the request names; empty when it named
	// none.
	Manager string
	User    string
	Groups  []string
	Old     map[string]any
	New     map[string]any
}

// Verdict is the referee's call on a write.
type Verdict struct {
	Allowed  bool
	Message  string
	Warnings []string
}

// Judge checks a write against every policy that matches it.
func Judge(policies []Policy, w Write) Verdict {
	v := Verdict{Allowed: true}
	if w.Old == nil {
		// A new object takes no belt from anyone.
		return v
	}
	changed := wrestling.Changed(w.Old, w.New)
	for _, p := range policies {
		if !p.Match.matches(w) {
			continue
		}
		for _, b := range p.Belts {
			path := touched(b.Path, changed)
			if path == "" || b.Holders.hold(w) {
				continue
			}
			msg := fmt.Sprintf("♂ %s of %s %s belongs to %s (BeltPolicy %s); %s challenged it",
				path, w.Kind, w.Name, b.Holders, p.Name, who(w))
			if b.Action == Warn {
				v.Warnings = append(v.Warnings, msg)
				continue
			}
			v.Allowed = false
			v.Message = msg + ". Ask the holder, or change the policy."
			return v
		}
	}
	return v
}

func (m Match) matches(w Write) bool {
	if m.Group != w.Group || m.Kind != w.Kind {
		return false
	}
	if len(m.Namespaces) > 0 && !slices.Contains(m.Namespaces, w.Namespace) {
		return false
	}
	return len(m.Names) == 0 || slices.Contains(m.Names, w.Name)
}

// touched is the first changed path the belt covers, or "".
func touched(belt string, changed []string) string {
	for _, c := range changed {
		if c == belt || strings.HasPrefix(c, belt+".") || strings.HasPrefix(belt, c+".") {
			return c
		}
	}
	return ""
}

func (h Holders) hold(w Write) bool {
	if w.Manager != "" && slices.Contains(h.Managers, w.Manager) {
		return true
	}
	if slices.Contains(h.Users, w.User) {
		return true
	}
	for _, g := range w.Groups {
		if slices.Contains(h.Groups, g) {
			return true
		}
	}
	return false
}

func (h Holders) String() string {
	var parts []string
	for _, m := range h.Managers {
		parts = append(parts, "manager "+m)
	}
	for _, u := range h.Users {
		parts = append(parts, u)
	}
	for _, g := range h.Groups {
		parts = append(parts, "group "+g)
	}
	if len(parts) == 0 {
		return "nobody"
	}
	return strings.Join(parts, ", ")
}

func who(w Write) string {
	if w.Manager != "" {
		return w.User + " (manager " + w.Manager + ")"
	}
	return w.User
}
