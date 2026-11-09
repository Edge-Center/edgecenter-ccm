package edgecenter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	cloudprovider "k8s.io/cloud-provider"
)

const regionalTestID = "7b9cf879-7146-417c-abfd-cb4272f0c935"

type regionTransport func(*http.Request) (*http.Response, error)

func (f regionTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// Exercise the real SDK's URL construction, response decoding and error handling
// without contacting the cloud or using credentials from the environment.
func testRegion(t *testing.T, name string, regionID int, status int, instances []edgecloud.Instance) nodeRegion {
	t.Helper()
	client, err := NewEdgecenterClient(EdgecenterOpts{ApiURL: "https://cloud.invalid", ApiToken: "test", ProjectID: 1, RegionID: regionID}, "test")
	require.NoError(t, err)
	client.HTTPClient.Transport = regionTransport(func(req *http.Request) (*http.Response, error) {
		require.Contains(t, req.URL.Path, fmt.Sprintf("/1/%d", regionID))
		body := `{"message":"test API error"}`
		if status == http.StatusOK {
			var value interface{} = map[string]interface{}{"count": len(instances), "results": instances}
			if strings.HasSuffix(req.URL.Path, "/"+regionalTestID) {
				require.Len(t, instances, 1)
				value = instances[0]
			}
			data, marshalErr := json.Marshal(value)
			require.NoError(t, marshalErr)
			body = string(data)
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	return nodeRegion{name: name, regionID: regionID, projectID: 1, client: client}
}

func testRegionalNode() *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "dt-worker01"}}
}

func testRegionalVM() edgecloud.Instance {
	return edgecloud.Instance{ID: regionalTestID, Name: "dt-worker01", Status: "ACTIVE", Region: "DT-2", Flavor: &edgecloud.Flavor{FlavorID: "test-flavor"}}
}

func TestRegionalNodeSurvivesDefaultRegionMiss(t *testing.T) {
	for _, providerID := range []string{"", buildProviderID(regionalTestID)} {
		t.Run(providerID, func(t *testing.T) {
			defaultStatus := http.StatusOK
			if providerID != "" {
				defaultStatus = http.StatusNotFound
			}
			r := &nodeRegionResolver{regions: []nodeRegion{
				testRegion(t, "default", 1, defaultStatus, nil),
				testRegion(t, "dt", 2, http.StatusOK, []edgecloud.Instance{testRegionalVM()}),
			}}
			i := &Instances{regions: r}
			node := testRegionalNode()
			node.Spec.ProviderID = providerID
			exists, err := i.InstanceExists(context.Background(), node)
			require.NoError(t, err)
			require.True(t, exists, "a VM in DT must not be deleted because it is absent from DM")
			metadata, err := i.InstanceMetadata(context.Background(), node)
			require.NoError(t, err)
			require.Equal(t, buildProviderID(regionalTestID), metadata.ProviderID)
			require.Equal(t, "DT-2", metadata.Region)
			shutdown, err := i.InstanceShutdown(context.Background(), node)
			require.NoError(t, err)
			require.False(t, shutdown)
		})
	}
}

func TestRegionalDeletionRequiresConfirmedAbsence(t *testing.T) {
	for _, providerID := range []string{"", buildProviderID(regionalTestID)} {
		for _, status := range []int{http.StatusOK, http.StatusForbidden, http.StatusUnauthorized, http.StatusNotFound} {
			t.Run(fmt.Sprintf("%s/%d", providerID, status), func(t *testing.T) {
				// A successful empty list or an instance GET returning 404 is
				// conclusive. A list endpoint returning 404 is not.
				if providerID != "" && status == http.StatusOK {
					return
				}
				r := &nodeRegionResolver{regions: []nodeRegion{testRegion(t, "default", 1, status, nil)}}
				node := testRegionalNode()
				node.Spec.ProviderID = providerID
				exists, err := (&Instances{regions: r}).InstanceExists(context.Background(), node)
				require.False(t, exists)
				confirmed := (providerID == "" && status == http.StatusOK) || (providerID != "" && status == http.StatusNotFound)
				if confirmed {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			})
		}
	}
}

func TestRegionalLookupRejectsAmbiguousNamesAndUnknownRegions(t *testing.T) {
	r := &nodeRegionResolver{regions: []nodeRegion{
		testRegion(t, "default", 1, http.StatusOK, []edgecloud.Instance{testRegionalVM()}),
		testRegion(t, "dt", 2, http.StatusOK, []edgecloud.Instance{testRegionalVM()}),
	}}
	node := testRegionalNode()
	_, _, err := r.resolve(context.Background(), node)
	require.ErrorContains(t, err, "ambiguous")
	node.Labels = map[string]string{NodeRegionLabel: "unknown"}
	_, _, err = r.resolve(context.Background(), node)
	require.ErrorContains(t, err, "unknown")
	node.Labels[NodeRegionLabel] = "dt"
	_, region, err := r.resolve(context.Background(), node)
	require.NoError(t, err)
	require.Equal(t, "dt", region.name)
}

func TestRegionalLookupPreservesAPIErrorsAndInvalidIdentity(t *testing.T) {
	r := &nodeRegionResolver{regions: []nodeRegion{
		testRegion(t, "default", 1, http.StatusOK, []edgecloud.Instance{testRegionalVM()}),
		testRegion(t, "dt", 2, http.StatusForbidden, nil),
	}}
	_, _, err := r.resolve(context.Background(), testRegionalNode())
	require.Error(t, err, "an inaccessible scope cannot establish uniqueness")
	node := testRegionalNode()
	node.Spec.ProviderID = "other:///" + regionalTestID
	_, _, err = r.resolve(context.Background(), node)
	require.ErrorContains(t, err, "expected format")
	_, _, err = r.resolve(context.Background(), nil)
	require.Error(t, err)
	transportFailure := testRegion(t, "default", 1, http.StatusOK, nil)
	transportFailure.client.HTTPClient.Transport = regionTransport(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("connection failed") })
	node.Spec.ProviderID = ""
	_, _, err = (&nodeRegionResolver{regions: []nodeRegion{transportFailure}}).resolve(context.Background(), node)
	require.ErrorContains(t, err, "connection failed")
}

func TestRegionalLoadBalancerUsesOnlyDefaultScope(t *testing.T) {
	dmVM := testRegionalVM()
	dmVM.Name = "dm-worker01"
	r := &nodeRegionResolver{regions: []nodeRegion{
		testRegion(t, "default", 1, http.StatusOK, []edgecloud.Instance{dmVM}),
		testRegion(t, "dt", 2, http.StatusOK, []edgecloud.Instance{testRegionalVM()}),
	}}
	dm := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "dm-worker01", Labels: map[string]string{NodeRegionLabel: "default"}}}
	dt := testRegionalNode()
	dt.Labels = map[string]string{NodeRegionLabel: "dt"}
	l := &LbaasV2{nodeRegions: r}
	local, err := l.selectServiceNodes(context.Background(), []*corev1.Node{dt, dm})
	require.NoError(t, err)
	require.Equal(t, []*corev1.Node{dm}, local)
	dt.Labels[NodeRegionLabel] = "unknown"
	_, err = l.selectServiceNodes(context.Background(), []*corev1.Node{dt, dm})
	require.Error(t, err)
}

func TestRegionalConfigAndExactNameLookup(t *testing.T) {
	cfg, err := ReadConfig(strings.NewReader(`[Edgecenter]
api-token=test
project-id=1
region-id=1
[NodeRegion "dt"]
region-id=2
project-id=3
`))
	require.NoError(t, err)
	require.Equal(t, 2, cfg.NodeRegion["dt"].RegionID)
	base := testRegion(t, "default", 1, http.StatusOK, nil)
	r, err := newNodeRegionResolver(cfg, base.client)
	require.NoError(t, err)
	require.Len(t, r.regions, 2)
	for _, opts := range []*NodeRegionOpts{nil, {RegionID: 0}, {RegionID: 1, ProjectID: 1}} {
		cfg.NodeRegion["dt"] = opts
		_, err := newNodeRegionResolver(cfg, base.client)
		require.Error(t, err)
	}
	vm := testRegionalVM()
	vm.Name = "dt-worker01-extra"
	r = &nodeRegionResolver{regions: []nodeRegion{testRegion(t, "default", 1, http.StatusOK, []edgecloud.Instance{vm})}}
	_, _, err = r.resolve(context.Background(), testRegionalNode())
	require.Error(t, err)
	require.NotErrorIs(t, err, cloudprovider.InstanceNotFound)
	vm = testRegionalVM()
	r = &nodeRegionResolver{regions: []nodeRegion{testRegion(t, "default", 1, http.StatusOK, []edgecloud.Instance{vm, vm})}}
	_, _, err = r.resolve(context.Background(), testRegionalNode())
	require.ErrorContains(t, err, "ambiguous")
}

func TestPrimaryRegionAliasPreservesCanonicalScope(t *testing.T) {
	cfg, err := ReadConfig(strings.NewReader(`[Edgecenter]
api-url=https://cloud.invalid
api-token=test
project-id=1
region-id=1
node-region-name=dm
[NodeRegion "dt"]
region-id=2
`))
	require.NoError(t, err)
	require.Equal(t, "dm", cfg.Edgecenter.NodeRegionName)
	vm := testRegionalVM()
	vm.Name, vm.Region = "dm-worker01", ""
	primary := testRegion(t, "default", 1, http.StatusOK, []edgecloud.Instance{vm})
	r, err := newNodeRegionResolver(cfg, primary.client)
	require.NoError(t, err)
	require.Len(t, r.regions, 2, "alias must not add a second API scope")
	require.Same(t, primary.client, r.regions[0].client)
	foreign := testRegion(t, "dt", 2, http.StatusOK, nil)
	r.regions[1].client = foreign.client
	primaryCalls, foreignCalls := 0, 0
	primaryTransport := primary.client.HTTPClient.Transport
	primary.client.HTTPClient.Transport = regionTransport(func(req *http.Request) (*http.Response, error) {
		primaryCalls++
		return primaryTransport.RoundTrip(req)
	})
	foreignTransport := foreign.client.HTTPClient.Transport
	foreign.client.HTTPClient.Transport = regionTransport(func(req *http.Request) (*http.Response, error) {
		foreignCalls++
		return foreignTransport.RoundTrip(req)
	})
	for _, label := range []string{"dm", "default"} {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: vm.Name, Labels: map[string]string{NodeRegionLabel: label}}}
		_, scope, err := r.resolve(context.Background(), node)
		require.NoError(t, err)
		require.Equal(t, "default", scope.name, "LB filtering must keep the canonical primary scope")
		require.Equal(t, 1, scope.regionID)
		require.Empty(t, metadataRegion(&vm, scope), "alias must not change legacy region metadata")
	}
	require.Equal(t, 2, primaryCalls)
	require.Zero(t, foreignCalls, "explicit alias must never query a different region")
	primaryCalls = 0
	_, _, err = r.resolve(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: vm.Name}})
	require.NoError(t, err)
	require.Equal(t, 1, primaryCalls, "unlabelled lookup must not query the primary twice")
	require.Equal(t, 1, foreignCalls)
	_, _, err = r.resolve(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: vm.Name, Labels: map[string]string{NodeRegionLabel: "unknown"}}})
	require.ErrorContains(t, err, "unknown")
	require.Equal(t, 1, primaryCalls, "unknown alias must fail before any lookup")
	require.Equal(t, 1, foreignCalls)
}

func TestPrimaryRegionAliasValidationAndLegacyDefaults(t *testing.T) {
	primary := testRegion(t, "default", 1, http.StatusOK, []edgecloud.Instance{testRegionalVM()})
	for _, alias := range []string{"", "default", "dm"} {
		cfg := Config{Edgecenter: EdgecenterOpts{ProjectID: 1, RegionID: 1, NodeRegionName: alias}}
		r, err := newNodeRegionResolver(cfg, primary.client)
		require.NoError(t, err)
		require.Len(t, r.regions, 1)
		node := testRegionalNode()
		node.Labels = map[string]string{NodeRegionLabel: "dm"}
		_, _, err = r.resolve(context.Background(), node)
		if alias == "dm" {
			require.NoError(t, err)
		} else {
			require.ErrorContains(t, err, "unknown")
		}
	}
	for _, alias := range []string{" ", "dm/primary", "-dm", strings.Repeat("a", 64)} {
		_, err := newNodeRegionResolver(Config{Edgecenter: EdgecenterOpts{NodeRegionName: alias}}, primary.client)
		require.ErrorContains(t, err, "invalid Edgecenter node-region-name")
	}
	_, err := newNodeRegionResolver(Config{
		Edgecenter: EdgecenterOpts{NodeRegionName: "dt"},
		NodeRegion: map[string]*NodeRegionOpts{"dt": {RegionID: 2}},
	}, primary.client)
	require.ErrorContains(t, err, "conflicts with a NodeRegion section")
	_, err = newNodeRegionResolver(Config{
		Edgecenter: EdgecenterOpts{ProjectID: 1, RegionID: 1, NodeRegionName: "dm"},
		NodeRegion: map[string]*NodeRegionOpts{"other": {ProjectID: 1, RegionID: 1}},
	}, primary.client)
	require.ErrorContains(t, err, "duplicate project/region")
}

func TestPrimaryRegionAliasPreservesAPIErrors(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		primary := testRegion(t, "default", 1, status, nil)
		r, err := newNodeRegionResolver(Config{Edgecenter: EdgecenterOpts{ProjectID: 1, RegionID: 1, NodeRegionName: "dm"}}, primary.client)
		require.NoError(t, err)
		node := testRegionalNode()
		node.Spec.ProviderID = "edgecenter:///" + regionalTestID
		node.Labels = map[string]string{NodeRegionLabel: "dm"}
		_, _, err = r.resolve(context.Background(), node)
		require.Error(t, err)
		if status == http.StatusNotFound {
			require.ErrorIs(t, err, cloudprovider.InstanceNotFound)
		} else {
			require.NotErrorIs(t, err, cloudprovider.InstanceNotFound, "403 must not permit node deletion")
		}
	}
}
