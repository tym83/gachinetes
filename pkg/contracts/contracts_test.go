package contracts

import (
	"strings"
	"testing"
	"time"
)

func TestJudge(t *testing.T) {
	now := time.Date(2026, 10, 29, 14, 0, 0, 0, time.UTC)
	c := Contract{Name: "fix-prod", Spec: Spec{User: "alice", ClusterRole: "admin", Namespace: "prod", Duration: "1h"}}
	if s, _ := Judge(c, 1, now); s != Active {
		t.Fatalf("a self-service contract: %s", s)
	}
	c.Activated, c.Epoch = now.Add(-2*time.Hour), 1
	if s, _ := Judge(c, 1, now); s != Expired {
		t.Fatalf("after its time: %s", s)
	}
	c.Activated = now.Add(-time.Minute)
	if s, msg := Judge(c, 2, now); s != Revoked || !strings.Contains(msg, "safeword") {
		t.Fatalf("after a safeword: %s %s", s, msg)
	}
	spotted := Contract{Name: "x", Spec: Spec{User: "bob", ClusterRole: "cluster-admin", Duration: "30m", Spotter: true}}
	if s, msg := Judge(spotted, 1, now); s != Pending || !strings.Contains(msg, "gachictl spot") {
		t.Fatalf("without its spotter: %s %s", s, msg)
	}
	spotted.ApprovedBy = "carol"
	if s, _ := Judge(spotted, 1, now); s != Active {
		t.Fatalf("spotted: %s", s)
	}
	for _, bad := range []Spec{
		{ClusterRole: "admin", Duration: "1h"},
		{User: "a", Group: "b", ClusterRole: "admin", Duration: "1h"},
		{User: "a", Duration: "1h"},
		{User: "a", ClusterRole: "admin", Duration: "48h"},
	} {
		if s, _ := Judge(Contract{Spec: bad}, 1, now); s != Rejected {
			t.Errorf("%+v: %s", bad, s)
		}
	}
}

func TestSafewordLeavesExpiredContracts(t *testing.T) {
	now := time.Now()
	c := Contract{Name: "old", Spec: Spec{User: "alice", ClusterRole: "view", Duration: "10m"}, Phase: string(Expired), Epoch: 1}
	if s, _ := Judge(c, 2, now); s != Expired {
		t.Fatalf("an expired contract after the safeword: %s", s)
	}
	c.Phase = string(Active)
	if s, _ := Judge(c, 2, now); s != Revoked {
		t.Fatalf("an active contract after the safeword: %s", s)
	}
}
