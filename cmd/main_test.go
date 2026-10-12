package main

import (
	"slices"
	"testing"

	"k8s.io/cloud-provider/app"
	"k8s.io/cloud-provider/names"
	cmconfig "k8s.io/controller-manager/config"
	cmoptions "k8s.io/controller-manager/options"
)

func TestServiceControllerAliasIsCanonicalized(t *testing.T) {
	controllerOptions := cmoptions.NewGenericControllerManagerConfigurationOptions(&cmconfig.GenericControllerManagerConfiguration{})
	controllerOptions.Controllers = []string{"service"}
	controllerConfig := &cmconfig.GenericControllerManagerConfiguration{}

	controllerNames, aliases := controllerConfiguration()
	if err := controllerOptions.ApplyTo(
		controllerConfig,
		controllerNames,
		app.ControllersDisabledByDefault.List(),
		aliases,
	); err != nil {
		t.Fatalf("configure generic controller manager: %v", err)
	}

	if !slices.Contains(controllerConfig.Controllers, names.ServiceLBController) {
		t.Fatalf("service alias was not canonicalized: %v", controllerConfig.Controllers)
	}
}
