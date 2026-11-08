package main

import (
	"strings"
	"testing"
	"time"

	"k8s.io/cli-runtime/pkg/genericclioptions"

	"github.com/tym83/gachinetes/pkg/fantasy"
)

func TestFantasyCommands(t *testing.T) {
	root := rootCommand(&app{flags: genericclioptions.NewConfigFlags(true)})
	for _, sub := range []string{"preview", "discard", "merge", "list"} {
		if c, _, err := root.Find([]string{"fantasy", sub}); err != nil || c.Name() != sub {
			t.Errorf("fantasy %s: %v", sub, err)
		}
	}
	if c, _, err := root.Find([]string{"tape", "restore"}); err != nil || c.Name() != "restore" {
		t.Errorf("tape restore: %v", err)
	}
}

func TestPreviewMarkdown(t *testing.T) {
	st := fantasy.PreviewStatus{Name: "pr-shop-42", Status: fantasy.StatusReady, ReportText: "+ Deployment shop/web\n",
		Endpoint: "https://pr-shop-42.gachi-fantasies.svc:6443", Expires: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
	md := previewMarkdown(st, fantasy.PreviewNamespace)
	for _, want := range []string{"`pr-shop-42`", "+ Deployment shop/web", "port-forward svc/pr-shop-42 16443:6443", "2026-11-11T00:00:00Z"} {
		if !strings.Contains(md, want) {
			t.Errorf("no %q in\n%s", want, md)
		}
	}
	failed := previewMarkdown(fantasy.PreviewStatus{Name: "pr-1", Status: fantasy.StatusFailed, Error: "not quiet"}, "ns")
	if !strings.Contains(failed, "failed") || !strings.Contains(failed, "not quiet") {
		t.Errorf("failed: %s", failed)
	}
}
