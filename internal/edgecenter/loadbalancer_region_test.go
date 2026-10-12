package edgecenter

import (
	"context"
	"net"
	"strings"
	"testing"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResolveServiceRegionAndIPAddress(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		defaultID   int
		wantRegion  int
		wantIP      string
		wantError   bool
	}{
		{
			name:       "use configured region without static address",
			defaultID:  10,
			wantRegion: 10,
		},
		{
			name: "region requires address",
			annotations: map[string]string{
				ServiceAnnotationLoadBalancerRegionID: "20",
			},
			wantError: true,
		},
		{
			name: "address requires region",
			annotations: map[string]string{
				ServiceAnnotationLoadBalancerIPAddress: "192.0.2.20",
			},
			wantError: true,
		},
		{
			name: "parse regional static address",
			annotations: map[string]string{
				ServiceAnnotationLoadBalancerRegionID:  " 20 ",
				ServiceAnnotationLoadBalancerIPAddress: " 192.0.2.20 ",
			},
			wantRegion: 20,
			wantIP:     "192.0.2.20",
		},
		{
			name: "reject nonnumeric region",
			annotations: map[string]string{
				ServiceAnnotationLoadBalancerRegionID:  "moscow",
				ServiceAnnotationLoadBalancerIPAddress: "192.0.2.20",
			},
			wantError: true,
		},
		{
			name: "reject IPv6 address",
			annotations: map[string]string{
				ServiceAnnotationLoadBalancerRegionID:  "20",
				ServiceAnnotationLoadBalancerIPAddress: "2001:db8::20",
			},
			wantError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Annotations: test.annotations}}
			regionID, address, err := resolveServiceRegionAndIPAddress(service, test.defaultID)
			if (err != nil) != test.wantError {
				t.Fatalf("resolveServiceRegionAndIPAddress() error = %v, wantError %v", err, test.wantError)
			}
			if err != nil {
				return
			}
			if regionID != test.wantRegion {
				t.Errorf("region ID = %d, want %d", regionID, test.wantRegion)
			}
			gotIP := ""
			if address != nil {
				gotIP = address.String()
			}
			if gotIP != test.wantIP {
				t.Errorf("IP address = %q, want %q", gotIP, test.wantIP)
			}
		})
	}
}

func TestReadConfigLoadsRegionalLoadBalancerOptions(t *testing.T) {
	config, err := ReadConfig(strings.NewReader(`[LoadBalancerRegion "20"]
network-id = "network-20"
subnet-id = "subnet-20"
floating-network-id = "floating-network-20"
node-selector = "topology.kubernetes.io/region=moscow"
`))
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}

	region := config.LoadBalancer.LBRegions["20"]
	if region == nil {
		t.Fatal("ReadConfig() did not load region 20")
	}
	if region.NetworkID != "network-20" || region.SubnetID != "subnet-20" || region.FloatingNetworkID != "floating-network-20" || region.NodeSelector != "topology.kubernetes.io/region=moscow" {
		t.Fatalf("regional settings = %+v", region)
	}
}

func TestReadConfigRejectsNonNumericRegionalSection(t *testing.T) {
	_, err := ReadConfig(strings.NewReader(`[LoadBalancerRegion "moscow"]
subnet-id = "subnet-moscow"
`))
	if err == nil {
		t.Fatal("ReadConfig() accepted a nonnumeric region ID")
	}
}

func TestForServiceUsesRegionalClientAndNetworkSettings(t *testing.T) {
	controller := &LbaasV2{LoadBalancer: LoadBalancer{
		client: edgecloud.NewClient(nil),
		opts: LoadBalancerOpts{
			NetworkID: "default-network",
			LBRegions: map[string]*LBRegion{
				"20": {
					NetworkID:         "network-20",
					SubnetID:          "subnet-20",
					FloatingNetworkID: "floating-network-20",
					NodeSelector:      "topology.kubernetes.io/region=moscow",
				},
			},
		},
		edgecenter: EdgecenterOpts{
			ApiURL:    "https://api.example.test",
			ApiToken:  "test-token",
			ProjectID: 1,
			RegionID:  10,
		},
	}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		ServiceAnnotationLoadBalancerRegionID:  "20",
		ServiceAnnotationLoadBalancerIPAddress: "192.0.2.20",
	}}}

	regional, err := controller.forService(service)
	if err != nil {
		t.Fatalf("forService() error = %v", err)
	}
	if regional == controller {
		t.Fatal("forService() returned the default regional controller")
	}
	if regional.edgecenter.RegionID != 20 {
		t.Errorf("regional ID = %d, want 20", regional.edgecenter.RegionID)
	}
	if regional.opts.NetworkID != "network-20" || regional.opts.SubnetID != "subnet-20" || regional.opts.FloatingNetworkID != "floating-network-20" {
		t.Errorf("regional network settings = %+v", regional.opts)
	}
	nodes, err := regional.selectServiceNodes([]*corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "worker-moscow", Labels: map[string]string{"topology.kubernetes.io/region": "moscow"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "worker-moscow-2", Labels: map[string]string{"topology.kubernetes.io/region": "moscow-2"}}},
	})
	if err != nil {
		t.Fatalf("selectServiceNodes() error = %v", err)
	}
	if len(nodes) != 1 || nodes[0].Name != "worker-moscow" {
		t.Fatalf("selected nodes = %v, want only worker-moscow", nodes)
	}
	if controller.edgecenter.RegionID != 10 || controller.opts.NetworkID != "default-network" {
		t.Fatal("forService() changed the default controller settings")
	}
}

func TestForServiceRequiresRegionalBackendSettings(t *testing.T) {
	controller := &LbaasV2{LoadBalancer: LoadBalancer{
		client:     edgecloud.NewClient(nil),
		opts:       LoadBalancerOpts{LBRegions: map[string]*LBRegion{"20": {NetworkID: "network-20", SubnetID: "subnet-20"}}},
		edgecenter: EdgecenterOpts{RegionID: 10},
	}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		ServiceAnnotationLoadBalancerRegionID:  "20",
		ServiceAnnotationLoadBalancerIPAddress: "192.0.2.20",
	}}}
	if _, err := controller.forService(service); err == nil {
		t.Fatal("forService() accepted a regional static IP without node-selector")
	}

	controller.opts.LBRegions["20"].NodeSelector = "topology.kubernetes.io/region=moscow"
	controller.opts.LBRegions["20"].NetworkID = ""
	controller.opts.LBRegions["20"].SubnetID = ""
	if _, err := controller.forService(service); err == nil {
		t.Fatal("forService() accepted a regional static IP without network-id or subnet-id")
	}
}

func TestSelectReservedVIPPort(t *testing.T) {
	address := net.ParseIP("192.0.2.20")
	tests := []struct {
		name        string
		reservedIPs []edgecloud.ReservedFixedIP
		currentLB   *edgecloud.Loadbalancer
		wantPortID  string
		wantSubnet  string
		wantFound   bool
		wantError   bool
	}{
		{
			name: "free reserved port",
			reservedIPs: []edgecloud.ReservedFixedIP{{
				FixedIPAddress: address,
				IsExternal:     true,
				PortID:         "vip-port",
				SubnetID:       "public-subnet",
			}},
			wantPortID: "vip-port",
			wantSubnet: "public-subnet",
			wantFound:  true,
		},
		{
			name: "port is attached to another load balancer",
			reservedIPs: []edgecloud.ReservedFixedIP{{
				FixedIPAddress: address,
				IsExternal:     true,
				PortID:         "vip-port",
				Reservation:    edgecloud.Reservation{ResourceID: "old-lb"},
			}},
			wantError: true,
		},
		{
			name: "port belongs to current load balancer",
			reservedIPs: []edgecloud.ReservedFixedIP{{
				FixedIPAddress: address,
				IsExternal:     true,
				PortID:         "vip-port",
				SubnetID:       "public-subnet",
				Reservation:    edgecloud.Reservation{ResourceID: "current-lb"},
			}},
			currentLB:  &edgecloud.Loadbalancer{ID: "current-lb", VipAddress: address, VipPortID: "vip-port"},
			wantPortID: "vip-port",
			wantSubnet: "public-subnet",
			wantFound:  true,
		},
		{
			name:        "address not reserved in region",
			reservedIPs: []edgecloud.ReservedFixedIP{{FixedIPAddress: net.ParseIP("192.0.2.21")}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selection, found, err := selectReservedVIPPort(test.reservedIPs, address, test.currentLB)
			if (err != nil) != test.wantError {
				t.Fatalf("selectReservedVIPPort() error = %v, wantError %v", err, test.wantError)
			}
			if found != test.wantFound {
				t.Fatalf("selectReservedVIPPort() found = %t, want %t", found, test.wantFound)
			}
			if selection != nil && selection.ReservedVIPPortID != test.wantPortID {
				t.Errorf("VIP port = %q, want %q", selection.ReservedVIPPortID, test.wantPortID)
			}
			if selection != nil && selection.ReservedVIPSubnetID != test.wantSubnet {
				t.Errorf("VIP subnet = %q, want %q", selection.ReservedVIPSubnetID, test.wantSubnet)
			}
		})
	}
}

func TestBuildLoadBalancerCreateRequestUsesReservedVIPPort(t *testing.T) {
	request := buildLoadBalancerCreateRequest("looky-ingress-moscow", &ServiceOptions{
		FlavorID:  "lb1-1-2",
		NetworkID: "private-network",
		SubnetID:  "private-subnet",
		VIPPortID: "reserved-vip-port",
	})
	if request.VipPortID != "reserved-vip-port" {
		t.Fatalf("VIP port ID = %q, want reserved-vip-port", request.VipPortID)
	}
	if request.VipNetworkID != "" || request.VipSubnetID != "" {
		t.Fatalf("VIP network fields must be empty when a reserved VIP port is used: %+v", request)
	}
}

func TestEnsureLoadBalancerRejectsStaticAddressChange(t *testing.T) {
	controller := &LbaasV2{LoadBalancer: LoadBalancer{edgecenter: EdgecenterOpts{RegionID: 20}}}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			ServiceAnnotationLoadBalancerRegionID:  "20",
			ServiceAnnotationLoadBalancerIPAddress: "192.0.2.20",
		}},
		Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{
			Ingress: []corev1.LoadBalancerIngress{{IP: "192.0.2.21"}},
		}},
	}
	_, err := controller.ensureLoadBalancer(context.Background(), "test", service, nil)
	if err == nil || err.Error() != "static IP cannot change on an existing Service; delete and recreate the Service" {
		t.Fatalf("ensureLoadBalancer() error = %v", err)
	}
}
