/*
Copyright 2016 The Kubernetes Authors.

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
	"net"
	"strings"
	"time"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	edgecloudUtil "github.com/Edge-Center/edgecentercloud-go/v2/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"

	v1service "ec-ccm/internal/api/v1/service"
)

var lbFlavor = "lb1-1-2"

type LbaasV2 struct {
	LoadBalancer
}

// getLoadbalancerByName get the load balancer which is in valid status by the given name/legacy name.
func getLoadbalancerByName(ctx context.Context, client *edgecloud.Client, name string, legacyName string) (*edgecloud.Loadbalancer, error) {
	list, _, err := client.Loadbalancers.List(ctx, nil)
	if err != nil {
		return nil, err
	}

	var validLBs []edgecloud.Loadbalancer

	for _, lb := range list {
		if lb.Name == name &&
			lb.ProvisioningStatus != edgecloud.ProvisioningStatusDeleted &&
			lb.ProvisioningStatus != edgecloud.ProvisioningStatusPendingDelete {
			validLBs = append(validLBs, lb)
		}
	}

	if len(validLBs) > 1 {
		return nil, ErrMultipleResults
	}

	if len(validLBs) == 0 {
		for _, lb := range list {
			if lb.Name == legacyName &&
				lb.ProvisioningStatus != edgecloud.ProvisioningStatusDeleted &&
				lb.ProvisioningStatus != edgecloud.ProvisioningStatusPendingDelete {
				validLBs = append(validLBs, lb)
			}
		}
		if len(validLBs) > 1 {
			return nil, ErrMultipleResults
		}
		if len(validLBs) == 0 {
			return nil, ErrNotFound
		}
	}

	return &validLBs[0], nil
}

func (l *LbaasV2) prepareHealthMonitorOpts(port corev1.ServicePort) *edgecloud.HealthMonitorCreateRequest {
	monitorProtocol := edgecloud.HealthMonitorType(port.Protocol)
	switch monitorProtocol {
	case
		edgecloud.HealthMonitorTypeHTTP,
		edgecloud.HealthMonitorTypeHTTPS,
		edgecloud.HealthMonitorTypePING,
		edgecloud.HealthMonitorTypeTCP,
		edgecloud.HealthMonitorTypeTLSHello,
		edgecloud.HealthMonitorTypeUDPConnect:
	default:
		monitorProtocol = edgecloud.HealthMonitorTypeTCP
	}

	opts := &edgecloud.HealthMonitorCreateRequest{
		Type:       monitorProtocol,
		Delay:      int(l.opts.MonitorDelay.Duration.Seconds()),
		Timeout:    int(l.opts.MonitorTimeout.Duration.Seconds()),
		MaxRetries: int(l.opts.MonitorMaxRetries),
	}

	if monitorProtocol == edgecloud.HealthMonitorTypeHTTP {
		httpMethod := edgecloud.HTTPMethodGET
		opts.HTTPMethod = &httpMethod
		opts.URLPath = "/"
	}

	return opts
}

func (l *LbaasV2) deleteLoadBalancer(ctx context.Context, loadbalancerID string) error {
	_, err := edgecloudUtil.ExecuteAndExtractTaskResult(ctx, l.client.Loadbalancers.Delete, loadbalancerID, l.client, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("failed to delete loadbalancer %s: %w", loadbalancerID, err)
	}

	isExist, err := edgecloudUtil.ResourceIsExist(ctx, l.client.Loadbalancers.Get, loadbalancerID)

	if err != nil {
		return fmt.Errorf("failed to check if loadbalancer %s exists: %w", loadbalancerID, err)
	}

	if !isExist {
		return nil
	}

	return fmt.Errorf("cannot delete loadbalancer with ID %s", loadbalancerID)

}

// GetLoadBalancer returns whether the specified load balancer exists and its status
func (l *LbaasV2) GetLoadBalancer(ctx context.Context, clusterName string, service *corev1.Service) (*corev1.LoadBalancerStatus, bool, error) {
	name := l.GetLoadBalancerName(ctx, clusterName, service)
	legacyName := l.GetLoadBalancerLegacyName(ctx, clusterName, service)
	loadbalancer, err := getLoadbalancerByName(ctx, l.client, name, legacyName)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}

	status := &corev1.LoadBalancerStatus{
		Ingress: []corev1.LoadBalancerIngress{{IP: loadbalancer.VipAddress.String()}},
	}

	return status, true, nil
}

// GetLoadBalancerName returns the constructed load balancer name.
func (l *LbaasV2) GetLoadBalancerName(_ context.Context, _ string, service *corev1.Service) string {
	name := fmt.Sprintf("svc_%s_%s_%s", l.edgecenter.ClusterID, service.Namespace, service.Name)
	return strings.TrimSuffix(cutString(name), "-")
}

// GetLoadBalancerLegacyName returns the legacy load balancer name for backward compatibility.
func (l *LbaasV2) GetLoadBalancerLegacyName(_ context.Context, _ string, service *corev1.Service) string {
	return cloudprovider.DefaultLoadBalancerName(service)
}

// TODO: This code currently ignores 'region' and always creates a
// loadbalancer in only the current Edgecenter region.  We should take
// a list of regions (from config) and query/create loadbalancers in each region.

// EnsureLoadBalancer creates a new load balancer or updates the existing one.
func (l *LbaasV2) EnsureLoadBalancer(ctx context.Context, clusterName string, apiService *corev1.Service, nodes []*corev1.Node) (*corev1.LoadBalancerStatus, error) {
	serviceName := fmt.Sprintf("%s/%s", apiService.Namespace, apiService.Name)

	klog.V(4).Infof("EnsureLoadBalancer(%s, %s)", clusterName, serviceName)

	if len(nodes) == 0 {
		return nil, fmt.Errorf("there are no available nodes for LoadBalancer service %s", serviceName)
	}

	l.opts.NetworkID = getStringFromServiceAnnotation(apiService, ServiceAnnotationLoadBalancerNetworkID, l.opts.NetworkID)
	l.opts.SubnetID = getStringFromServiceAnnotation(apiService, ServiceAnnotationLoadBalancerSubnetID, l.opts.SubnetID)
	if len(l.opts.SubnetID) == 0 && len(l.opts.NetworkID) == 0 {
		// Get SubnetID automatically.
		// The LB needs to be configured with instance addresses on the same subnet, so get SubnetID by one node.
		subnetID, err := getSubnetIDForLB(ctx, l.client, *nodes[0])
		if err != nil {
			klog.Warningf("Failed to find subnet-id for loadbalancer service %s/%s: %v", apiService.Namespace, apiService.Name, err)
			return nil, fmt.Errorf("no subnet-id for service %s/%s : subnet-id not set in cloud provider config, "+
				"and failed to find subnet-id from Edgecenter: %v", apiService.Namespace, apiService.Name, err)
		}
		l.opts.SubnetID = subnetID
	}

	ports := apiService.Spec.Ports
	if len(ports) == 0 {
		return nil, fmt.Errorf("no ports provided to edgecenter load balancer")
	}

	var (
		lbClass            *LBClass
		internalAnnotation bool
		err                error
	)

	class := getStringFromServiceAnnotation(apiService, ServiceAnnotationLoadBalancerClass, "")
	if class != "" {
		lbClass = l.opts.LBClasses[class]
		if lbClass == nil {
			return nil, fmt.Errorf("invalid loadbalancer class %q", class)
		}

		// Only set the internalAnnotation to true when no FloatingNetwork information is provided
		if lbClass.FloatingNetworkID == "" && lbClass.FloatingSubnetID == "" {
			internalAnnotation = lbClass.SubnetID != ""
			if !internalAnnotation {
				return nil, fmt.Errorf("empty loadbalancer class configuration for class %q", class)
			}
		}

		klog.V(4).Infof("found loadbalancer class %q with %+v", class, lbClass)
	} else {
		internalAnnotation, err = getBoolFromServiceAnnotation(apiService, ServiceAnnotationLoadBalancerInternal, l.opts.InternalLB)
		if err != nil {
			return nil, err
		}
	}

	floatingPool := getStringFromServiceAnnotation(apiService, ServiceAnnotationLoadBalancerFloatingNetworkID, l.opts.FloatingNetworkID)
	if lbClass != nil && lbClass.FloatingNetworkID != "" {
		floatingPool = lbClass.FloatingNetworkID
		klog.V(4).Infof("found floating network id %q from class %q", floatingPool, class)
	}

	if len(floatingPool) == 0 {
		floatingPool, err = getFloatingNetworkIDForLB(ctx, l.client)
		if err != nil {
			klog.Warningf("Failed to find floating-network-id for Service %s: %v", serviceName, err)
		}
	}

	if internalAnnotation {
		klog.V(4).Infof("Ensure an internal loadbalancer service.")
	} else {
		if len(floatingPool) != 0 {
			klog.V(4).Infof("Ensure an external loadbalancer service, using floatingPool: %v", floatingPool)
		} else {
			return nil, fmt.Errorf("floating-network-id or loadbalancer.edgecenterlabs.com/floating-network-id should be specified when ensuring an external loadbalancer service")
		}
	}

	if !l.opts.UseOctavia {
		// Check for TCP protocol on each port
		for _, port := range ports {
			if port.Protocol != corev1.ProtocolTCP {
				return nil, fmt.Errorf("only TCP LoadBalancer is supported for edgecenter load balancers")
			}
		}
	}

	sourceRanges, err := v1service.GetLoadBalancerSourceRanges(apiService)
	if err != nil {
		return nil, fmt.Errorf("failed to get source ranges for loadbalancer service %s: %v", serviceName, err)
	}
	if l.opts.UseOctavia {
		klog.V(4).Info("loadBalancerSourceRanges is supported")
	} else if !v1service.IsAllowAll(sourceRanges) && !l.opts.ManageSecurityGroups {
		return nil, fmt.Errorf("source range restrictions are not supported for edgecenter load balancers without managing security groups")
	}

	// Use more meaningful name for the load balancer but still need to check the legacy name for backward compatibility.
	name := l.GetLoadBalancerName(ctx, clusterName, apiService)
	legacyName := l.GetLoadBalancerLegacyName(ctx, clusterName, apiService)
	loadbalancer, err := getLoadbalancerByName(ctx, l.client, name, legacyName)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("error getting loadbalancer for Service %s: %w", serviceName, err)
		}

		klog.V(2).Infof("Creating loadbalancer %s", name)

		loadbalancer, err = l.createLoadBalancerWithListeners(ctx, name, lbClass, ports, apiService)
		if err != nil {
			return nil, fmt.Errorf("error creating loadbalancer %s: %v", name, err)
		}
	} else {
		klog.V(2).Infof("LoadBalancer %s already exists", loadbalancer.Name)
	}

	provisioningStatus, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, loadbalancer.ID)
	if err != nil {
		return nil, fmt.Errorf("timeout when waiting for loadbalancer to be ACTIVE, current provisioning status %s: %w", provisioningStatus, err)
	}

	err = l.ensureLoadBalancerListeners(ctx, loadbalancer, ports, apiService, nodes)
	if err != nil {
		return nil, err
	}

	address := loadbalancer.VipAddress
	if !internalAnnotation {
		klog.V(2).Infof("creating floating IP")
		var existingAddr net.IP

		var lbIP string

		if len(apiService.Status.LoadBalancer.Ingress) > 0 {
			lbIP = apiService.Status.LoadBalancer.Ingress[0].IP
		}

		if lbIP != "" {
			existingAddr = net.ParseIP(lbIP)
		}
		address, err = l.EnsureFloatingIP(ctx, l.client, false, loadbalancer.VipPortID, existingAddr)
		if err != nil {
			return nil, fmt.Errorf("failed to create floating IP: %v", err)
		}

		klog.V(2).Infof("floating IP created: %s", address.String())
	}

	status := &corev1.LoadBalancerStatus{
		Ingress: []corev1.LoadBalancerIngress{{IP: address.String()}},
	}

	if l.opts.ManageSecurityGroups {
		err = l.ensureSecurityGroup(ctx, l.client, clusterName, apiService, nodes)
		if err != nil {
			return status, fmt.Errorf("failed when reconciling security groups for LB service %v/%v: %v", apiService.Namespace, apiService.Name, err)
		}
	}

	return status, nil
}

// UpdateLoadBalancer updates hosts under the specified load balancer.
func (l *LbaasV2) UpdateLoadBalancer(ctx context.Context, clusterName string, svc *corev1.Service, nodes []*corev1.Node) error {

	klog.V(4).Infof("UpdateLoadBalancer CALLED for %s/%s", svc.Namespace, svc.Name)
	klog.V(4).Infof("  Ports: %v", svc.Spec.Ports)
	klog.V(4).Infof("  Nodes: %d", len(nodes))

	l.opts.SubnetID = getStringFromServiceAnnotation(svc, ServiceAnnotationLoadBalancerSubnetID, l.opts.SubnetID)
	if l.opts.SubnetID == "" && len(nodes) > 0 {
		subnetID, err := getSubnetIDForLB(ctx, l.client, *nodes[0])
		if err != nil {
			return fmt.Errorf("failed determining subnet ID: %v", err)
		}
		l.opts.SubnetID = subnetID
	}

	if len(svc.Spec.Ports) == 0 {
		return fmt.Errorf("no ports provided to load balancer")
	}

	name := l.GetLoadBalancerName(ctx, clusterName, svc)
	legacyName := l.GetLoadBalancerLegacyName(ctx, clusterName, svc)

	lb, err := getLoadbalancerByName(ctx, l.client, name, legacyName)
	if err != nil {
		return fmt.Errorf("cannot lookup LB: %v", err)
	}
	if lb == nil {
		return fmt.Errorf("LB does not exist for service %s/%s", svc.Namespace, svc.Name)
	}

	klog.V(4).Infof("Found LB %s (ID=%s)", lb.Name, lb.ID)

	if err := l.reconcilePools(ctx, lb, svc, nodes); err != nil {
		return fmt.Errorf("pool reconciliation failed: %w", err)
	}

	if l.opts.ManageSecurityGroups {
		if err := l.updateSecurityGroup(ctx, svc, nodes); err != nil {
			return fmt.Errorf("security group update failed: %v", err)
		}
	}

	return nil
}

// EnsureLoadBalancerDeleted deletes the specified load balancer
func (l *LbaasV2) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *corev1.Service) error {
	serviceName := fmt.Sprintf("%s/%s", service.Namespace, service.Name)
	klog.V(4).Infof("EnsureLoadBalancerDeleted(%s, %s)", clusterName, serviceName)

	name := l.GetLoadBalancerName(ctx, clusterName, service)
	legacyName := l.GetLoadBalancerLegacyName(ctx, clusterName, service)
	loadbalancer, err := getLoadbalancerByName(ctx, l.client, name, legacyName)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if loadbalancer == nil {
		return nil
	}

	allListeners, err := getListenersByLoadBalancerID(ctx, l.client, loadbalancer.ID)
	if err != nil {
		return fmt.Errorf("error getting listeners for LB %s: %v", loadbalancer.ID, err)
	}

	for _, listener := range allListeners {
		err := l.ensureListenerDeleted(ctx, loadbalancer, &listener)
		if err != nil {
			return err
		}
	}

	// delete the loadbalancer and all its sub-resources.
	err = l.deleteLoadBalancer(ctx, loadbalancer.ID)
	if err != nil {
		return fmt.Errorf("failed to delete loadbalancer %s: %w", loadbalancer.ID, err)
	}

	err = waitLoadbalancerDeleted(ctx, l.client, loadbalancer.ID)
	if err != nil {
		return fmt.Errorf("failed to delete loadbalancer %s: %w", loadbalancer.ID, err)
	}

	// Delete the Security Group
	if l.opts.ManageSecurityGroups {
		err := l.EnsureSecurityGroupDeleted(ctx, service)
		if err != nil {
			return fmt.Errorf("failed to delete Security Group for loadbalancer service %s: %v", serviceName, err)
		}
	}

	shouldSaveFIP, err := getBoolFromServiceAnnotation(service, ServiceAnnotationSaveFloating, false)
	if err != nil {
		return fmt.Errorf("cant get internal annotation: %w", err)
	}

	ingress := service.Status.LoadBalancer.Ingress
	if !shouldSaveFIP && len(ingress) > 0 {
		for _, ing := range ingress {
			existingAddr := net.ParseIP(ing.IP)
			if existingAddr == nil {
				continue
			}

			_, err := l.EnsureFloatingIP(ctx, l.client, true, loadbalancer.VipPortID, existingAddr)
			if err != nil {
				klog.V(2).Infof("failed to delete floating ip for loadbalancer service %s: %v", serviceName, err)
			}
		}
	}

	return nil
}

func waitLoadbalancerActiveProvisioningStatus(ctx context.Context, client *edgecloud.Client, loadbalancerID string) (string, error) {
	var provisioningStatus string

	backoff := wait.Backoff{
		Duration: loadbalancerActiveInitDelay,
		Factor:   loadbalancerActiveFactor,
		Steps:    loadbalancerActiveSteps,
	}

	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		lb, _, err := client.Loadbalancers.Get(ctx, loadbalancerID)
		if err != nil {
			return false, err
		}

		provisioningStatus = string(lb.ProvisioningStatus)

		switch lb.ProvisioningStatus {
		case edgecloud.ProvisioningStatusActive:
			return true, nil
		case edgecloud.ProvisioningStatusError:
			return true, fmt.Errorf("loadbalancer has gone into ERROR state")
		default:
			return false, nil
		}
	})

	if errors.Is(err, wait.ErrWaitTimeout) {
		err = fmt.Errorf("loadbalancer failed to go into ACTIVE provisioning status within allotted time")
	}

	return provisioningStatus, err
}

func waitLoadbalancerDeleted(ctx context.Context, client *edgecloud.Client, loadbalancerID string) error {
	backoff := wait.Backoff{
		Duration: loadbalancerDeleteInitDelay,
		Factor:   loadbalancerDeleteFactor,
		Steps:    loadbalancerDeleteSteps,
	}
	err := wait.ExponentialBackoff(backoff, func() (bool, error) {

		isExist, err := edgecloudUtil.ResourceIsExist(ctx, client.Loadbalancers.Get, loadbalancerID)

		if err != nil {
			return false, err
		}

		return !isExist, nil
	})

	if errors.Is(err, wait.ErrWaitTimeout) {
		err = fmt.Errorf("loadbalancer failed to delete within the allotted time")
	}

	return err
}

// getSubnetIDForLB returns subnet-id for a specific node
func getSubnetIDForLB(ctx context.Context, client *edgecloud.Client, node corev1.Node) (string, error) {
	ipAddress, err := nodeAddressForLB(&node)
	if err != nil {
		return "", err
	}

	instanceID := node.Spec.ProviderID
	if ind := strings.LastIndex(instanceID, "/"); ind >= 0 {
		instanceID = instanceID[(ind + 1):]
	}

	list, _, err := client.Instances.InterfaceList(ctx, instanceID)
	if err != nil {
		return "", err
	}

	for _, intf := range list {
		for _, fixedIP := range intf.IPAssignments {
			if fixedIP.IPAddress.String() == ipAddress {
				return fixedIP.SubnetID, nil
			}
		}
	}

	return "", ErrNotFound
}
