package main

import (
	"os"
	"strings"
	"testing"
)

func TestGymArgs(t *testing.T) {
	dir := t.TempDir()
	args, err := gymArgs([]string{"--gym-kubeconfig=/etc/k/s.conf", "--gym-leader-elect=true", "--v=2"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 || args[0] != "--v=2" || args[1] != "--config="+dir+"/gachi-scheduler.yaml" {
		t.Fatalf("args: %v", args)
	}
	raw, _ := os.ReadFile(dir + "/gachi-scheduler.yaml")
	if !strings.Contains(string(raw), `kubeconfig: "/etc/k/s.conf"`) || !strings.Contains(string(raw), "leaderElect: true") {
		t.Fatalf("config:\n%s", raw)
	}
	if args, _ := gymArgs([]string{"--config=x"}, dir); len(args) != 1 {
		t.Fatalf("without gym flags: %v", args)
	}
}
