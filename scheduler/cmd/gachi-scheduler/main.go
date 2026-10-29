// gachi-scheduler is kube-scheduler with the gym's rules: weight classes,
// tag teams and boys next door. It takes kube-scheduler's flags; given
// --gym-kubeconfig instead of a configuration, it writes one that enables
// the rules for that kubeconfig, as a distribution runs it in place of
// kube-scheduler.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/component-base/cli"
	"k8s.io/kubernetes/cmd/kube-scheduler/app"

	"github.com/tym83/gachinetes/scheduler/plugins"
)

// config enables the rules on top of the default profile.
const config = `apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection: {kubeconfig: %q}
leaderElection: {leaderElect: %s}
profiles:
- schedulerName: default-scheduler
  plugins:
    preFilter: {enabled: [{name: TagTeam}]}
    filter: {enabled: [{name: WeightClass}]}
    permit: {enabled: [{name: TagTeam}]}
    score: {enabled: [{name: BoyNextDoor, weight: 5}]}
`

func main() {
	args, err := gymArgs(os.Args[1:], os.TempDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cmd := app.NewSchedulerCommand(
		app.WithPlugin(plugins.WeightClassName, plugins.NewWeightClass),
		app.WithPlugin(plugins.TagTeamName, plugins.NewTagTeam),
		app.WithPlugin(plugins.BoyNextDoorName, plugins.NewBoyNextDoor),
	)
	cmd.SetArgs(args)
	os.Exit(cli.Run(cmd))
}

// gymArgs turns --gym-kubeconfig=PATH and --gym-leader-elect=BOOL into a
// written configuration and --config.
func gymArgs(in []string, dir string) ([]string, error) {
	var out []string
	kubeconfig, elect := "", "false"
	for _, a := range in {
		switch {
		case strings.HasPrefix(a, "--gym-kubeconfig="):
			kubeconfig = strings.TrimPrefix(a, "--gym-kubeconfig=")
		case strings.HasPrefix(a, "--gym-leader-elect="):
			elect = strings.TrimPrefix(a, "--gym-leader-elect=")
		default:
			out = append(out, a)
		}
	}
	if kubeconfig == "" {
		return out, nil
	}
	path := filepath.Join(dir, "gachi-scheduler.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(config, kubeconfig, elect)), 0o600); err != nil {
		return nil, err
	}
	return append(out, "--config="+path), nil
}
