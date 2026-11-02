package main

import (
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
	"k8s.io/cloud-provider/app"
	"k8s.io/cloud-provider/names"
	"k8s.io/cloud-provider/options"
	controllerapp "k8s.io/controller-manager/app"
	controllerconfig "k8s.io/controller-manager/config"
)

func TestControllerExclusions(t *testing.T) {
	for _, value := range []string{"*,-route,-cloud-node-lifecycle", "*,-node-route-controller,-cloud-node-lifecycle-controller"} {
		t.Run(value, func(t *testing.T) {
			all, aliases := controllerConfiguration()
			opts, err := options.NewCloudControllerManagerOptions()
			require.NoError(t, err)
			sets := opts.Flags(all, app.ControllersDisabledByDefault.List(), aliases, nil, nil)
			flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
			for _, set := range sets.FlagSets {
				flags.AddFlagSet(set)
			}
			require.NoError(t, flags.Parse([]string{"--controllers=" + value}))
			require.Empty(t, opts.Generic.Validate(all, app.ControllersDisabledByDefault.List(), aliases))
			cfg := &controllerconfig.GenericControllerManagerConfiguration{}
			require.NoError(t, opts.Generic.ApplyTo(cfg, all, app.ControllersDisabledByDefault.List(), aliases))
			for _, name := range []string{names.CloudNodeLifecycleController, names.NodeRouteController} {
				require.False(t, controllerapp.IsControllerEnabled(name, app.ControllersDisabledByDefault, cfg.Controllers), name)
			}
			for _, name := range []string{names.CloudNodeController, names.ServiceLBController} {
				require.True(t, controllerapp.IsControllerEnabled(name, app.ControllersDisabledByDefault, cfg.Controllers), name)
			}
		})
	}
}
