package main

import (
	goflag "flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"time"

	"ec-ccm/internal/edgecenter"
	"ec-ccm/internal/util/panicutil"
	"ec-ccm/internal/version"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server/healthz"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/cloud-provider/app"
	"k8s.io/cloud-provider/options"
	"k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
	_ "k8s.io/component-base/metrics/prometheus/restclient"
	_ "k8s.io/component-base/metrics/prometheus/version"
	"k8s.io/klog/v2"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func init() {
	mux := http.NewServeMux()
	healthz.InstallHandler(mux)
}

var versionFlag bool

func main() {
	defer panicutil.HandlePanic("main")

	rand.Seed(time.Now().UTC().UnixNano())

	controllerList := []string{"cloud-node", "cloud-node-lifecycle", "service", "route"}

	s, err := options.NewCloudControllerManagerOptions()
	if err != nil {
		klog.Fatalf("unable to initialize command options: %v", err)
	}
	s.KubeCloudShared.CloudProvider.Name = edgecenter.ProviderName

	fmt.Printf("Cloud Options %v\n", s)

	klogFlags := goflag.NewFlagSet("klog", goflag.ExitOnError)
	klog.InitFlags(klogFlags)
	pflag.CommandLine.AddGoFlagSet(klogFlags)

	command := &cobra.Command{
		Use: "edgecenterv2-cloud-controller-manager",
		Long: `The Cloud controller manager is a daemon that embeds
		the cloud specific control loops shipped with Kubernetes.`,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			cmd.Flags().VisitAll(func(f1 *pflag.Flag) {
				if f2 := goflag.CommandLine.Lookup(f1.Name); f2 != nil {
					_ = f2.Value.Set(f1.Value.String())
				}
			})

			verbosityFlag := klogFlags.Lookup("v")
			if verbosityFlag != nil {
				fmt.Printf("Klog verbosity set to: %s\n", verbosityFlag.Value.String())
			} else {
				fmt.Println("Klog verbosity flag not found")
			}
		},
		Run: func(cmd *cobra.Command, args []string) {
			defer panicutil.HandlePanic("command.Run")

			if versionFlag {
				version.PrintVersionAndExit()
			}

			flag.PrintFlags(cmd.Flags())

			logMetricsEndpointFromFlags(cmd.Flags())

			c, err := s.Config(controllerList, app.ControllersDisabledByDefault.List(), nil, nil, nil)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}

			cloudconfig := c.Complete().ComponentConfig.KubeCloudShared.CloudProvider
			cloud, err := cloudprovider.InitCloudProvider(cloudconfig.Name, cloudconfig.CloudConfigFile)
			if err != nil {
				klog.Fatalf("Cloud provider could not be initialized: %v", err)
			}
			if cloud == nil {
				klog.Fatalf("cloud provider is nil")
			}

			completedConfig := c.Complete()
			controllerInitializers := app.ConstructControllerInitializers(app.DefaultInitFuncConstructors, completedConfig, cloud)
			if err := app.Run(completedConfig, cloud, controllerInitializers, nil, wait.NeverStop); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
		},
	}

	fs := command.Flags()
	namedFlagSets := s.Flags(controllerList, app.ControllersDisabledByDefault.List(), nil, nil, nil)
	for _, f := range namedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}

	fs.BoolVar(&versionFlag, "version", false, "Print version and exit")

	edgecenter.AddExtraFlags(pflag.CommandLine)

	pflag.CommandLine.SetNormalizeFunc(flag.WordSepNormalizeFunc)

	logs.InitLogs()
	defer logs.FlushLogs()

	klog.V(1).Infof("edgecenter-cloud-controller-manager version: %s", version.Version)

	if err := command.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func logMetricsEndpointFromFlags(fs *pflag.FlagSet) {
	get := func(name string) (string, bool) {
		f := fs.Lookup(name)
		if f == nil {
			return "", false
		}
		return f.Value.String(), true
	}

	if mba, ok := get("metrics-bind-address"); ok && mba != "" {
		klog.Infof("Metrics endpoint (metrics-bind-address): http://%s/metrics", mba)
	}

	bind, ok1 := get("bind-address")
	securePort, ok2 := get("secure-port")
	if ok1 && ok2 && bind != "" && securePort != "" {
		klog.Infof("Metrics endpoint (secure-port): https://%s:%s/metrics", bind, securePort)
	}
}
