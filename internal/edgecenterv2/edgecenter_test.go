/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package edgecenter

import (
	"context"
	"errors"
	"fmt"
	edgecentercloud "github.com/Edge-Center/edgecentercloud-go"
	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	"net"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/spf13/pflag"

	"ec-ccm/internal/util/metadata"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	testClusterName = "testCluster"

	// volumeStatus* is configuration of exponential backoff for
	// waiting for specified volume status. Starting with 1
	// seconds, multiplying by 1.2 with each step and taking 13 steps at maximum
	// it will time out after 32s, which roughly corresponds to 30s
	volumeStatusInitDelay = 1 * time.Second
	volumeStatusFactor    = 1.2
	volumeStatusSteps     = 13
)

// ConfigFromEnv allows setting up credentials etc. using EC_* Edgecenter client environment variables.
// Used only in tests
func ConfigFromEnv() Config {
	var cfg Config

	// Set default values for config params
	cfg.BlockStorage.BSVersion = "auto"
	cfg.BlockStorage.TrustDevicePath = false
	cfg.BlockStorage.IgnoreVolumeAZ = false
	cfg.Metadata.SearchOrder = fmt.Sprintf("%s,%s", metadata.ConfigDriveID, metadata.MetadataID)
	cfg.Networking.IPv6SupportDisabled = false
	cfg.Networking.PublicNetworkName = "public"
	cfg.LoadBalancer.InternalLB = false

	// edgecenter
	regionID, projectID := 1, 1
	var err error
	if os.Getenv("EC_REGION_ID") != "" {
		if regionID, err = strconv.Atoi(os.Getenv("EC_REGION_ID")); err != nil {
			panic(err)
		}
	}
	if os.Getenv("EC_PROJECT_ID") != "" {
		if projectID, err = strconv.Atoi(os.Getenv("EC_PROJECT_ID")); err != nil {
			panic(err)
		}
	}
	cfg.Edgecenter.ApiURL = os.Getenv("EC_API_URL")
	cfg.Edgecenter.RegionID = regionID
	cfg.Edgecenter.ProjectID = projectID
	cfg.Edgecenter.ApiToken = os.Getenv("EC_API_TOKEN")

	return cfg
}

func WaitForVolumeStatus(t *testing.T, edgecenter *Edgecenter, volumeName string, status string) {
	ctx := context.Background()

	backoff := wait.Backoff{
		Duration: volumeStatusInitDelay,
		Factor:   volumeStatusFactor,
		Steps:    volumeStatusSteps,
	}

	start := time.Now().Second()
	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		getVol, err := edgecenter.getVolume(ctx, volumeName)
		if err != nil {
			return false, err
		}
		if getVol.Status == status {
			t.Logf("Volume (%s) status changed to %s after %v seconds\n",
				volumeName,
				status,
				time.Now().Second()-start)
			return true, nil
		}
		return false, nil
	})
	if errors.Is(err, wait.ErrWaitTimeout) {
		t.Logf("Volume (%s) status did not change to %s after %v seconds\n",
			volumeName,
			status,
			time.Now().Second()-start)
		return
	}
	if err != nil {
		t.Fatalf("Cannot get existing Cinder volume (%s): %v", volumeName, err)
	}
}

func TestReadConfig(t *testing.T) {
	_, err := ReadConfig(nil)
	if err == nil {
		t.Errorf("Should fail when no config is provided: %s", err)
	}

	cfg, err := ReadConfig(strings.NewReader(`
 [Global]
 auth-url = http://auth.url
 user-id = user
 password = mypass
 tenant-name = demo
 tenant-domain-name = Default
 region = RegionOne
 [LoadBalancer]
 create-monitor = yes
 monitor-delay = 1m
 monitor-timeout = 30s
 monitor-max-retries = 3
 [BlockStorage]
 bs-version = auto
 trust-device-path = yes
 ignore-volume-az = yes
 [Metadata]
 search-order = configDrive, metadataService
 [Edgecenter]
 api-url=http://edgecenter.url
 project-id=1
 region-id=1
 api-token=api-token
 `))
	if err != nil {
		t.Fatalf("Should succeed when a valid config is provided: %s", err)
	}
	if !cfg.LoadBalancer.CreateMonitor {
		t.Errorf("incorrect lb.createmonitor: %t", cfg.LoadBalancer.CreateMonitor)
	}
	if cfg.LoadBalancer.MonitorDelay.Duration != 1*time.Minute {
		t.Errorf("incorrect lb.monitordelay: %s", cfg.LoadBalancer.MonitorDelay)
	}
	if cfg.LoadBalancer.MonitorTimeout.Duration != 30*time.Second {
		t.Errorf("incorrect lb.monitortimeout: %s", cfg.LoadBalancer.MonitorTimeout)
	}
	if cfg.LoadBalancer.MonitorMaxRetries != 3 {
		t.Errorf("incorrect lb.monitormaxretries: %d", cfg.LoadBalancer.MonitorMaxRetries)
	}
	if cfg.BlockStorage.TrustDevicePath != true {
		t.Errorf("incorrect bs.trustdevicepath: %v", cfg.BlockStorage.TrustDevicePath)
	}
	if cfg.BlockStorage.BSVersion != "auto" {
		t.Errorf("incorrect bs.bs-version: %v", cfg.BlockStorage.BSVersion)
	}
	if cfg.BlockStorage.IgnoreVolumeAZ != true {
		t.Errorf("incorrect bs.IgnoreVolumeAZ: %v", cfg.BlockStorage.IgnoreVolumeAZ)
	}
	if cfg.Metadata.SearchOrder != "configDrive, metadataService" {
		t.Errorf("incorrect md.search-order: %v", cfg.Metadata.SearchOrder)
	}
	if cfg.Edgecenter.ApiURL != "http://edgecenter.url" {
		t.Errorf("incorrect apiurl: %s", cfg.Edgecenter.ApiURL)
	}
	if cfg.Edgecenter.RegionID != 1 {
		t.Errorf("incorrect region id %d", cfg.Edgecenter.RegionID)
	}
	if cfg.Edgecenter.ProjectID != 1 {
		t.Errorf("incorrect project id %d", cfg.Edgecenter.ProjectID)
	}
	if cfg.Edgecenter.ApiToken != "api-token" {
		t.Errorf("incorrect api token %s", cfg.Edgecenter.ApiToken)
	}
}

func TestCheckEdgecenterOpts(t *testing.T) {
	delay := MyDuration{60 * time.Second}
	timeout := MyDuration{30 * time.Second}
	tests := []struct {
		name           string
		edgecenterOpts *Edgecenter
		expectedError  error
	}{
		{
			name: "test1",
			edgecenterOpts: &Edgecenter{
				client: nil,
				lbOpts: LoadBalancerOpts{
					LBVersion:            "v2",
					SubnetID:             "6261548e-ffde-4bc7-bd22-59c83578c5ef",
					FloatingNetworkID:    "38b8b5f9-64dc-4424-bf86-679595714786",
					LBMethod:             "ROUND_ROBIN",
					LBProvider:           "haproxy",
					CreateMonitor:        true,
					MonitorDelay:         delay,
					MonitorTimeout:       timeout,
					MonitorMaxRetries:    uint(3),
					ManageSecurityGroups: true,
				},
				metadataOpts: MetadataOpts{
					SearchOrder: metadata.ConfigDriveID,
				},
			},
			expectedError: nil,
		},
		{
			name: "test2",
			edgecenterOpts: &Edgecenter{
				client: nil,
				lbOpts: LoadBalancerOpts{
					LBVersion:            "v2",
					FloatingNetworkID:    "38b8b5f9-64dc-4424-bf86-679595714786",
					LBMethod:             "ROUND_ROBIN",
					CreateMonitor:        true,
					MonitorDelay:         delay,
					MonitorTimeout:       timeout,
					MonitorMaxRetries:    uint(3),
					ManageSecurityGroups: true,
				},
				metadataOpts: MetadataOpts{
					SearchOrder: metadata.ConfigDriveID,
				},
			},
			expectedError: nil,
		},
		{
			name: "test3",
			edgecenterOpts: &Edgecenter{
				client: nil,
				lbOpts: LoadBalancerOpts{
					LBVersion:            "v2",
					SubnetID:             "6261548e-ffde-4bc7-bd22-59c83578c5ef",
					FloatingNetworkID:    "38b8b5f9-64dc-4424-bf86-679595714786",
					LBMethod:             "ROUND_ROBIN",
					CreateMonitor:        true,
					MonitorTimeout:       timeout,
					MonitorMaxRetries:    uint(3),
					ManageSecurityGroups: true,
				},
				metadataOpts: MetadataOpts{
					SearchOrder: metadata.ConfigDriveID,
				},
			},
			expectedError: fmt.Errorf("monitor-delay not set in cloud provider config"),
		},
		{
			name: "test4",
			edgecenterOpts: &Edgecenter{
				client: nil,
				metadataOpts: MetadataOpts{
					SearchOrder: "",
				},
			},
			expectedError: fmt.Errorf("invalid value in section [Metadata] with key `search-order`. Value cannot be empty"),
		},
		{
			name: "test5",
			edgecenterOpts: &Edgecenter{
				client: nil,
				metadataOpts: MetadataOpts{
					SearchOrder: "value1,value2,value3",
				},
			},
			expectedError: fmt.Errorf("invalid value in section [Metadata] with key `search-order`. Value cannot contain more than 2 elements"),
		},
		{
			name: "test6",
			edgecenterOpts: &Edgecenter{
				client: nil,
				metadataOpts: MetadataOpts{
					SearchOrder: "value1",
				},
			},
			expectedError: fmt.Errorf("invalid element %q found in section [Metadata] with key `search-order`."+
				"Supported elements include %q and %q", "value1", metadata.ConfigDriveID, metadata.MetadataID),
		},
		{
			name: "test7",
			edgecenterOpts: &Edgecenter{
				client: nil,
				lbOpts: LoadBalancerOpts{
					LBVersion:            "v2",
					SubnetID:             "6261548e-ffde-4bc7-bd22-59c83578c5ef",
					FloatingNetworkID:    "38b8b5f9-64dc-4424-bf86-679595714786",
					LBMethod:             "ROUND_ROBIN",
					CreateMonitor:        true,
					MonitorDelay:         delay,
					MonitorTimeout:       timeout,
					ManageSecurityGroups: true,
				},
				metadataOpts: MetadataOpts{
					SearchOrder: metadata.ConfigDriveID,
				},
			},
			expectedError: fmt.Errorf("monitor-max-retries not set in cloud provider config"),
		},
		{
			name: "test8",
			edgecenterOpts: &Edgecenter{
				client: nil,
				lbOpts: LoadBalancerOpts{
					LBVersion:            "v2",
					SubnetID:             "6261548e-ffde-4bc7-bd22-59c83578c5ef",
					FloatingNetworkID:    "38b8b5f9-64dc-4424-bf86-679595714786",
					LBMethod:             "ROUND_ROBIN",
					CreateMonitor:        true,
					MonitorDelay:         delay,
					MonitorMaxRetries:    uint(3),
					ManageSecurityGroups: true,
				},
				metadataOpts: MetadataOpts{
					SearchOrder: metadata.ConfigDriveID,
				},
			},
			expectedError: fmt.Errorf("monitor-timeout not set in cloud provider config"),
		},
	}

	for _, testCase := range tests {
		err := checkEdgecenterOpts(testCase.edgecenterOpts)

		if err == nil && testCase.expectedError == nil {
			continue
		}

		if err != nil || testCase.expectedError != nil || !errors.Is(err, testCase.expectedError) {
			t.Errorf("%s failed: expected err=%q, got %q",
				testCase.name, testCase.expectedError, err)
		}
	}
}

func TestCaller(t *testing.T) {
	called := false
	myFunc := func() { called = true }

	c := newCaller()
	c.call(myFunc)

	if !called {
		t.Errorf("caller failed to call function in default case")
	}

	c.disarm()
	called = false
	c.call(myFunc)

	if called {
		t.Error("caller still called function when disarmed")
	}

	// Confirm the "usual" deferred caller pattern works as expected

	called = false
	successCase := func() {
		c := newCaller()
		defer c.call(func() { called = true })
		c.disarm()
	}
	if successCase(); called {
		t.Error("Deferred success case still invoked unwind")
	}

	called = false
	failureCase := func() {
		c := newCaller()
		defer c.call(func() { called = true })
	}
	if failureCase(); !called {
		t.Error("Deferred failure case failed to invoke unwind")
	}
}

func TestNodeAddresses(t *testing.T) {
	instance := edgecloud.Instance{
		ID:          "",
		Name:        "",
		Description: "",
		CreatedAt:   time.Time{}.String(),
		Status:      "",
		VMState:     "",
		TaskState:   "",
		Flavor:      &edgecloud.Flavor{},
		Metadata: edgecloud.Metadata{
			"name":       "a1-yinvcez57-0-bvynoyawrhcg-kube-minion-fg5i4jwcc2yy",
			TypeHostName: "a1-yinvcez57-0-bvynoyawrhcg-kube-minion-fg5i4jwcc2yy.novalocal",
		},
		Volumes: nil,
		Addresses: map[string][]edgecloud.InstanceAddress{
			"private": {
				{
					Type:    string(edgecloud.AddressTypeFixed),
					Address: net.ParseIP("10.0.0.32"),
				}, {
					Type:    string(edgecloud.AddressTypeFloating),
					Address: net.ParseIP("50.56.176.36"),
				}, {
					Address: net.ParseIP("10.0.0.31"),
				},
			},
			"public": {
				{
					Address: net.ParseIP("50.56.176.35"),
				}, {
					Address: net.ParseIP("2001:4800:780e:510:be76:4eff:fe04:84a8"),
				},
			},
		},
		SecurityGroups: nil,
		CreatorTaskID:  "",
		TaskID:         "",
		ProjectID:      0,
		RegionID:       0,
		Region:         "",
	}

	networkingOpts := NetworkingOpts{
		PublicNetworkName: "public",
	}

	addrs, err := nodeAddresses(&instance, networkingOpts)
	require.NoError(t, err)

	t.Logf("addresses are %v", addrs)

	want := []v1.NodeAddress{
		{Type: v1.NodeInternalIP, Address: "10.0.0.32"},
		{Type: v1.NodeExternalIP, Address: "50.56.176.36"},
		{Type: v1.NodeInternalIP, Address: "10.0.0.31"},
		{Type: v1.NodeExternalIP, Address: "50.56.176.35"},
		{Type: v1.NodeExternalIP, Address: "2001:4800:780e:510:be76:4eff:fe04:84a8"},
		{Type: v1.NodeHostName, Address: "a1-yinvcez57-0-bvynoyawrhcg-kube-minion-fg5i4jwcc2yy.novalocal"},
	}

	require.Equal(t, want, addrs)

}

func TestNodeAddressesCustomPublicNetwork(t *testing.T) {
	srv := &edgecloud.Instance{
		ID:          "",
		Name:        "",
		Description: "",
		CreatedAt:   time.Time{}.String(),
		Status:      "",
		VMState:     "",
		TaskState:   "",
		Flavor:      &edgecloud.Flavor{},
		Volumes:     nil,
		Addresses: map[string][]edgecloud.InstanceAddress{
			"private": {
				{
					Type:    string(edgecloud.AddressTypeFixed),
					Address: net.ParseIP("10.0.0.32"),
				}, {
					Type:    string(edgecloud.AddressTypeFloating),
					Address: net.ParseIP("50.56.176.36"),
				}, {
					Address: net.ParseIP("10.0.0.31"),
				},
			},
			"pub-net": {
				{
					Address: net.ParseIP("50.56.176.35"),
				}, {
					Address: net.ParseIP("2001:4800:780e:510:be76:4eff:fe04:84a8"),
				},
			},
		},
		SecurityGroups: nil,
		CreatorTaskID:  "",
		TaskID:         "",
		ProjectID:      0,
		RegionID:       0,
		Region:         "",
	}

	networkingOpts := NetworkingOpts{
		PublicNetworkName: "pub-net",
	}

	addrs, err := nodeAddresses(srv, networkingOpts)
	require.NoError(t, err)

	t.Logf("addresses are %v", addrs)

	want := []v1.NodeAddress{
		{Type: v1.NodeInternalIP, Address: "10.0.0.32"},
		{Type: v1.NodeExternalIP, Address: "50.56.176.36"},
		{Type: v1.NodeInternalIP, Address: "10.0.0.31"},
		{Type: v1.NodeExternalIP, Address: "50.56.176.35"},
		{Type: v1.NodeExternalIP, Address: "2001:4800:780e:510:be76:4eff:fe04:84a8"},
	}

	require.Equal(t, want, addrs)

}

func TestNodeAddressesIPv6Disabled(t *testing.T) {
	instance := &edgecloud.Instance{
		ID:          "",
		Name:        "",
		Description: "",
		CreatedAt:   time.Time{}.String(),
		Status:      "",
		VMState:     "",
		TaskState:   "",
		Flavor:      &edgecloud.Flavor{},
		Volumes:     nil,
		Addresses: map[string][]edgecloud.InstanceAddress{
			"private": {
				{
					Type:    string(edgecloud.AddressTypeFixed),
					Address: net.ParseIP("10.0.0.32"),
				}, {
					Type:    string(edgecloud.AddressTypeFloating),
					Address: net.ParseIP("50.56.176.36"),
				}, {
					Address: net.ParseIP("10.0.0.31"),
				},
			},
			"public": {
				{
					Address: net.ParseIP("50.56.176.35"),
				}, {
					Address: net.ParseIP("2001:4800:780e:510:be76:4eff:fe04:84a8"),
				},
			},
		},
		SecurityGroups: nil,
		CreatorTaskID:  "",
		TaskID:         "",
		ProjectID:      0,
		RegionID:       0,
		Region:         "",
	}

	networkingOpts := NetworkingOpts{
		PublicNetworkName:   "public",
		IPv6SupportDisabled: true,
	}

	addrs, err := nodeAddresses(instance, networkingOpts)
	require.NoError(t, err)

	t.Logf("addresses are %v", addrs)

	want := []v1.NodeAddress{
		{Type: v1.NodeInternalIP, Address: "10.0.0.32"},
		{Type: v1.NodeExternalIP, Address: "50.56.176.36"},
		{Type: v1.NodeInternalIP, Address: "10.0.0.31"},
		{Type: v1.NodeExternalIP, Address: "50.56.176.35"},
	}

	require.Equal(t, want, addrs)

}

func TestNewEdgecenter(t *testing.T) {
	cfg := ConfigFromEnv()
	testConfigFromEnv(t, &cfg)

	_, err := NewEdgecenter(cfg)
	if err != nil {
		t.Fatalf("Failed to construct/authenticate Edgecenter: %s", err)
	}
}

func TestLoadBalancer(t *testing.T) {
	cfg := ConfigFromEnv()
	testConfigFromEnv(t, &cfg)

	versions := []string{"v2"}

	for _, v := range versions {
		t.Logf("Trying LBVersion = '%s'\n", v)
		cfg.LoadBalancer.LBVersion = v

		edgecenter, err := NewEdgecenter(cfg)
		if err != nil {
			t.Fatalf("Failed to construct/authenticate Edgecenter: %s", err)
		}

		lb, ok := edgecenter.LoadBalancer()
		if !ok {
			t.Fatalf("LoadBalancer() returned false - perhaps your stack doesn't support Neutron?")
		}

		_, exists, err := lb.GetLoadBalancer(context.TODO(), testClusterName, &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "noexist"}})
		if err != nil {
			var errReAuth *edgecentercloud.UnableToReauthenticateError
			if errors.As(err, &errReAuth) {
				t.Skipf("Please run this test in an Edgecenter vm and set proper API tokens.")
			}
			t.Fatalf("GetLoadBalancer(\"noexist\") returned error: %s", err)
		}
		if exists {
			t.Fatalf("GetLoadBalancer(\"noexist\") returned exists")
		}
	}
}

var FakeMetadata = metadata.Metadata{
	UUID:             "83679162-1378-4288-a2d4-70e13ec132aa",
	Name:             "test",
	AvailabilityZone: "nova",
}

func TestZones(t *testing.T) {
	metadata.Set(&FakeMetadata)
	defer metadata.Clear()

	edgecenter := Edgecenter{
		client: edgecloud.NewClient(nil),
		region: "myRegion",
	}

	z, ok := edgecenter.Zones()
	if !ok {
		t.Fatalf("Zones() returned false")
	}

	zone, err := z.GetZone(context.TODO())
	if err != nil {
		t.Fatalf("GetZone() returned error: %s", err)
	}

	if zone.Region != "myRegion" {
		t.Fatalf("GetZone() returned wrong region (%s)", zone.Region)
	}

	if zone.FailureDomain != "nova" {
		t.Fatalf("GetZone() returned wrong failure domain (%s)", zone.FailureDomain)
	}
}

var diskPathRegexp = regexp.MustCompile("/dev/disk/(?:by-id|by-path)/")

func TestVolumes(t *testing.T) {
	cfg := ConfigFromEnv()
	testConfigFromEnv(t, &cfg)

	ctx := context.Background()

	edgecenter, err := NewEdgecenter(cfg)
	if err != nil {
		t.Fatalf("Failed to construct/authenticate Edgecenter: %s", err)
	}

	tags := map[string]string{
		"test": "value",
	}
	vol, _, _, _, err := edgecenter.CreateVolume(ctx, "kubernetes-test-volume-"+rand.String(10), 1, "", "", &tags)
	if err != nil {
		var errReAuth *edgecentercloud.UnableToReauthenticateError
		if errors.As(err, &errReAuth) {
			t.Skipf("Please run this test in an Edgecenter vm and set proper API tokens.")
		}
		t.Fatalf("Cannot create a new Cinder volume: %v", err)
	}
	t.Logf("Volume (%s) created\n", vol)

	WaitForVolumeStatus(t, edgecenter, vol, volumeAvailableStatus)

	id, err := edgecenter.InstanceID()
	if err != nil {
		t.Logf("Cannot find instance id: %v - perhaps you are running this test outside a VM launched by Edgecenter", err)
	} else {
		diskID, err := edgecenter.AttachDisk(ctx, id, vol)
		if err != nil {
			t.Fatalf("Cannot AttachDisk Cinder volume %s: %v", vol, err)
		}
		t.Logf("Volume (%s) attached, disk ID: %s\n", vol, diskID)

		WaitForVolumeStatus(t, edgecenter, vol, volumeInUseStatus)

		devicePath, err := edgecenter.GetDevicePath(diskID)
		if err != nil {
			t.Fatalf("Cannot GetDevicePath for Cinder volume %s: %v", vol, err)
		}
		if diskPathRegexp.FindString(devicePath) == "" {
			t.Fatalf("GetDevicePath returned and unexpected path for Cinder volume %s, returned %s", vol, devicePath)
		}
		t.Logf("Volume (%s) found at path: %s\n", vol, devicePath)

		err = edgecenter.DetachDisk(ctx, id, vol)
		if err != nil {
			t.Fatalf("Cannot DetachDisk Cinder volume %s: %v", vol, err)
		}
		t.Logf("Volume (%s) detached\n", vol)

		WaitForVolumeStatus(t, edgecenter, vol, volumeAvailableStatus)
	}

	expectedVolSize := resource.MustParse("2Gi")
	newVolSize, err := edgecenter.ExpandVolume(ctx, vol, resource.MustParse("1Gi"), expectedVolSize)
	if err != nil {
		t.Fatalf("Cannot expand a Cinder volume: %v", err)
	}
	if newVolSize != expectedVolSize {
		t.Logf("Expected: %v but got: %v ", expectedVolSize, newVolSize)
	}
	t.Logf("Volume expanded to (%v) \n", newVolSize)

	WaitForVolumeStatus(t, edgecenter, vol, volumeAvailableStatus)

	err = edgecenter.DeleteVolume(ctx, vol)
	if err != nil {
		t.Fatalf("Cannot delete Cinder volume %s: %v", vol, err)
	}
	t.Logf("Volume (%s) deleted\n", vol)

}

func TestInstanceIDFromProviderID(t *testing.T) {
	testCases := []struct {
		providerID string
		instanceID string
		fail       bool
	}{
		{
			providerID: ProviderName + "://" + "/" + "7b9cf879-7146-417c-abfd-cb4272f0c935",
			instanceID: "7b9cf879-7146-417c-abfd-cb4272f0c935",
			fail:       false,
		},
		{
			// https://github.com/kubernetes/kubernetes/issues/85731
			providerID: "/7b9cf879-7146-417c-abfd-cb4272f0c935",
			instanceID: "7b9cf879-7146-417c-abfd-cb4272f0c935",
			fail:       false,
		},
		{
			providerID: "openstack://7b9cf879-7146-417c-abfd-cb4272f0c935",
			instanceID: "",
			fail:       true,
		},
		{
			providerID: "edgecenter:///7b9cf879-7146-417c-abfd-cb4272f0c935",
			instanceID: "7b9cf879-7146-417c-abfd-cb4272f0c935",
			fail:       false,
		},
		{
			providerID: "7b9cf879-7146-417c-abfd-cb4272f0c935",
			instanceID: "",
			fail:       true,
		},
		{
			providerID: "other-provider:///7b9cf879-7146-417c-abfd-cb4272f0c935",
			instanceID: "",
			fail:       true,
		},
	}

	for _, test := range testCases {
		instanceID, err := instanceIDFromProviderID(test.providerID)
		if (err != nil) != test.fail {
			t.Errorf("expected err: %t, got err: %v", test.fail, err)
		}

		if test.fail {
			continue
		}

		if instanceID != test.instanceID {
			t.Errorf("%s yielded %s. expected %q", test.providerID, instanceID, test.instanceID)
		}
	}
}

func TestUserAgentFlag(t *testing.T) {
	tests := []struct {
		name        string
		shouldParse bool
		flags       []string
		expected    []string
	}{
		{"no_flag", true, []string{}, nil},
		{"one_flag", true, []string{"--user-agent=cluster/abc-123"}, []string{"cluster/abc-123"}},
		{"multiple_flags", true, []string{"--user-agent=a/b", "--user-agent=c/d"}, []string{"a/b", "c/d"}},
		{"flag_with_space", true, []string{"--user-agent=a b"}, []string{"a b"}},
		{"flag_split_with_space", true, []string{"--user-agent=a", "b"}, []string{"a"}},
		{"empty_flag", false, []string{"--user-agent"}, nil},
	}

	for _, testCase := range tests {
		userAgentData = []string{}

		t.Run(testCase.name, func(t *testing.T) {
			fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
			AddExtraFlags(fs)

			err := fs.Parse(testCase.flags)

			if testCase.shouldParse && err != nil {
				t.Errorf("Flags failed to parse")
			} else if !testCase.shouldParse && err == nil {
				t.Errorf("Flags should not have parsed")
			} else if testCase.shouldParse {
				if !reflect.DeepEqual(userAgentData, testCase.expected) {
					t.Errorf("userAgentData %#v did not match expected value %#v", userAgentData, testCase.expected)
				}
			}
		})
	}
}

func testConfigFromEnv(t *testing.T, cfg *Config) {
	if cfg.Edgecenter.ApiURL == "" {
		t.Skip("No config found in environment")
	}
}
