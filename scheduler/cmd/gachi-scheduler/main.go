// gachi-scheduler is kube-scheduler with the gym's rules: weight classes,
// tag teams and boys next door. It takes kube-scheduler's flags and a
// configuration that enables the rules (see deploy/scheduler.yaml).
package main

import (
	"os"

	"k8s.io/component-base/cli"
	"k8s.io/kubernetes/cmd/kube-scheduler/app"

	"github.com/tym83/gachinetes/scheduler/plugins"
)

func main() {
	cmd := app.NewSchedulerCommand(
		app.WithPlugin(plugins.WeightClassName, plugins.NewWeightClass),
		app.WithPlugin(plugins.TagTeamName, plugins.NewTagTeam),
		app.WithPlugin(plugins.BoyNextDoorName, plugins.NewBoyNextDoor),
	)
	os.Exit(cli.Run(cmd))
}
