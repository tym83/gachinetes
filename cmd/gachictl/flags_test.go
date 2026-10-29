package main

import (
	"testing"

	"k8s.io/cli-runtime/pkg/genericclioptions"
)

// Every command's flags live with kubectl's without a clash: a clash
// panics at the first command line.
func TestNoFlagClash(t *testing.T) {
	root := rootCommand(&app{flags: genericclioptions.NewConfigFlags(true)})
	for _, c := range root.Commands() {
		root.SetArgs([]string{c.Name(), "--help"})
		if err := root.Execute(); err != nil {
			t.Errorf("%s: %v", c.Name(), err)
		}
	}
}
