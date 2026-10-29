// Package contracts gives elevated access for a while and takes it back:
// a Contract binds a cluster role to a person for a time, a safeword ends
// every contract at once, and a spotter, a second person who may grant the
// role, signs for the dangerous ones.
package contracts

import (
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Annotations admission sets: who asked, and who approved.
const (
	RequestedBy = "gym.gachinetes.dev/requested-by"
	ApprovedBy  = "gym.gachinetes.dev/approved-by"
	// Label marks the bindings contracts make.
	Label = "gym.gachinetes.dev/contract"
)

// Spec is a Contract's spec.
type Spec struct {
	// User, Group or ServiceAccount (namespace/name) gets the role.
	User           string `json:"user,omitempty"`
	Group          string `json:"group,omitempty"`
	ServiceAccount string `json:"serviceAccount,omitempty"`
	ClusterRole    string `json:"clusterRole"`
	// Namespace: the role is bound there only; empty is the cluster.
	Namespace string `json:"namespace,omitempty"`
	Duration  string `json:"duration"`
	Reason    string `json:"reason,omitempty"`
	// Spotter: a second person must approve before the contract holds.
	Spotter bool `json:"spotter,omitempty"`
}

// Contract is a Contract object read.
type Contract struct {
	Name        string
	Spec        Spec
	RequestedBy string
	ApprovedBy  string
	Created     time.Time
	// Epoch is the trust epoch the contract was made in; Activated when
	// it began to hold.
	Epoch     int64
	Activated time.Time
	Phase     string
}

// From reads a Contract object.
func From(u *unstructured.Unstructured) (Contract, error) {
	c := Contract{Name: u.GetName(), Created: u.GetCreationTimestamp().Time}
	raw, err := json.Marshal(u.Object["spec"])
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c.Spec); err != nil {
		return c, fmt.Errorf("Contract %s: %w", u.GetName(), err)
	}
	a := u.GetAnnotations()
	c.RequestedBy, c.ApprovedBy = a[RequestedBy], a[ApprovedBy]
	c.Epoch, _, _ = unstructured.NestedInt64(u.Object, "status", "epoch")
	c.Phase, _, _ = unstructured.NestedString(u.Object, "status", "phase")
	if s, _, _ := unstructured.NestedString(u.Object, "status", "activated"); s != "" {
		c.Activated, _ = time.Parse(time.RFC3339, s)
	}
	return c, nil
}

// Validate checks a spec on its own.
func (s Spec) Validate() error {
	n := 0
	for _, v := range []string{s.User, s.Group, s.ServiceAccount} {
		if v != "" {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("a contract is with one user, group or service account")
	}
	if s.ClusterRole == "" {
		return fmt.Errorf("a contract names a cluster role")
	}
	d, err := time.ParseDuration(s.Duration)
	if err != nil || d <= 0 || d > 24*time.Hour {
		return fmt.Errorf("duration %q: up to 24h", s.Duration)
	}
	return nil
}

// State is what a contract should be, at now, in the current epoch.
type State string

const (
	Pending  State = "Pending"  // waits for its spotter
	Active   State = "Active"   // holds: its binding exists
	Expired  State = "Expired"  // its time is up
	Revoked  State = "Revoked"  // a safeword ended it
	Rejected State = "Rejected" // not valid
)

// Judge tells what a contract is: the binding exists only while Active.
func Judge(c Contract, epoch int64, now time.Time) (State, string) {
	if err := c.Spec.Validate(); err != nil {
		return Rejected, err.Error()
	}
	if c.Phase == string(Revoked) || (c.Epoch != 0 && c.Epoch < epoch) {
		return Revoked, "♂ safeword: the trust epoch moved on"
	}
	if c.Phase == string(Expired) {
		return Expired, "its time is up"
	}
	if c.Spec.Spotter && c.ApprovedBy == "" {
		return Pending, "waits for a spotter: gachictl spot contract/" + c.Name
	}
	start := c.Activated
	if start.IsZero() {
		start = now
	}
	d, _ := time.ParseDuration(c.Spec.Duration)
	if now.After(start.Add(d)) {
		return Expired, "its time is up"
	}
	return Active, fmt.Sprintf("holds until %s", start.Add(d).UTC().Format(time.RFC3339))
}
