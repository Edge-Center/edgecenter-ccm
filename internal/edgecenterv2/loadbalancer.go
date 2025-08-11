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
	"reflect"
	"strconv"
	"strings"
	"time"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	edgecloudUtil "github.com/Edge-Center/edgecentercloud-go/v2/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"

	v1service "ec-ccm/internal/api/v1/service"
)

// Note: when creating a new Loadbalancer (VM), it can take some time before it is ready for use,
// this timeout is used for waiting until the Loadbalancer provisioning status goes to ACTIVE state.
const (
	// loadbalancerActive* is configuration of exponential backoff for
	// going into ACTIVE loadbalancer provisioning status. Starting with 1
	// seconds, multiplying by 1.2 with each step and taking 19 steps at maximum
	// it will time out after 128s, which roughly corresponds to 120s
	loadbalancerActiveInitDelay = 1 * time.Second
	loadbalancerActiveFactor    = 1.2
	loadbalancerActiveSteps     = 19

	// loadbalancerDelete* is configuration of exponential backoff for
	// waiting for delete operation to complete. Starting with 1
	// seconds, multiplying by 1.2 with each step and taking 13 steps at maximum
	// it will time out after 32s, which roughly corresponds to 30s
	loadbalancerDeleteInitDelay = 1 * time.Second
	loadbalancerDeleteFactor    = 1.2
	loadbalancerDeleteSteps     = 13

	activeStatus = "ACTIVE" // nolint
	errorStatus  = "ERROR"  // nolint

	defaultLBProtocol = edgecloud.ListenerProtocolTCP
	maxNameLength     = 63

	ServiceAnnotationLoadBalancerConnLimit            = "loadbalancer.edgecenter.com/connection-limit"                 // nolint
	ServiceAnnotationLoadBalancerFloatingNetworkID    = "loadbalancer.edgecenter.com/floating-network-id"              // nolint
	ServiceAnnotationLoadBalancerFloatingSubnet       = "loadbalancer.edgecenter.com/floating-subnet"                  // nolint
	ServiceAnnotationLoadBalancerFloatingSubnetID     = "loadbalancer.edgecenter.com/floating-subnet-id"               // nolint
	ServiceAnnotationLoadBalancerInternal             = "service.beta.kubernetes.io/edgecenter-internal-load-balancer" // nolint
	ServiceAnnotationSaveFloating                     = "loadbalancer.edgecenter.com/save-floating"                    // nolint
	ServiceAnnotationLoadBalancerClass                = "loadbalancer.edgecenter.com/class"                            // nolint
	ServiceAnnotationLoadBalancerKeepFloatingIP       = "loadbalancer.edgecenter.com/keep-floatingip"                  // nolint
	ServiceAnnotationLoadBalancerPortID               = "loadbalancer.edgecenter.com/port-id"                          // nolint
	ServiceAnnotationLoadBalancerProxyEnabled         = "loadbalancer.edgecenter.com/proxy-protocol"                   // nolint
	ServiceAnnotationLoadBalancerSubnetID             = "loadbalancer.edgecenter.com/subnet-id"                        // nolint
	ServiceAnnotationLoadBalancerNetworkID            = "loadbalancer.edgecenter.com/network-id"                       // nolint
	ServiceAnnotationLoadBalancerTimeoutClientData    = "loadbalancer.edgecenter.com/timeout-client-data"              // nolint
	ServiceAnnotationLoadBalancerTimeoutMemberConnect = "loadbalancer.edgecenter.com/timeout-member-connect"           // nolint
	ServiceAnnotationLoadBalancerTimeoutMemberData    = "loadbalancer.edgecenter.com/timeout-member-data"              // nolint
	ServiceAnnotationLoadBalancerTimeoutTCPInspect    = "loadbalancer.edgecenter.com/timeout-tcp-inspect"              // nolint
	ServiceAnnotationLoadBalancerXForwardedFor        = "loadbalancer.edgecenter.com/x-forwarded-for"                  // nolint

	// ServiceAnnotationLoadBalancerInternal is the annotation used on the service
	// to indicate that we want an internal loadbalancer service.
	// If the value of ServiceAnnotationLoadBalancerInternal is false, it indicates that we want an external loadbalancer service. Default to false.
	k8sIngressTag = "k8s_ingress"
)

type ListenerTimeoutConfig struct {
	TimeoutClientData    *int
	TimeoutMemberData    *int
	TimeoutMemberConnect *int
}

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

func getListenersByLoadBalancerID(ctx context.Context, client *edgecloud.Client, loadbalancerID string) ([]edgecloud.Listener, error) {
	opts := &edgecloud.ListenerListOptions{LoadbalancerID: loadbalancerID}

	list, _, err := client.Loadbalancers.ListenerList(ctx, opts)
	if err != nil {
		return nil, err
	}

	return list, nil
}

// get listener for a port or nil if it does not exist
func getListenerForPort(existingListeners []edgecloud.Listener, protocol edgecloud.LoadbalancerListenerProtocol, port int) *edgecloud.Listener {
	for _, l := range existingListeners {
		if l.Protocol == protocol && l.ProtocolPort == port {
			return &l
		}
	}
	return nil
}

// Get pool for a listener. A listener always has exactly one pool.
func getPoolByListenerID(
	ctx context.Context,
	client *edgecloud.Client,
	loadbalancerID, listenerID string,
	details bool,
) (*edgecloud.Pool, error) {
	opts := &edgecloud.PoolListOptions{
		LoadbalancerID: loadbalancerID,
		ListenerID:     listenerID,
		Details:        details,
	}

	list, _, err := client.Loadbalancers.PoolList(ctx, opts)
	if err != nil {
		return nil, err
	}

	if len(list) == 0 {
		return nil, ErrNotFound
	}

	if len(list) > 1 {
		return nil, ErrMultipleResults
	}

	return &list[0], nil
}

// Check if a member exists for node
func memberExists(members []edgecloud.PoolMember, addr string, port int) bool {
	for _, member := range members {
		if member.Address.String() == addr && member.ProtocolPort == port {
			return true
		}
	}

	return false
}

func popListener(existingListeners []edgecloud.Listener, id string) []edgecloud.Listener {
	for i, existingListener := range existingListeners {
		if existingListener.ID == id {
			existingListeners[i] = existingListeners[len(existingListeners)-1]
			existingListeners = existingListeners[:len(existingListeners)-1]
			break
		}
	}

	return existingListeners
}

func popMember(members []edgecloud.PoolMember, addr string, port int) []edgecloud.PoolMember {
	for i, member := range members {
		if member.Address.String() == addr && member.ProtocolPort == port {
			members[i] = members[len(members)-1]
			members = members[:len(members)-1]
		}
	}
	return members
}

func getSecurityGroupName(service *corev1.Service) string {
	securityGroupName := fmt.Sprintf("lb-sg-%s-%s-%s", service.UID, service.Namespace, service.Name)
	//Edgecenter requires that the name of a security group is shorter than 63 bytes.
	if len(securityGroupName) > maxNameLength {
		securityGroupName = securityGroupName[:maxNameLength]
	}

	return strings.TrimSuffix(securityGroupName, "-")
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

type SecurityGroupRuleListOpts struct {
	ID              string                               `json:"id"`
	SecurityGroupID string                               `json:"security_group_id"`
	RemoteGroupID   string                               `json:"remote_group_id"`
	Direction       edgecloud.SecurityGroupRuleDirection `json:"direction"`
	EtherType       edgecloud.EtherType                  `json:"ethertype"`
	Protocol        edgecloud.SecurityGroupRuleProtocol  `json:"protocol"`
	PortRangeMax    int                                  `json:"port_range_max"`
	PortRangeMin    int                                  `json:"port_range_min"`
	RemoteIPPrefix  string                               `json:"remote_ip_prefix"`
}

func compareSecurityGroup(exists edgecloud.SecurityGroupRule, check SecurityGroupRuleListOpts) bool {
	if check.ID != "" && exists.ID != check.ID {
		return false
	}
	if check.SecurityGroupID != "" && exists.SecurityGroupID != check.SecurityGroupID {
		return false
	}
	if check.RemoteGroupID != "" && exists.RemoteGroupID != check.RemoteGroupID {
		return false
	}
	if check.Direction != "" && exists.Direction != check.Direction {
		return false
	}
	if check.EtherType != "" && (exists.EtherType == nil || (exists.EtherType != nil && *exists.EtherType != check.EtherType)) {
		return false
	}
	if check.Protocol != "" && (exists.Protocol == nil || (exists.Protocol != nil && *exists.Protocol != check.Protocol)) {
		return false
	}
	if check.PortRangeMax != 0 && (exists.PortRangeMax == nil || (exists.PortRangeMax != nil && *exists.PortRangeMax != check.PortRangeMax)) {
		return false
	}
	if check.PortRangeMin != 0 && (exists.PortRangeMin == nil || (exists.PortRangeMin != nil && *exists.PortRangeMin != check.PortRangeMin)) {
		return false
	}
	if check.RemoteIPPrefix != "" && (exists.RemoteIPPrefix == nil || (exists.RemoteIPPrefix != nil && *exists.RemoteIPPrefix != check.RemoteIPPrefix)) {
		return false
	}
	return true
}

func getSecurityGroupRules(
	ctx context.Context,
	client *edgecloud.Client,
	opts SecurityGroupRuleListOpts,
) ([]edgecloud.SecurityGroupRule, error) {
	var securityRulesAll []edgecloud.SecurityGroupRule
	if opts.SecurityGroupID != "" {
		securityGroup, _, err := client.SecurityGroups.Get(ctx, opts.SecurityGroupID)
		if err != nil {
			return nil, err
		}

		securityRulesAll = securityGroup.SecurityGroupRules
	} else {
		securityGroups, _, err := client.SecurityGroups.List(ctx, nil)
		if err != nil {
			return nil, err
		}

		for _, group := range securityGroups {
			securityRulesAll = append(securityRulesAll, group.SecurityGroupRules...)
		}
	}

	securityRules := securityRulesAll[:0]
	for _, rule := range securityRulesAll {
		if compareSecurityGroup(rule, opts) {
			securityRules = append(securityRules, rule)
		}
	}

	return securityRules, nil
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

func toRuleProtocol(protocol corev1.Protocol) edgecloud.SecurityGroupRuleProtocol {
	tp := edgecloud.SecurityGroupRuleProtocol(strings.ToLower(string(protocol)))
	if err := tp.IsValid(); err != nil {
		return edgecloud.SGRuleProtocolTCP
	}
	return tp
}

func toListenersProtocol(protocol corev1.Protocol) edgecloud.LoadbalancerListenerProtocol {
	switch protocol {
	case corev1.ProtocolTCP:
		return edgecloud.ListenerProtocolTCP
	case corev1.ProtocolUDP:
		return edgecloud.ListenerProtocolUDP
	default:
		return defaultLBProtocol
	}
}

func createNodeSecurityGroupRules(
	ctx context.Context,
	client *edgecloud.Client,
	nodeSecurityGroupID string,
	port int,
	protocol corev1.Protocol,
	lbSecGroup string,
	svc *corev1.Service,
) error {
	v4NodeSecGroupRuleCreateOpts := &edgecloud.RuleCreateRequest{
		Direction:       edgecloud.SGRuleDirectionIngress,
		EtherType:       edgecloud.EtherTypeIPv4,
		Protocol:        toRuleProtocol(protocol),
		SecurityGroupID: &nodeSecurityGroupID,
		RemoteGroupID:   &lbSecGroup,
		PortRangeMax:    &port,
		PortRangeMin:    &port,
	}

	_, _, err := client.SecurityGroups.RuleCreate(ctx, nodeSecurityGroupID, v4NodeSecGroupRuleCreateOpts)
	if err != nil {
		return err
	}

	v6NodeSecGroupRuleCreateOpts := &edgecloud.RuleCreateRequest{
		Direction:       edgecloud.SGRuleDirectionIngress,
		EtherType:       edgecloud.EtherTypeIPv6,
		Protocol:        toRuleProtocol(protocol),
		SecurityGroupID: &nodeSecurityGroupID,
		RemoteGroupID:   &lbSecGroup,
		PortRangeMax:    &port,
		PortRangeMin:    &port,
	}

	_, _, err = client.SecurityGroups.RuleCreate(ctx, nodeSecurityGroupID, v6NodeSecGroupRuleCreateOpts)
	if err != nil {
		return err
	}

	if svc.Spec.HealthCheckNodePort > 0 && svc.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyTypeLocal {
		port := int(svc.Spec.HealthCheckNodePort)
		sgr := &edgecloud.RuleCreateRequest{
			Direction:       edgecloud.SGRuleDirectionIngress,
			EtherType:       edgecloud.EtherTypeIPv4,
			Protocol:        edgecloud.SGRuleProtocolTCP,
			SecurityGroupID: &nodeSecurityGroupID,
			RemoteGroupID:   &lbSecGroup,
			PortRangeMax:    &port,
			PortRangeMin:    &port,
		}

		_, _, err = client.SecurityGroups.RuleCreate(ctx, nodeSecurityGroupID, sgr)
		if err != nil {
			return err
		}
	}

	return nil
}

func (l *LbaasV2) createLoadBalancerWithListeners(
	ctx context.Context,
	name string,
	lbClass *LBClass,
	ports []corev1.ServicePort,
	apiService *corev1.Service,
) (*edgecloud.Loadbalancer, error) {
	keepClientIP, err := getBoolFromServiceAnnotation(apiService, ServiceAnnotationLoadBalancerXForwardedFor, false)
	if err != nil {
		return nil, err
	}

	//meta := map[string]string{InternalClusterNameMeta: l.edgecenter.ClusterID}
	//if l.edgecenter.NoBillingTag {
	//	meta[NoBillingMeta] = "true"
	//}

	createOpts := &edgecloud.LoadbalancerCreateRequest{
		Name:   name,
		Flavor: lbFlavor,
		//Metadata: meta,
		//Tags:     []string{l.edgecenter.ClusterID, k8sIngressTag},
	}

	if lbClass != nil {
		if lbClass.NetworkID != "" {
			createOpts.VipNetworkID = lbClass.NetworkID
		}
		if lbClass.SubnetID != "" {
			createOpts.VipSubnetID = lbClass.SubnetID
		}
	} else {
		if l.opts.NetworkID != "" {
			createOpts.VipNetworkID = l.opts.NetworkID
		}
		if l.opts.SubnetID != "" {
			createOpts.VipSubnetID = l.opts.SubnetID
		}
	}

	timeoutOverrides, err := parseListenerTimeouts(apiService)
	if err != nil {
		klog.Warningf("Invalid timeout annotation on service %s/%s: %v", apiService.Namespace, apiService.Name, err)
	}

	var listenerOpts []edgecloud.LoadbalancerListenerCreateRequest
	for portIndex, port := range ports {
		listenerProtocol := toListenersProtocol(port.Protocol)
		if keepClientIP {
			listenerProtocol = edgecloud.ListenerProtocolHTTP
		}

		listenerName := cutString(fmt.Sprintf("%d_%s_listener", portIndex, name))
		listenerCreateOpt := edgecloud.LoadbalancerListenerCreateRequest{
			Name:             strings.TrimSuffix(listenerName, "-"),
			ProtocolPort:     int(port.Port),
			Protocol:         listenerProtocol,
			InsertXForwarded: keepClientIP,
		}

		if timeoutOverrides != nil {
			listenerCreateOpt.TimeoutClientData = timeoutOverrides.TimeoutClientData
			listenerCreateOpt.TimeoutMemberData = timeoutOverrides.TimeoutMemberData
			listenerCreateOpt.TimeoutMemberConnect = timeoutOverrides.TimeoutMemberConnect
		}

		listenerOpts = append(listenerOpts, listenerCreateOpt)
	}

	createOpts.Listeners = listenerOpts

	klog.Infof("Creating load balancer %s with create request %+v", name, createOpts)

	task, err := edgecloudUtil.ExecuteAndExtractTaskResult(ctx, l.client.Loadbalancers.Create, createOpts, l.client, 5*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("failed to create loadbalancer: %w", err)
	}

	lbID := task.Loadbalancers[0]
	lb, _, err := l.client.Loadbalancers.Get(ctx, lbID)
	if err != nil {
		return nil, fmt.Errorf("cannot get loadbalancer with ID: %s. Error: %w", lbID, err)
	}

	return lb, nil
}

func parseListenerTimeouts(svc *corev1.Service) (*ListenerTimeoutConfig, error) {
	getTimeout := func(key string) (*int, error) {
		if val, ok := svc.Annotations[key]; ok {
			v, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("invalid value for annotation %q: %w", key, err)
			}
			return &v, nil
		}
		return nil, nil
	}

	timeoutClientData, err := getTimeout(ServiceAnnotationLoadBalancerTimeoutClientData)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", ServiceAnnotationLoadBalancerTimeoutClientData, err)
	}
	timeoutMemberData, err := getTimeout(ServiceAnnotationLoadBalancerTimeoutMemberData)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", ServiceAnnotationLoadBalancerTimeoutMemberData, err)
	}
	timeoutMemberConnect, err := getTimeout(ServiceAnnotationLoadBalancerTimeoutMemberConnect)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", ServiceAnnotationLoadBalancerTimeoutMemberConnect, err)
	}

	return &ListenerTimeoutConfig{
		TimeoutClientData:    timeoutClientData,
		TimeoutMemberData:    timeoutMemberData,
		TimeoutMemberConnect: timeoutMemberConnect,
	}, nil
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

func (l *LbaasV2) createListener(ctx context.Context, opts *edgecloud.ListenerCreateRequest) (*edgecloud.Listener, error) {
	task, err := edgecloudUtil.ExecuteAndExtractTaskResult(ctx, l.client.Loadbalancers.ListenerCreate, opts, l.client, waitSeconds)
	if err != nil {
		return nil, fmt.Errorf("failed to create listener for loadbalancer %s: %w", opts.LoadbalancerID, err)
	}

	lbl, _, err := l.client.Loadbalancers.ListenerGet(ctx, task.Listeners[0])
	if err != nil {
		return nil, fmt.Errorf("cannot get loadbalancer listener with ID: %s. Error: %w", task.Listeners[0], err)
	}

	return lbl, nil
}

func (l *LbaasV2) deleteListener(ctx context.Context, listenerID string) error {
	task, err := edgecloudUtil.ExecuteAndExtractTaskResult(
		ctx,
		l.client.Loadbalancers.ListenerDelete,
		listenerID,
		l.client,
		waitSeconds,
	)
	if err != nil {
		return fmt.Errorf("deleteListener: failed to delete listener %q: %w", listenerID, err)
	}

	if len(task.Listeners) == 0 {
		return nil
	}

	remainingListenerID := task.Listeners[0]

	isExist, err := edgecloudUtil.ResourceIsExist(ctx, l.client.Loadbalancers.ListenerGet, remainingListenerID)
	if err != nil {
		return fmt.Errorf("deleteListener: failed to check existence of listener %q: %w", remainingListenerID, err)
	}

	if !isExist {
		return nil
	}

	return fmt.Errorf("deleteListener: listener %q still exists after deletion attempt", remainingListenerID)
}

func (l *LbaasV2) createPool(ctx context.Context, opts *edgecloud.PoolCreateRequest) (*edgecloud.Pool, error) {
	task, err := edgecloudUtil.ExecuteAndExtractTaskResult(ctx, l.client.Loadbalancers.PoolCreate, opts, l.client, waitSeconds)
	if err != nil {
		return nil, fmt.Errorf("failed to create pool for listener %s: %w", opts.ListenerID, err)
	}

	lbp, _, err := l.client.Loadbalancers.PoolGet(ctx, task.Pools[0])
	if err != nil {
		return nil, fmt.Errorf("cannot get loadbalancer pool with ID: %s. Error: %w", task.Pools[0], err)
	}

	return lbp, nil
}

func (l *LbaasV2) deletePool(ctx context.Context, poolID string) error {
	_, err := edgecloudUtil.ExecuteAndExtractTaskResult(ctx, l.client.Loadbalancers.PoolDelete, poolID, l.client, waitSeconds)
	if err != nil {
		return fmt.Errorf("failed to delete pool %s: %w", poolID, err)
	}

	isExist, err := edgecloudUtil.ResourceIsExist(ctx, l.client.Loadbalancers.PoolGet, poolID)

	if err != nil {
		return fmt.Errorf("failed to check if pool %s exists: %w", poolID, err)
	}

	if !isExist {
		return nil
	}

	return fmt.Errorf("cannot delete lbpool with ID: %s", poolID)
}

func (l *LbaasV2) createPoolMember(ctx context.Context, poolID string, opts *edgecloud.PoolMemberCreateRequest) error {
	taskResp, _, err := l.client.Loadbalancers.PoolMemberCreate(ctx, poolID, opts)
	if err != nil {
		return fmt.Errorf("failed to create pool member for pool %s: %w", poolID, err)
	}

	err = edgecloudUtil.WaitForTaskComplete(ctx, l.client, taskResp.Tasks[0], waitSeconds)
	if err != nil {
		return fmt.Errorf("error creating loadbalancer pool member %v: %w", opts, err)
	}

	return nil
}

func (l *LbaasV2) deletePoolMember(ctx context.Context, poolID string, memberID string) error {
	taskResp, _, err := l.client.Loadbalancers.PoolMemberDelete(ctx, poolID, memberID)
	if err != nil {
		return fmt.Errorf("failed to delete loadbalancer pool member %s for pool %s: %w", memberID, poolID, err)
	}

	err = edgecloudUtil.WaitForTaskComplete(ctx, l.client, taskResp.Tasks[0], waitSeconds)
	if err != nil {
		return fmt.Errorf("error deleting loadbalancer pool member %s for pool %s: %w", memberID, poolID, err)
	}

	lbp, _, err := l.client.Loadbalancers.PoolGet(ctx, poolID)
	if err != nil {
		return fmt.Errorf("cannot get loadbalancer pool with ID: %s. Error: %w", poolID, err)
	}

	for _, m := range lbp.Members {
		if m.ID == memberID {
			return fmt.Errorf("cannot delete loadbalancer pool member %s for pool %s", memberID, poolID)
		}
	}

	return nil
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

// cutString makes sure the string length doesn't exceed 63, which is usually the maximum string length in Edgecenter.
func cutString(original string) string {
	ret := original
	if len(original) > maxNameLength {
		ret = original[:maxNameLength]
	}
	return ret
}

// The LB needs to be configured with instance addresses on the same
// subnet as the LB (aka opts.SubnetID). Currently, we're just
// guessing that the node's InternalIP is the right address.
// In case no InternalIP can be found, ExternalIP is tried.
// If neither InternalIP nor ExternalIP can be found an error is
// returned.
func nodeAddressForLB(node *corev1.Node) (string, error) {
	addresses := node.Status.Addresses
	if len(addresses) == 0 {
		return "", ErrNoAddressFound
	}

	allowedAddrTypes := []corev1.NodeAddressType{corev1.NodeInternalIP, corev1.NodeExternalIP}

	for _, allowedAddrType := range allowedAddrTypes {
		for _, addr := range addresses {
			if addr.Type == allowedAddrType {
				return addr.Address, nil
			}
		}
	}

	return "", ErrNoAddressFound
}

// getStringFromServiceAnnotation searches a given v1.Service for a specific annotationKey and either returns the annotation's value or a specified defaultSetting
func getStringFromServiceAnnotation(service *corev1.Service, annotationKey string, defaultSetting string) string {
	klog.V(4).Infof("getStringFromServiceAnnotation(%v, %v, %v)", service, annotationKey, defaultSetting)
	if annotationValue, ok := service.Annotations[annotationKey]; ok {
		// if there is an annotation for this setting, set the "setting" var to it
		// annotationValue can be empty, it is working as designed
		// it makes possible for instance provisioning loadbalancer without floatingip
		klog.V(4).Infof("Found a Service Annotation: %v = %v", annotationKey, annotationValue)
		return annotationValue
	}
	//if there is no annotation, set "settings" var to the value from cloud config
	klog.V(4).Infof("Could not find a Service Annotation; falling back on cloud-config setting: %v = %v", annotationKey, defaultSetting)
	return defaultSetting
}

// getBoolFromServiceAnnotation searches a given v1.Service for a specific annotationKey and either returns the annotation's value or a specified defaultSetting
func getBoolFromServiceAnnotation(service *corev1.Service, annotationKey string, defaultSetting bool) (bool, error) {
	klog.V(4).Infof("getBoolFromServiceAnnotation(%v, %v, %v)", service, annotationKey, defaultSetting)
	if annotationValue, ok := service.Annotations[annotationKey]; ok {
		returnValue := false
		switch annotationValue {
		case "true":
			returnValue = true
		case "false":
			returnValue = false
		default:
			return returnValue, fmt.Errorf("unknown %s annotation: %v, specify \"true\" or \"false\" ", annotationKey, annotationValue)
		}

		klog.V(4).Infof("Found a Service Annotation: %v = %v", annotationKey, returnValue)
		return returnValue, nil
	}
	klog.V(4).Infof("Could not find a Service Annotation; falling back to default setting: %v = %v", annotationKey, defaultSetting)
	return defaultSetting, nil
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

// applyNodeSecurityGroupIDForLB associates the security group with all the ports on the nodes.
func applyNodeSecurityGroupIDForLB(
	ctx context.Context,
	svc edgecloud.InstancesService,
	nodes []*corev1.Node,
	securityGroup string,
) error {
	opts := &edgecloud.AssignSecurityGroupRequest{Name: securityGroup}

	for _, node := range nodes {
		nodeName := types.NodeName(node.Name)
		instance, err := getInstanceByName(ctx, svc, nodeName)
		if err != nil {
			return err
		}

		sgExists := false

		for _, isg := range instance.SecurityGroups {
			if isg.Name == securityGroup {
				sgExists = true
				break
			}
		}

		if sgExists {
			continue
		}

		_, err = svc.SecurityGroupAssign(ctx, instance.ID, opts)
		if err != nil {
			return fmt.Errorf("failed to assign security group %s to instance %s: %w", securityGroup, instance.ID, err)
		}
	}

	return nil
}

// disassociateSecurityGroupForLB removes the given security group from the instances
func disassociateSecurityGroupForLB(
	ctx context.Context,
	svc edgecloud.InstancesService,
	securityGroupID string,
	securityGroupName string,
) error {
	instances, _, err := svc.FilterBySecurityGroup(ctx, securityGroupID)
	if err != nil {
		return fmt.Errorf("cannot get instances for security group %s: %w", securityGroupID, err)
	}

	opts := &edgecloud.AssignSecurityGroupRequest{Name: securityGroupName}

	for _, instance := range instances {
		sgExists := false

		for _, isg := range instance.SecurityGroups {
			if isg.Name == securityGroupName {
				sgExists = true
				break
			}
		}

		if !sgExists {
			continue
		}

		_, err = svc.SecurityGroupUnAssign(ctx, instance.ID, opts)
		if err != nil {
			return fmt.Errorf("failed to unassign security group %s from instance %s: %w", securityGroupName, instance.ID, err)
		}
	}

	return nil
}

// getNodeSecurityGroupIDForLB lists node-security-groups for specific nodes
func getNodeSecurityGroupIDForLB(
	ctx context.Context,
	client *edgecloud.Client,
	nodes []*corev1.Node,
) ([]string, error) {
	secGroupIDs := sets.NewString()

	for _, node := range nodes {
		list, _, err := client.Instances.SecurityGroupList(ctx, string(node.UID))
		if err != nil {
			return nil, err
		}

		for _, sg := range list {
			secGroupIDs.Insert(sg.ID)
		}
	}

	return secGroupIDs.List(), nil
}

// isSecurityGroupNotFound return true while 'err' is object of edgecentercloud.ResourceNotFoundError
func isSecurityGroupNotFound(err error) bool {
	errType := reflect.TypeOf(err).String()
	errTypeSlice := strings.Split(errType, ".")
	errTypeValue := ""
	if len(errTypeSlice) != 0 {
		errTypeValue = errTypeSlice[len(errTypeSlice)-1]
	}
	if errTypeValue == "ResourceNotFoundError" {
		return true
	}

	return false
}

// getFloatingNetworkIDForLB returns a floating-network-id for cluster.
func getFloatingNetworkIDForLB(ctx context.Context, client *edgecloud.Client) (string, error) {
	list, _, err := client.Networks.List(ctx, nil)
	if err != nil {
		return "", err
	}

	var floatingNetworkIds []string
	for _, network := range list {
		if network.External {
			floatingNetworkIds = append(floatingNetworkIds, network.ID)
		}
	}

	if len(floatingNetworkIds) == 0 {
		return "", ErrNotFound
	}

	if len(floatingNetworkIds) > 1 {
		klog.V(4).Infof("find multiple external networks, pick the first one when there are no explicit configuration.")
	}

	return floatingNetworkIds[0], nil
}

func (l *LbaasV2) ensureLoadBalancerListeners(
	ctx context.Context,
	loadbalancer *edgecloud.Loadbalancer,
	ports []corev1.ServicePort,
	apiService *corev1.Service,
	nodes []*corev1.Node,
) error {
	oldListeners, err := getListenersByLoadBalancerID(ctx, l.client, loadbalancer.ID)
	if err != nil {
		return fmt.Errorf("error getting LB %s listeners: %w", loadbalancer.Name, err)
	}

	var persistence *edgecloud.LoadbalancerSessionPersistence
	affinity := apiService.Spec.SessionAffinity
	switch affinity {
	case corev1.ServiceAffinityClientIP:
		persistence = &edgecloud.LoadbalancerSessionPersistence{Type: edgecloud.SessionPersistenceSourceIP}
	case corev1.ServiceAffinityNone:
		persistence = nil
	default:
		return fmt.Errorf("unsupported load balancer affinity: %v", affinity)
	}

	lbMethod := edgecloud.LoadbalancerAlgorithm(l.opts.LBMethod)
	switch lbMethod {
	case
		edgecloud.LoadbalancerAlgorithmRoundRobin,
		edgecloud.LoadbalancerAlgorithmSourceIP,
		edgecloud.LoadbalancerAlgorithmLeastConnections:
	default:
		lbMethod = edgecloud.LoadbalancerAlgorithmRoundRobin
	}

	keepClientIP, err := getBoolFromServiceAnnotation(apiService, ServiceAnnotationLoadBalancerXForwardedFor, false)
	if err != nil {
		return fmt.Errorf("failed to get X-Forwarded-For annotation: %w", err)
	}

	timeoutOverrides, err := parseListenerTimeouts(apiService)
	if err != nil {
		klog.Warningf("Invalid timeout annotation on service %s/%s: %v", apiService.Namespace, apiService.Name, err)
	}

	for portIndex, port := range ports {
		var listener *edgecloud.Listener
		if keepClientIP {
			listener = getListenerForPort(oldListeners, edgecloud.ListenerProtocolHTTP, int(port.Port))
		} else {
			listener = getListenerForPort(oldListeners, toListenersProtocol(port.Protocol), int(port.Port))
		}

		if listener == nil {
			listenerProtocol := toListenersProtocol(port.Protocol)
			if keepClientIP {
				listenerProtocol = edgecloud.ListenerProtocolHTTP
			}
			listenerName := cutString(fmt.Sprintf("%d_%s_listener", portIndex, loadbalancer.Name))
			listenerCreateOpt := &edgecloud.ListenerCreateRequest{
				Name:             strings.TrimSuffix(listenerName, "-"),
				Protocol:         listenerProtocol,
				ProtocolPort:     int(port.Port),
				LoadbalancerID:   loadbalancer.ID,
				InsertXForwarded: keepClientIP,
			}

			if timeoutOverrides != nil {
				listenerCreateOpt.TimeoutClientData = timeoutOverrides.TimeoutClientData
				listenerCreateOpt.TimeoutMemberData = timeoutOverrides.TimeoutMemberData
				listenerCreateOpt.TimeoutMemberConnect = timeoutOverrides.TimeoutMemberConnect
			}

			klog.V(4).Infof("Creating listener for port %d using protocol: %s", int(port.Port), listenerProtocol)

			listener, err = l.createListener(ctx, listenerCreateOpt)
			if err != nil {
				return fmt.Errorf("failed to create listener for loadbalancer %s: %w", loadbalancer.ID, err)
			}

			klog.V(4).Infof("Listener %s created for loadbalancer %s", listener.ID, loadbalancer.ID)
		}

		// After all ports have been processed, remaining listeners are removed as obsolete.
		// Pop valid listeners.
		oldListeners = popListener(oldListeners, listener.ID)

		poolName := cutString(fmt.Sprintf("pool_%d_%s", portIndex, loadbalancer.Name))
		poolName = strings.TrimSuffix(poolName, "-")

		_, err = l.ensureLoadBalancerPool(ctx, loadbalancer, listener, poolName, port, apiService, persistence,
			lbMethod, nodes, keepClientIP)
		if err != nil {
			return fmt.Errorf("error ensuring pool for listener %s: %w", listener.ID, err)
		}
	}

	// All remaining listeners are obsolete, delete
	for _, listener := range oldListeners {
		klog.V(4).Infof("Deleting obsolete listener %s:", listener.ID)
		err = l.ensureListenerDeleted(ctx, loadbalancer, &listener)
		if err != nil {
			return fmt.Errorf("failed to delete obsolete listener %s: %w", listener.ID, err)
		}
	}
	return nil
}

func (l *LbaasV2) ensureLoadBalancerPool(
	ctx context.Context,
	loadbalancer *edgecloud.Loadbalancer,
	listener *edgecloud.Listener,
	poolName string,
	port corev1.ServicePort,
	apiService *corev1.Service,
	persistence *edgecloud.LoadbalancerSessionPersistence,
	lbMethod edgecloud.LoadbalancerAlgorithm,
	nodes []*corev1.Node,
	keepClientIP bool,
) (*edgecloud.Pool, error) {
	pool, err := getPoolByListenerID(ctx, l.client, loadbalancer.ID, listener.ID, true)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("error getting pool for listener %s: %v", listener.ID, err)
		}
		pool = nil
	}

	if pool == nil {
		// Use the protocol of the listener
		poolProto := edgecloud.LoadbalancerPoolProtocol(listener.Protocol)

		useProxyProtocol, err := getBoolFromServiceAnnotation(apiService,
			ServiceAnnotationLoadBalancerProxyEnabled, false)
		if err != nil {
			return nil, err
		}
		if useProxyProtocol && keepClientIP {
			return nil, fmt.Errorf("annotation %s and %s cannot be used together",
				ServiceAnnotationLoadBalancerProxyEnabled, ServiceAnnotationLoadBalancerXForwardedFor)
		}
		if keepClientIP {
			poolProto = edgecloud.LoadbalancerPoolProtocol(edgecloud.ListenerProtocolHTTP)
		}

		var healthMonitorOpts *edgecloud.HealthMonitorCreateRequest
		if l.opts.CreateMonitor {
			healthMonitorOpts = l.prepareHealthMonitorOpts(port)
		}

		createOpt := &edgecloud.PoolCreateRequest{
			LoadbalancerPoolCreateRequest: edgecloud.LoadbalancerPoolCreateRequest{
				Name:                  poolName,
				Protocol:              poolProto,
				LoadbalancerAlgorithm: lbMethod,
				ListenerID:            listener.ID,
				HealthMonitor:         healthMonitorOpts,
				SessionPersistence:    persistence,
			},
		}

		klog.V(4).Infof("Creating pool for listener %s using protocol %s", listener.ID, poolProto)

		pool, err = l.createPool(ctx, createOpt)
		if err != nil {
			return nil, fmt.Errorf("error creating pool for listener %s: %v", listener.ID, err)
		}
		provisioningStatus, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, loadbalancer.ID)
		if err != nil {
			return nil, fmt.Errorf("timeout when waiting for loadbalancer to be ACTIVE after creating pool, current provisioning status %s", provisioningStatus)
		}

	}

	klog.V(4).Infof("Pool created for listener %s: %s", listener.ID, pool.ID)

	members := pool.Members

	for _, node := range nodes {
		addr, err := nodeAddressForLB(node)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				// Node failure, do not create member
				klog.Warningf("Failed to create LB pool member for node %s: %v", node.Name, err)
				continue
			}
			return nil, fmt.Errorf("error getting address for node %s: %v", node.Name, err)
		}

		if memberExists(members, addr, int(port.NodePort)) {
			// After all members have been processed, remaining members are deleted as obsolete.
			members = popMember(members, addr, int(port.NodePort))
			continue
		}

		klog.V(4).Infof("Creating member for pool %s", pool.ID)

		member := &edgecloud.PoolMemberCreateRequest{
			ProtocolPort: int(port.NodePort),
			Address:      net.ParseIP(addr),
			SubnetID:     l.opts.SubnetID,
		}

		err = l.createPoolMember(ctx, pool.ID, member)
		if err != nil {
			return nil, fmt.Errorf("error creating LB pool member for node: %s, %v", node.Name, err)
		}

		provisioningStatus, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, loadbalancer.ID)
		if err != nil {
			return nil, fmt.Errorf("timeout when waiting for loadbalancer to be ACTIVE after creating member, current provisioning status %s", provisioningStatus)
		}

		klog.V(4).Infof("Ensured pool %s has member for %s at %s", pool.ID, node.Name, addr)
	}

	// Delete obsolete members for this pool
	for _, member := range members {
		klog.V(4).Infof("Deleting obsolete member %s for pool %s address %s", member.ID, pool.ID, member.Address)

		err = l.deletePoolMember(ctx, pool.ID, member.ID)

		if err != nil {
			return nil, fmt.Errorf("error deleting obsolete member %s for pool %s address %s: %v", member.ID, pool.ID, member.Address, err)
		}

		provisioningStatus, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, loadbalancer.ID)

		if err != nil {
			return nil, fmt.Errorf("timeout when waiting for loadbalancer to be ACTIVE after deleting member, current provisioning status %s", provisioningStatus)
		}
	}

	return pool, nil

}

func (l *LbaasV2) ensureListenerDeleted(
	ctx context.Context,
	loadbalancer *edgecloud.Loadbalancer,
	listener *edgecloud.Listener,
) error {
	// get pool for listener
	pool, err := getPoolByListenerID(ctx, l.client, loadbalancer.ID, listener.ID, false)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("error getting pool for obsolete listener %s: %v", listener.ID, err)
		}
		pool = nil
	}

	if pool != nil {
		klog.V(4).Infof("Deleting obsolete pool %s for listener %s", pool.ID, listener.ID)

		// delete pool
		err = l.deletePool(ctx, pool.ID)
		if err != nil {
			return fmt.Errorf("error deleting obsolete pool %s for listener %s: %v", pool.ID, listener.ID, err)
		}

		provisioningStatus, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, loadbalancer.ID)
		if err != nil {
			return fmt.Errorf("timeout when waiting for loadbalancer to be ACTIVE after deleting pool, current provisioning status %s", provisioningStatus)
		}
	}

	// delete listener
	err = l.deleteListener(ctx, listener.ID)
	if err != nil {
		return fmt.Errorf("error deleteting obsolete listener: %v", err)
	}

	provisioningStatus, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, loadbalancer.ID)
	if err != nil {
		return fmt.Errorf("timeout when waiting for loadbalancer to be ACTIVE after deleting listener, current provisioning status %s", provisioningStatus)
	}

	klog.V(2).Infof("Deleted obsolete listener: %s", listener.ID)

	return nil
}

// TODO: This code currently ignores 'region' and always creates a
// loadbalancer in only the current Edgecenter region.  We should take
// a list of regions (from config) and query/create loadbalancers in each region.

// EnsureLoadBalancer creates a new load balancer or updates the existing one.
func (l *LbaasV2) EnsureLoadBalancer(
	ctx context.Context,
	clusterName string,
	apiService *corev1.Service,
	nodes []*corev1.Node,
) (*corev1.LoadBalancerStatus, error) {
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
		address, err = l.EnsureFloatingIP(ctx, l.client, false, loadbalancer.VipPortID, loadbalancer.VipAddress, existingAddr)
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

func (l *LbaasV2) getFloatingIPsByAddr(
	ctx context.Context,
	client *edgecloud.Client,
	addr string,
) ([]edgecloud.FloatingIP, error) {
	list, _, err := client.Floatingips.List(ctx)
	if err != nil {
		return nil, err
	}

	var result []edgecloud.FloatingIP
	for _, fip := range list {
		if fip.FloatingIPAddress == addr {
			result = append(result, fip)
		}
	}

	return result, nil
}

func (l *LbaasV2) EnsureFloatingIP(
	ctx context.Context,
	client *edgecloud.Client,
	needDelete bool,
	portID string,
	ipAddr, existingFIP net.IP,
) (net.IP, error) {

	klog.Info("Ensuring floating IP start")

	// If needed, delete the floating IPs and return.
	if needDelete {
		fips, err := l.getFloatingIPsByAddr(ctx, client, existingFIP.String())
		if err != nil {
			return nil, err
		}

		for _, fip := range fips {
			_, err = edgecloudUtil.ExecuteAndExtractTaskResult(ctx, client.Floatingips.Delete, fip.ID, client, waitSeconds)
			if err != nil {
				return nil, err
			}
		}

		return nil, nil
	}

	fips, err := edgecloudUtil.FloatingIPsListByPortID(ctx, client, portID)
	if err != nil && !errors.Is(err, edgecloudUtil.ErrFloatingIPsNotFound) {
		return nil, err
	}

	if len(fips) > 1 {
		return nil, fmt.Errorf("more than one floating IPs for port %s found", portID)
	}

	if existingFIP == nil {
		createOpts := &edgecloud.FloatingIPCreateRequest{
			PortID: portID,
		}

		klog.Infof("Creating floating with opts %+v", createOpts)

		task, err := edgecloudUtil.ExecuteAndExtractTaskResult(ctx, client.Floatingips.Create, createOpts, client, waitSeconds)
		if err != nil {
			return nil, fmt.Errorf("error creating floatingip %+v: %s", createOpts, err.Error())
		}

		fip, _, err := client.Floatingips.Get(ctx, task.FloatingIPs[0])
		if err != nil {
			return nil, fmt.Errorf("cannot get loadbalancer with ID: %s. Error: %w", task.FloatingIPs[0], err)
		}

		return net.ParseIP(fip.FloatingIPAddress), nil
	}

	list, _, err := client.Floatingips.List(ctx)
	if err != nil {
		return nil, err
	}

	var fipID string
	for _, f := range list {
		if f.FloatingIPAddress == existingFIP.String() {
			fipID = f.ID
			break
		}
	}

	if fipID == "" {
		return nil, fmt.Errorf("floating ip %s does not exist", existingFIP.String())
	}

	createOpts := &edgecloud.AssignFloatingIPRequest{
		PortID: portID,
	}

	klog.Infof("fip assign fipID %s opts %+v", fipID, createOpts)

	fip, _, err := client.Floatingips.Assign(ctx, fipID, createOpts)
	if err != nil {
		return nil, err
	}

	return net.ParseIP(fip.FloatingIPAddress), nil
}

// ensureSecurityGroup ensures security group exist for specific loadbalancer service.
// Creating security group for specific loadbalancer service when it does not exist.
func (l *LbaasV2) ensureSecurityGroup(
	ctx context.Context,
	client *edgecloud.Client,
	clusterName string,
	apiService *corev1.Service,
	nodes []*corev1.Node,
) error {
	// find node-security-group for service
	var err error
	// get service ports
	ports := apiService.Spec.Ports
	if len(ports) == 0 {
		return fmt.Errorf("no ports provided to edgecenter load balancer")
	}

	// ensure security group for LB
	lbSecGroupName := getSecurityGroupName(apiService)

	list, _, err := client.SecurityGroups.List(ctx, nil)
	if err != nil {
		return err
	}

	var lbSecGroupID string
	for _, sg := range list {
		if sg.Name == lbSecGroupName {
			lbSecGroupID = sg.ID
			break
		}
	}

	if lbSecGroupID == "" {
		// create security group
		description := fmt.Sprintf("Security Group for %s/%s Service LoadBalancer in cluster %s",
			apiService.Namespace, apiService.Name, clusterName)

		createOpts := &edgecloud.SecurityGroupCreateRequest{
			SecurityGroup: edgecloud.SecurityGroupCreateRequestInner{
				Name:        lbSecGroupName,
				Description: &description,
			},
		}

		lbSecGroup, _, err := client.SecurityGroups.Create(ctx, createOpts)
		if err != nil {
			return fmt.Errorf("failed to create Security Group for loadbalancer service %s/%s: opts: %+v %v",
				apiService.Namespace, apiService.Name, createOpts, err)
		}
		lbSecGroupID = lbSecGroup.ID
	}

	// ensure rules for node security group
	for _, port := range ports {
		// If Octavia is used, the VIP port security group is already taken good care of, we only need to allow ingress
		// traffic from Octavia amphorae to the node port on the worker nodes.
		subnet, _, err := client.Subnetworks.Get(ctx, l.opts.SubnetID)
		if err != nil {
			return fmt.Errorf("failed to find subnet %s from edgecenter API: %w", l.opts.SubnetID, err)
		}

		sgListOpts := SecurityGroupRuleListOpts{
			Direction:       edgecloud.SGRuleDirectionIngress,
			Protocol:        toRuleProtocol(port.Protocol),
			PortRangeMax:    int(port.NodePort),
			PortRangeMin:    int(port.NodePort),
			RemoteIPPrefix:  subnet.CIDR,
			SecurityGroupID: lbSecGroupID,
		}

		sgRules, err := getSecurityGroupRules(ctx, l.client, sgListOpts)
		if err != nil {
			return fmt.Errorf("failed to find security group rules in %s: %v", lbSecGroupID, err)
		}

		if len(sgRules) != 0 {
			continue
		}

		// The Octavia amphorae and worker nodes are supposed to be in the same subnet. We allow the ingress traffic
		// from the amphorae to the specific node port on the nodes.
		cidr := subnet.CIDR
		rPort := int(port.NodePort)

		description := fmt.Sprintf("Security Group for %s/%s Service LoadBalancer in cluster %s", apiService.Namespace, apiService.Name, clusterName)

		createOpts := &edgecloud.RuleCreateRequest{
			Direction:       edgecloud.SGRuleDirectionIngress,
			PortRangeMax:    &rPort,
			PortRangeMin:    &rPort,
			Protocol:        toRuleProtocol(port.Protocol),
			RemoteIPPrefix:  &cidr,
			SecurityGroupID: &lbSecGroupID,
			EtherType:       edgecloud.EtherTypeIPv4,
			Description:     &description,
		}

		klog.Infof("Create sg rule %s opts %+v", lbSecGroupID, createOpts)
		_, _, err = client.SecurityGroups.RuleCreate(ctx, lbSecGroupID, createOpts)
		if err != nil {
			return fmt.Errorf("failed to create rule for security group %s: %v", lbSecGroupID, err)
		}

		err = applyNodeSecurityGroupIDForLB(ctx, l.client.Instances, nodes, lbSecGroupName)
		if err != nil {
			return err
		}
	}

	return nil
}

// UpdateLoadBalancer updates hosts under the specified load balancer.
func (l *LbaasV2) UpdateLoadBalancer(ctx context.Context, clusterName string, service *corev1.Service, nodes []*corev1.Node) error {
	serviceName := fmt.Sprintf("%s/%s", service.Namespace, service.Name)
	klog.V(4).Infof("UpdateLoadBalancer(%v, %s, %v)", clusterName, serviceName, nodes)

	l.opts.SubnetID = getStringFromServiceAnnotation(service, ServiceAnnotationLoadBalancerSubnetID, l.opts.SubnetID)
	if len(l.opts.SubnetID) == 0 && len(nodes) > 0 {
		// Get SubnetID automatically.
		// The LB needs to be configured with instance addresses on the same subnet, so get SubnetID by one node.
		subnetID, err := getSubnetIDForLB(ctx, l.client, *nodes[0])
		if err != nil {
			klog.Warningf("Failed to find subnet-id for loadbalancer service %s/%s: %v", service.Namespace, service.Name, err)
			return fmt.Errorf("no subnet-id for service %s/%s : subnet-id not set in cloud provider config, "+
				"and failed to find subnet-id from Edgecenter: %v", service.Namespace, service.Name, err)
		}
		l.opts.SubnetID = subnetID
	}

	ports := service.Spec.Ports
	if len(ports) == 0 {
		return fmt.Errorf("no ports provided to edgecenter load balancer")
	}

	name := l.GetLoadBalancerName(ctx, clusterName, service)
	legacyName := l.GetLoadBalancerLegacyName(ctx, clusterName, service)
	loadbalancer, err := getLoadbalancerByName(ctx, l.client, name, legacyName)
	if err != nil {
		return err
	}
	if loadbalancer == nil {
		return fmt.Errorf("loadbalancer does not exist for Service %s", serviceName)
	}

	// Get all listeners for this loadbalancer, by "port key".
	type portKey struct {
		Protocol edgecloud.LoadbalancerListenerProtocol
		Port     int
	}
	var listenerIDs []string
	lbListeners := make(map[portKey]edgecloud.Listener)
	allListeners, err := getListenersByLoadBalancerID(ctx, l.client, loadbalancer.ID)
	if err != nil {
		return fmt.Errorf("error getting listeners for LB %s: %v", loadbalancer.ID, err)
	}
	for _, l := range allListeners {
		key := portKey{Protocol: l.Protocol, Port: l.ProtocolPort}
		lbListeners[key] = l
		listenerIDs = append(listenerIDs, l.ID)
	}

	// Get all pools for this loadbalancer, by listener ID.
	lbPools := make(map[string]edgecloud.Pool)
	for _, listenerID := range listenerIDs {
		// with details=true we get pool member right from pool struct
		pool, err := getPoolByListenerID(ctx, l.client, loadbalancer.ID, listenerID, true)
		if err != nil {
			return fmt.Errorf("error getting pool for listener %s: %v", listenerID, err)
		}
		lbPools[listenerID] = *pool
	}

	// Compose Set of member (addresses) that _should_ exist
	addresses := make(map[string]*corev1.Node)
	for _, node := range nodes {
		addr, err := nodeAddressForLB(node)
		if err != nil {
			return err
		}
		addresses[addr] = node
	}

	// Check for adding/removing members associated with each port
	for _, port := range ports {
		// Get listener associated with this port
		listener, ok := lbListeners[portKey{
			Protocol: toListenersProtocol(port.Protocol),
			Port:     int(port.Port),
		}]
		if !ok {
			return fmt.Errorf("loadbalancer %s does not contain required listener for port %d and protocol %s", loadbalancer.ID, port.Port, port.Protocol)
		}

		// Get pool associated with this listener
		pool, ok := lbPools[listener.ID]
		if !ok {
			return fmt.Errorf("loadbalancer %s does not contain required pool for listener %s", loadbalancer.ID, listener.ID)
		}

		members := make(map[string]edgecloud.PoolMember)
		for _, member := range pool.Members {
			members[member.Address.String()] = member
		}

		// Add any new members for this port
		for addr := range addresses {
			if member, ok := members[addr]; ok && member.ProtocolPort == int(port.NodePort) {
				// Already exists, do not create member
				continue
			}

			createOpts := &edgecloud.PoolMemberCreateRequest{
				Address:      net.ParseIP(addr),
				ProtocolPort: int(port.NodePort),
				SubnetID:     l.opts.SubnetID,
			}

			err = l.createPoolMember(ctx, pool.ID, createOpts)
			if err != nil {
				return err
			}

			provisioningStatus, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, loadbalancer.ID)
			if err != nil {
				return fmt.Errorf("timeout when waiting for loadbalancer to be ACTIVE after creating member, current provisioning status %s", provisioningStatus)
			}
		}

		// Remove any old members for this port
		for _, member := range members {
			if _, ok := addresses[member.Address.String()]; ok && member.ProtocolPort == int(port.NodePort) {
				// Still present, do not delete member
				continue
			}

			err = l.deletePoolMember(ctx, pool.ID, member.ID)
			if err != nil {
				return err
			}

			provisioningStatus, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, loadbalancer.ID)
			if err != nil {
				return fmt.Errorf("timeout when waiting for loadbalancer to be ACTIVE after deleting member, current provisioning status %s", provisioningStatus)
			}
		}
	}

	if l.opts.ManageSecurityGroups {
		err := l.updateSecurityGroup(ctx, service, nodes)
		if err != nil {
			return fmt.Errorf("failed to update Security Group for loadbalancer service %s: %v", serviceName, err)
		}
	}

	return nil
}

// updateSecurityGroup updating security group for specific loadbalancer service.
func (l *LbaasV2) updateSecurityGroup(
	ctx context.Context,
	apiService *corev1.Service,
	nodes []*corev1.Node,
) error {
	originalNodeSecurityGroupIDs := l.opts.NodeSecurityGroupIDs

	var err error
	l.opts.NodeSecurityGroupIDs, err = getNodeSecurityGroupIDForLB(ctx, l.client, nodes)
	if err != nil {
		return fmt.Errorf("failed to find node-security-group for loadbalancer service %s/%s: %v", apiService.Namespace, apiService.Name, err)
	}
	klog.V(4).Infof("find node-security-group %v for loadbalancer service %s/%s", l.opts.NodeSecurityGroupIDs, apiService.Namespace, apiService.Name)

	original := sets.NewString(originalNodeSecurityGroupIDs...)
	current := sets.NewString(l.opts.NodeSecurityGroupIDs...)
	removals := original.Difference(current)

	// Generate Name
	lbSecGroupName := getSecurityGroupName(apiService)

	list, _, err := l.client.SecurityGroups.List(ctx, nil)
	if err != nil {
		return err
	}

	var lbSecGroupID string
	for _, sg := range list {
		if sg.Name == lbSecGroupName {
			lbSecGroupID = sg.ID
			break
		}
	}

	if lbSecGroupID == "" {
		return fmt.Errorf("error occurred finding security group: %s: %v", lbSecGroupName, err)
	}

	ports := apiService.Spec.Ports
	if len(ports) == 0 {
		return fmt.Errorf("no ports provided to edgecenter load balancer")
	}

	for _, port := range ports {
		for removal := range removals {
			// Delete the rules in the Node Security Group
			opts := SecurityGroupRuleListOpts{
				Direction:       edgecloud.SGRuleDirectionIngress,
				SecurityGroupID: removal,
				RemoteGroupID:   lbSecGroupID,
				PortRangeMax:    int(port.NodePort),
				PortRangeMin:    int(port.NodePort),
				Protocol:        edgecloud.SecurityGroupRuleProtocol(port.Protocol),
			}

			secGroupRules, err := getSecurityGroupRules(ctx, l.client, opts)
			if err != nil {
				return fmt.Errorf("error finding rules for remote group id %s in security group id %s: %v", lbSecGroupID, removal, err)
			}

			for _, rule := range secGroupRules {
				_, err = l.client.SecurityGroups.Delete(ctx, rule.ID)
				if err != nil {
					return fmt.Errorf("error occurred deleting security group rule: %s: %w", rule.ID, err)
				}
			}
		}

		for _, nodeSecurityGroupID := range l.opts.NodeSecurityGroupIDs {
			opts := SecurityGroupRuleListOpts{
				Direction:       edgecloud.SGRuleDirectionIngress,
				SecurityGroupID: nodeSecurityGroupID,
				RemoteGroupID:   lbSecGroupID,
				PortRangeMax:    int(port.NodePort),
				PortRangeMin:    int(port.NodePort),
				Protocol:        edgecloud.SecurityGroupRuleProtocol(port.Protocol),
			}

			secGroupRules, err := getSecurityGroupRules(ctx, l.client, opts)
			if err != nil {
				return fmt.Errorf("error finding rules for remote group id %s in security group id %s: %v", lbSecGroupID, nodeSecurityGroupID, err)
			}

			if len(secGroupRules) != 0 {
				// Do not add rule when find rules for remote group in the Node Security Group
				continue
			}

			// Add the rules in the Node Security Group
			err = createNodeSecurityGroupRules(ctx, l.client, nodeSecurityGroupID, int(port.NodePort), port.Protocol, lbSecGroupID, apiService)
			if err != nil {
				return fmt.Errorf("error occurred creating security group for loadbalancer service %s/%s: %v", apiService.Namespace, apiService.Name, err)
			}
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

			_, err := l.EnsureFloatingIP(ctx, l.client, true, loadbalancer.VipPortID, loadbalancer.VipAddress, existingAddr)
			if err != nil {
				klog.V(2).Infof("failed to delete floating ip for loadbalancer service %s: %v", serviceName, err)
			}
		}
	}

	return nil
}

// EnsureSecurityGroupDeleted deleting security group for specific loadbalancer service.
func (l *LbaasV2) EnsureSecurityGroupDeleted(ctx context.Context, service *corev1.Service) error {
	// Generate Name
	lbSecGroupName := getSecurityGroupName(service)

	list, _, err := l.client.SecurityGroups.List(ctx, nil)
	if err != nil {
		return err
	}

	var lbSecGroupID string
	for _, sg := range list {
		if sg.Name == lbSecGroupName {
			lbSecGroupID = sg.ID
			break
		}
	}

	if lbSecGroupID == "" {
		return nil
	}

	if l.opts.UseOctavia {
		// Disassociate the security group from the neutron ports on the nodes.
		if err := disassociateSecurityGroupForLB(ctx, l.client.Instances, lbSecGroupID, lbSecGroupName); err != nil {
			return fmt.Errorf("failed to disassociate security group %s: %v", lbSecGroupID, err)
		}
	}

	_, err = l.client.SecurityGroups.Delete(ctx, lbSecGroupID)
	if err != nil {
		return err
	}

	if len(l.opts.NodeSecurityGroupIDs) == 0 {
		// Just happen when nodes have no Security Group, or should not happen
		// UpdateLoadBalancer and EnsureLoadBalancer can set lbaas.opts.NodeSecurityGroupIDs when it is empty
		// And service controller_openstack call UpdateLoadBalancer to set lbaas.opts.NodeSecurityGroupIDs when controller_openstack manager service is restarted.
		klog.Warningf("Can not find node-security-group from all the nodes of this cluster when delete loadbalancer service %s/%s",
			service.Namespace, service.Name)
		return nil
	}

	// Delete the rules in the Node Security Group
	for _, nodeSecurityGroupID := range l.opts.NodeSecurityGroupIDs {
		opts := SecurityGroupRuleListOpts{
			SecurityGroupID: nodeSecurityGroupID,
			RemoteGroupID:   lbSecGroupID,
		}
		secGroupRules, err := getSecurityGroupRules(ctx, l.client, opts)
		if err != nil {
			return fmt.Errorf("error finding rules for remote group id %s in security group id %s: %w", lbSecGroupID, nodeSecurityGroupID, err)
		}

		for _, rule := range secGroupRules {
			_, err = edgecloudUtil.ExecuteAndExtractTaskResult(ctx, l.client.SecurityGroups.RuleDelete, rule.ID, l.client, waitSeconds)
			if err != nil {
				return fmt.Errorf("error occurred deleting security group rule: %s: %v", rule.ID, err)
			}
		}
	}

	return nil
}
