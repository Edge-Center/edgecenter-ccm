package edgecenter

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// resolveServiceRegionAndIPAddress reads the optional regional load balancer
// settings from a Service. Both annotations must be present together.
func resolveServiceRegionAndIPAddress(service *corev1.Service, defaultRegionID int) (int, net.IP, error) {
	regionValue, hasRegion := service.Annotations[ServiceAnnotationLoadBalancerRegionID]
	addressValue, hasAddress := service.Annotations[ServiceAnnotationLoadBalancerIPAddress]
	if hasRegion != hasAddress {
		return 0, nil, fmt.Errorf("annotations %q and %q must be set together", ServiceAnnotationLoadBalancerRegionID, ServiceAnnotationLoadBalancerIPAddress)
	}
	if !hasRegion {
		return defaultRegionID, nil, nil
	}

	regionID, err := strconv.Atoi(strings.TrimSpace(regionValue))
	if err != nil || regionID <= 0 {
		return 0, nil, fmt.Errorf("annotation %q must contain a positive Edgecenter region ID", ServiceAnnotationLoadBalancerRegionID)
	}

	address := net.ParseIP(strings.TrimSpace(addressValue))
	if address == nil || address.To4() == nil {
		return 0, nil, fmt.Errorf("annotation %q must contain a valid IPv4 address", ServiceAnnotationLoadBalancerIPAddress)
	}

	return regionID, address, nil
}

// forService returns a load balancer client that uses the Service's region.
func (l *LbaasV2) forService(service *corev1.Service) (*LbaasV2, error) {
	regionID, staticIP, err := resolveServiceRegionAndIPAddress(service, l.edgecenter.RegionID)
	if err != nil {
		return nil, err
	}
	loadBalancer := l.LoadBalancer
	regionOpts := loadBalancer.opts.LBRegions[strconv.Itoa(regionID)]
	if staticIP != nil {
		if regionOpts == nil || strings.TrimSpace(regionOpts.NodeSelector) == "" {
			return nil, fmt.Errorf("LoadBalancerRegion %q must define node-selector for a regional static IP", strconv.Itoa(regionID))
		}
		if regionOpts.NetworkID == "" && regionOpts.SubnetID == "" {
			return nil, fmt.Errorf("LoadBalancerRegion %q must define network-id or subnet-id for a regional static IP", strconv.Itoa(regionID))
		}
	}
	if regionID == l.edgecenter.RegionID && regionOpts == nil {
		return l, nil
	}

	var nodeSelector labels.Selector
	if regionOpts != nil && strings.TrimSpace(regionOpts.NodeSelector) != "" {
		nodeSelector, err = labels.Parse(regionOpts.NodeSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid node-selector in LoadBalancerRegion %q: %w", strconv.Itoa(regionID), err)
		}
	}

	regionConfig := l.edgecenter
	client := l.client
	if regionID != l.edgecenter.RegionID {
		regionConfig.RegionID = regionID
		client, err = NewEdgecenterClient(regionConfig, "edgecenter-cloud-controller-manager", userAgentData...)
		if err != nil {
			return nil, err
		}
		if l.client != nil && l.client.HTTPClient != nil {
			client.HTTPClient.Timeout = l.client.HTTPClient.Timeout
		}
	}

	loadBalancer.client = client
	loadBalancer.edgecenter = regionConfig
	if regionOpts != nil {
		if regionOpts.NetworkID != "" {
			loadBalancer.opts.NetworkID = regionOpts.NetworkID
		}
		if regionOpts.SubnetID != "" {
			loadBalancer.opts.SubnetID = regionOpts.SubnetID
		}
		if regionOpts.FloatingNetworkID != "" {
			loadBalancer.opts.FloatingNetworkID = regionOpts.FloatingNetworkID
		}
	}

	return &LbaasV2{LoadBalancer: loadBalancer, nodeSelector: nodeSelector}, nil
}

func (l *LbaasV2) selectServiceNodes(nodes []*corev1.Node) ([]*corev1.Node, error) {
	if l.nodeSelector == nil {
		return nodes, nil
	}

	filtered := make([]*corev1.Node, 0, len(nodes))
	for _, node := range nodes {
		if node != nil && l.nodeSelector.Matches(labels.Set(node.Labels)) {
			filtered = append(filtered, node)
		}
	}
	if len(filtered) == 0 {
		return nil, fmt.Errorf("LoadBalancerRegion node-selector %q matched no nodes", l.nodeSelector.String())
	}

	return filtered, nil
}

// LBRegion contains network settings for a regional load balancer.
type LBRegion struct {
	NetworkID         string `gcfg:"network-id"`
	SubnetID          string `gcfg:"subnet-id"`
	FloatingNetworkID string `gcfg:"floating-network-id"`
	NodeSelector      string `gcfg:"node-selector"`
}
