package edgecenter

import (
	"bytes"
	"context"
	"flag"
	"net/http"
	"strings"
	"testing"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

func regionalService(region string) *corev1.Service {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "kafka", Namespace: "test"}}
	if region != "" {
		svc.Annotations = map[string]string{ServiceAnnotationLoadBalancerRegionID: region}
	}
	return svc
}

func regionalBalancer(t *testing.T) *LbaasV2 {
	t.Helper()
	dm := testRegion(t, "default", 1, http.StatusOK, nil)
	dt := testRegion(t, "dt", 2, http.StatusOK, []edgecloud.Instance{testRegionalVM()})
	return &LbaasV2{
		nodeRegions: &nodeRegionResolver{regions: []nodeRegion{dm, dt}},
		LoadBalancer: LoadBalancer{
			client:     dm.client,
			edgecenter: EdgecenterOpts{ProjectID: 1, RegionID: 1},
			opts: LoadBalancerOpts{
				NetworkID: "dm-network", SubnetID: "dm-subnet", FloatingNetworkID: "dm-floating", FloatingSubnetID: "dm-floating-subnet",
				LBRegions: map[string]*LBRegion{"2": {NetworkID: "dt-network", NodeSelector: "placement=dt"}},
			},
		},
	}
}

func TestServiceRegionUsesConfiguredClientAndIsolatedNetworks(t *testing.T) {
	l := regionalBalancer(t)
	regional, err := l.forService(regionalService(" 2 "))
	require.NoError(t, err)
	require.Equal(t, "dt", regional.scopeName)
	require.Same(t, l.nodeRegions.regions[1].client, regional.client)
	require.Equal(t, 2, regional.edgecenter.RegionID)
	require.Equal(t, "dt-network", regional.opts.NetworkID)
	require.Empty(t, regional.opts.SubnetID, "must not inherit a DM subnet when only a DT network is specified")
	require.Empty(t, regional.opts.FloatingNetworkID)
	require.Empty(t, regional.opts.FloatingSubnetID)
	require.Equal(t, 1, l.edgecenter.RegionID)
	require.Equal(t, "dm-subnet", l.opts.SubnetID, "another Service's defaults must stay unchanged")
	primary, err := l.forService(regionalService(""))
	require.NoError(t, err)
	require.Same(t, l.client, primary.client)
	require.Equal(t, "default", primary.scopeName)
	require.Equal(t, "dm-subnet", primary.opts.SubnetID)
}

func TestServiceRegionRejectsUnknownAndInvalidSettings(t *testing.T) {
	for _, value := range []string{"0", "-1", "moscow", "99", " "} {
		t.Run(value, func(t *testing.T) {
			_, err := regionalBalancer(t).forService(regionalService(value))
			require.Error(t, err)
		})
	}
	l := regionalBalancer(t)
	for _, settings := range []*LBRegion{nil, {NetworkID: "dt-network"}, {NodeSelector: "placement=dt"}, {NetworkID: "dt-network", NodeSelector: "bad selector !!!"}} {
		l.opts.LBRegions["2"] = settings
		_, err := l.forService(regionalService("2"))
		require.Error(t, err)
	}
	_, err := l.forService(nil)
	require.Error(t, err)
	duplicate := l.nodeRegions.regions[0]
	duplicate.name = "another-project"
	duplicate.projectID = 2
	l.nodeRegions.regions = append(l.nodeRegions.regions, duplicate)
	_, err = l.forService(regionalService("1"))
	require.ErrorContains(t, err, "ambiguous")
}

func TestServiceSelectorCannotOverrideActualVMRegion(t *testing.T) {
	l := regionalBalancer(t)
	dt := testRegionalNode()
	dt.Labels = map[string]string{NodeRegionLabel: "dt", "placement": "dt"}
	regional, err := l.forService(regionalService("2"))
	require.NoError(t, err)
	nodes, err := regional.selectServiceNodes(context.Background(), []*corev1.Node{dt})
	require.NoError(t, err)
	require.Equal(t, []*corev1.Node{dt}, nodes)
	// A misplaced label can select the node but cannot make its VM belong to DM.
	l.opts.LBRegions["1"] = &LBRegion{NodeSelector: "placement=dt"}
	_, err = l.EnsureLoadBalancer(context.Background(), "cluster", regionalService("1"), []*corev1.Node{dt})
	require.ErrorContains(t, err, "belongs to scope")
	delete(l.opts.LBRegions, "1")
	primary, err := l.forService(regionalService(""))
	require.NoError(t, err)
	nodes, err = primary.selectServiceNodes(context.Background(), []*corev1.Node{dt})
	require.NoError(t, err)
	require.Empty(t, nodes, "default LB must still exclude foreign-region backends")
	dt.Labels["placement"] = "other"
	nodes, err = regional.selectServiceNodes(context.Background(), []*corev1.Node{dt})
	require.NoError(t, err)
	require.Empty(t, nodes)
	err = l.UpdateLoadBalancer(context.Background(), "cluster", regionalService("2"), []*corev1.Node{dt})
	require.ErrorContains(t, err, "no available nodes")
}

func TestServiceLookupAndDeletionUseSelectedRegion(t *testing.T) {
	l := regionalBalancer(t)
	// A client that returns an empty LB list and checks the SDK's actual URL.
	l.nodeRegions.regions[1] = testRegion(t, "dt", 2, http.StatusOK, nil)
	l.client.HTTPClient.Transport = regionTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("regional LB lookup must not use the primary client")
		return nil, nil
	})
	_, exists, err := l.GetLoadBalancer(context.Background(), "cluster", regionalService("2"))
	require.NoError(t, err)
	require.False(t, exists)
	require.NoError(t, l.EnsureLoadBalancerDeleted(context.Background(), "cluster", regionalService("2")))
}

func TestReadRegionalLoadBalancerConfig(t *testing.T) {
	cfg, err := ReadConfig(strings.NewReader(`[LoadBalancerRegion "2"]
network-id=dt-network
subnet-id=dt-subnet
floating-network-id=dt-floating
node-selector=placement=dt
`))
	require.NoError(t, err)
	require.Equal(t, &LBRegion{NetworkID: "dt-network", SubnetID: "dt-subnet", FloatingNetworkID: "dt-floating", NodeSelector: "placement=dt"}, cfg.LoadBalancer.LBRegions["2"])
	for _, name := range []string{"dt", "0", "-1", "02"} {
		_, err = ReadConfig(strings.NewReader(`[LoadBalancerRegion "` + name + `"]` + "\nnetwork-id=x\n"))
		require.Error(t, err)
	}
}

func TestCloudConfigLogsDoNotContainToken(t *testing.T) {
	state := klog.CaptureState()
	defer state.Restore()
	flags := flag.NewFlagSet("logging-test", flag.ContinueOnError)
	klog.InitFlags(flags)
	require.NoError(t, flags.Set("v", "5"))
	klog.LogToStderr(false)
	var output bytes.Buffer
	klog.SetOutput(&output)
	cfg, err := ReadConfig(strings.NewReader("[Edgecenter]\napi-token=sentinel-secret-token\nproject-id=1\nregion-id=1\n"))
	require.NoError(t, err)
	LogCfg(cfg)
	klog.Flush()
	require.NotContains(t, output.String(), "sentinel-secret-token")
	require.Contains(t, output.String(), "RegionID")
}

func TestPrimaryAliasDoesNotChangeRegionalLoadBalancerFiltering(t *testing.T) {
	l := regionalBalancer(t)
	l.nodeRegions.primaryAlias = "dm"
	dmVM := testRegionalVM()
	dmVM.Name = "dm-worker01"
	l.nodeRegions.regions[0] = testRegion(t, "default", 1, http.StatusOK, []edgecloud.Instance{dmVM})
	dm := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: dmVM.Name, Labels: map[string]string{NodeRegionLabel: "dm"}}}
	dt := testRegionalNode()
	dt.Labels = map[string]string{NodeRegionLabel: "dt", "placement": "dt"}
	for _, region := range []string{"", "1", "2"} {
		regional, err := l.forService(regionalService(region))
		require.NoError(t, err)
		nodes, err := regional.selectServiceNodes(context.Background(), []*corev1.Node{dm, dt})
		require.NoError(t, err)
		if region == "2" {
			require.Equal(t, []*corev1.Node{dt}, nodes)
		} else {
			require.Equal(t, []*corev1.Node{dm}, nodes)
			require.Equal(t, "default", regional.scopeName)
		}
	}
}
