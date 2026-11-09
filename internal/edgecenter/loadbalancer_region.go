package edgecenter

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

const ServiceAnnotationLoadBalancerRegionID = "loadbalancer.edgecenter.com/region-id"

// LBRegion follows the regional network/selector configuration introduced in
// PR #20. Static IP reservation is independent and is not required here.
type LBRegion struct {
	NetworkID         string `gcfg:"network-id"`
	SubnetID          string `gcfg:"subnet-id"`
	FloatingNetworkID string `gcfg:"floating-network-id"`
	NodeSelector      string `gcfg:"node-selector"`
}

// forService returns an isolated view; shared clients and configuration are
// never mutated by concurrent reconciliations in other regions.
func (l *LbaasV2) forService(service *corev1.Service) (*LbaasV2, error) {
	if service == nil {
		return nil, fmt.Errorf("service is nil")
	}
	regionID := l.edgecenter.RegionID
	if value, ok := service.Annotations[ServiceAnnotationLoadBalancerRegionID]; ok {
		id, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("annotation %q must contain a positive region ID", ServiceAnnotationLoadBalancerRegionID)
		}
		regionID = id
	}
	selected := nodeRegion{name: "default", regionID: l.edgecenter.RegionID, projectID: l.edgecenter.ProjectID, client: l.client}
	_, explicitRegion := service.Annotations[ServiceAnnotationLoadBalancerRegionID]
	if regionID != l.edgecenter.RegionID || (explicitRegion && l.nodeRegions != nil) {
		if l.nodeRegions == nil {
			return nil, fmt.Errorf("region %d must be configured in NodeRegion", regionID)
		}
		found := false
		for _, region := range l.nodeRegions.regions {
			if region.regionID != regionID {
				continue
			}
			if found {
				return nil, fmt.Errorf("region %d exists in multiple configured projects; Service region-id is ambiguous", regionID)
			}
			selected, found = region, true
		}
		if !found {
			return nil, fmt.Errorf("region %d must be configured in NodeRegion", regionID)
		}
	}
	regional := *l
	regional.client = selected.client
	regional.edgecenter.RegionID = selected.regionID
	regional.edgecenter.ProjectID = selected.projectID
	regional.scopeName = selected.name
	regional.nodeSelector = nil
	settings := l.opts.LBRegions[strconv.Itoa(regionID)]
	foreign := selected.name != "default"
	if foreign && (settings == nil || strings.TrimSpace(settings.NodeSelector) == "" || (settings.NetworkID == "" && settings.SubnetID == "")) {
		return nil, fmt.Errorf("LoadBalancerRegion %q must define node-selector and network-id or subnet-id", strconv.Itoa(regionID))
	}
	if foreign {
		// Network IDs are regional. Never inherit the primary region's IDs.
		regional.opts.NetworkID = ""
		regional.opts.SubnetID = ""
		regional.opts.FloatingNetworkID = ""
		regional.opts.FloatingSubnetID = ""
	}
	if settings != nil {
		if value := strings.TrimSpace(settings.NodeSelector); value != "" {
			selector, err := labels.Parse(value)
			if err != nil {
				return nil, fmt.Errorf("invalid regional node-selector: %w", err)
			}
			regional.nodeSelector = selector
		}
		if settings.NetworkID != "" {
			regional.opts.NetworkID = settings.NetworkID
		}
		if settings.SubnetID != "" {
			regional.opts.SubnetID = settings.SubnetID
		}
		if settings.FloatingNetworkID != "" {
			regional.opts.FloatingNetworkID = settings.FloatingNetworkID
		}
	}
	return &regional, nil
}

func (l *LbaasV2) selectServiceNodes(ctx context.Context, nodes []*corev1.Node) ([]*corev1.Node, error) {
	if l.nodeRegions == nil {
		return nil, fmt.Errorf("node region resolver is not configured")
	}
	var filtered []*corev1.Node
	target := l.scopeName
	if target == "" {
		target = "default"
	}
	for _, node := range nodes {
		if node == nil {
			return nil, fmt.Errorf("nil node in load balancer candidates")
		}
		if l.nodeSelector != nil && !l.nodeSelector.Matches(labels.Set(node.Labels)) {
			continue
		}
		_, region, err := l.nodeRegions.resolve(ctx, node)
		if err != nil {
			return nil, err
		}
		if region.name != target {
			if l.nodeSelector != nil {
				return nil, fmt.Errorf("node %s matches LB selector but belongs to scope %q, expected %q", node.Name, region.name, target)
			}
			continue
		}
		filtered = append(filtered, node)
	}
	return filtered, nil
}

func (l *LbaasV2) EnsureLoadBalancer(ctx context.Context, clusterName string, service *corev1.Service, nodes []*corev1.Node) (*corev1.LoadBalancerStatus, error) {
	regional, err := l.forService(service)
	if err != nil {
		return nil, err
	}
	return regional.ensureLoadBalancer(ctx, clusterName, service, nodes)
}

func (l *LbaasV2) GetLoadBalancer(ctx context.Context, clusterName string, service *corev1.Service) (*corev1.LoadBalancerStatus, bool, error) {
	regional, err := l.forService(service)
	if err != nil {
		return nil, false, err
	}
	return regional.getLoadBalancer(ctx, clusterName, service)
}

func (l *LbaasV2) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *corev1.Service) error {
	regional, err := l.forService(service)
	if err != nil {
		return err
	}
	return regional.ensureLoadBalancerDeleted(ctx, clusterName, service)
}
