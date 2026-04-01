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

var lbFlavor = "lb2-1-2"

// LbaasV2 implements Kubernetes LoadBalancer operations using Edgecenter LBaaS v2.
type LbaasV2 struct {
	LoadBalancer
}

// ServiceOptions contains resolved load balancer settings for a particular Service.
// These options are derived from service annotations, cloud config and service spec,
// and are then reused across LB creation/update phases.
type ServiceOptions struct {
	NetworkID            string
	SubnetID             string
	FloatingNetworkID    string
	Internal             bool
	KeepClientIP         bool
	UseProxyProtocol     bool
	SecretID             string
	Timeouts             *ListenerTimeoutConfig
	LBMethod             edgecloud.LoadbalancerAlgorithm
	SessionPersistence   *edgecloud.LoadbalancerSessionPersistence
	CreateMonitor        bool
	ManageSecurityGroups bool
	LBClass              *LBClass
	NodeSecurityGroupIDs []string
}

// EnsureLoadBalancer creates or updates the load balancer for the Service.
// The flow is split into phases: LB -> listeners -> pools -> pool members -> obsolete cleanup.
func (l *LbaasV2) EnsureLoadBalancer(ctx context.Context, clusterName string, apiService *corev1.Service, nodes []*corev1.Node) (*corev1.LoadBalancerStatus, error) {
	serviceName := fmt.Sprintf("%s/%s", apiService.Namespace, apiService.Name)
	klog.V(2).Infof("[LB] === START EnsureLoadBalancer for %s ===", serviceName)

	if len(nodes) == 0 {
		err := fmt.Errorf("there are no available nodes for LoadBalancer service %s", serviceName)
		klog.Errorf("[LB][%s] FAILED before phase start: %v", serviceName, err)
		return nil, err
	}

	ports := apiService.Spec.Ports
	if len(ports) == 0 {
		err := fmt.Errorf("no ports provided to edgecenter load balancer")
		klog.Errorf("[LB][%s] FAILED before phase start: %v", serviceName, err)
		return nil, err
	}

	logLBPhaseStart(serviceName, "resolveServiceOptions")
	opts, err := l.resolveServiceOptions(ctx, apiService, nodes)
	if err != nil {
		logLBPhaseFailed(serviceName, "resolveServiceOptions", err)
		return nil, err
	}
	logLBPhaseDone(serviceName, "resolveServiceOptions", "")

	logLBPhaseStart(serviceName, "ensureLoadBalancer")

	name := l.GetLoadBalancerName(ctx, clusterName, apiService)
	legacyName := l.GetLoadBalancerLegacyName(ctx, clusterName, apiService)

	loadbalancer, err := getLoadbalancerByName(ctx, l.client, name, legacyName)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			err = fmt.Errorf("error getting loadbalancer for Service %s: %w", serviceName, err)
			logLBPhaseFailed(serviceName, "ensureLoadBalancer", err)
			return nil, err
		}

		klog.V(2).Infof("[LB][%s] Creating loadbalancer %s", serviceName, name)
		loadbalancer, err = l.createLoadBalancer(ctx, name, opts)
		if err != nil {
			err = fmt.Errorf("error creating loadbalancer %s: %v", name, err)
			logLBPhaseFailed(serviceName, "ensureLoadBalancer", err)
			return nil, err
		}
	} else {
		klog.V(2).Infof("[LB][%s] LoadBalancer %s already exists", serviceName, loadbalancer.Name)
	}
	logLBPhaseDone(serviceName, "ensureLoadBalancer", "")

	logLBWaitStart(serviceName, "LB ACTIVE (post-create)")
	provisioningStatus, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, loadbalancer.ID)
	if err != nil {
		err = fmt.Errorf("timeout when waiting for loadbalancer to be ACTIVE, current provisioning status %s: %w", provisioningStatus, err)
		logLBWaitFailed(serviceName, "LB ACTIVE (post-create)", err)
		return nil, err
	}
	logLBWaitDone(serviceName, "LB ACTIVE (post-create)")

	logLBPhaseStart(serviceName, "ensureListeners")
	listeners, obsoleteListeners, err := l.ensureLoadBalancerListeners(ctx, loadbalancer, ports, apiService, opts)
	if err != nil {
		logLBPhaseFailed(serviceName, "ensureListeners", err)
		return nil, err
	}
	logLBPhaseDone(serviceName, "ensureListeners", "(total=%d, obsolete=%d)", len(listeners), len(obsoleteListeners))

	logLBWaitStart(serviceName, "LB ACTIVE (post-listeners)")
	provisioningStatus, err = waitLoadbalancerActiveProvisioningStatus(ctx, l.client, loadbalancer.ID)
	if err != nil {
		err = fmt.Errorf("timeout when waiting for loadbalancer to be ACTIVE after listeners phase, current provisioning status %s: %w", provisioningStatus, err)
		logLBWaitFailed(serviceName, "LB ACTIVE (post-listeners)", err)
		return nil, err
	}
	logLBWaitDone(serviceName, "LB ACTIVE (post-listeners)")

	logLBPhaseStart(serviceName, "ensurePools")
	pools, obsoletePools, err := l.ensureLoadBalancerPools(ctx, loadbalancer, apiService, listeners, opts)
	if err != nil {
		logLBPhaseFailed(serviceName, "ensurePools", err)
		return nil, err
	}
	logLBPhaseDone(serviceName, "ensurePools", "(total=%d, obsolete=%d)", len(pools), len(obsoletePools))

	logLBWaitStart(serviceName, "LB ACTIVE (post-pools)")
	provisioningStatus, err = waitLoadbalancerActiveProvisioningStatus(ctx, l.client, loadbalancer.ID)
	if err != nil {
		err = fmt.Errorf("timeout when waiting for loadbalancer to be ACTIVE after pools phase, current provisioning status %s: %w", provisioningStatus, err)
		logLBWaitFailed(serviceName, "LB ACTIVE (post-pools)", err)
		return nil, err
	}
	logLBWaitDone(serviceName, "LB ACTIVE (post-pools)")

	logLBPhaseStart(serviceName, "reconcileMembers")
	if err := l.reconcilePoolMembers(ctx, loadbalancer, apiService, pools, opts, nodes); err != nil {
		logLBPhaseFailed(serviceName, "reconcileMembers", err)
		return nil, err
	}
	logLBPhaseDone(serviceName, "reconcileMembers", "")

	logLBPhaseStart(serviceName, "cleanupObsolete")
	if err := l.deleteObsoleteListenersAndPools(ctx, loadbalancer, obsoleteListeners, obsoletePools); err != nil {
		logLBPhaseFailed(serviceName, "cleanupObsolete", err)
		return nil, err
	}
	logLBPhaseDone(serviceName, "cleanupObsolete", "")

	address := loadbalancer.VipAddress
	if !opts.Internal {
		logLBPhaseStart(serviceName, "ensureFloatingIP")
		address, err = l.ensureServiceFloatingIP(ctx, apiService, loadbalancer, opts)
		if err != nil {
			logLBPhaseFailed(serviceName, "ensureFloatingIP", err)
			return nil, err
		}
		logLBPhaseDone(serviceName, "ensureFloatingIP", "(ip=%s)", address.String())
	}

	if opts.ManageSecurityGroups {
		logLBPhaseStart(serviceName, "ensureSecurityGroup")
		if err := l.ensureSecurityGroup(ctx, l.client, clusterName, apiService, nodes, opts); err != nil {
			err = fmt.Errorf("failed when reconciling security groups for LB service %v/%v: %v",
				apiService.Namespace, apiService.Name, err)
			logLBPhaseFailed(serviceName, "ensureSecurityGroup", err)
			return nil, err
		}
		logLBPhaseDone(serviceName, "ensureSecurityGroup", "")
	}

	status := &corev1.LoadBalancerStatus{
		Ingress: []corev1.LoadBalancerIngress{{IP: address.String()}},
	}

	klog.V(2).Infof("[LB] === DONE EnsureLoadBalancer for %s (ip=%s) ===", serviceName, address.String())
	return status, nil
}

// resolveServiceOptions resolves all load balancer settings for the given Service.
// It merges cloud config defaults, service annotations, service spec and LB class settings
// into a single normalized structure used by the rest of the reconciliation flow.
func (l *LbaasV2) resolveServiceOptions(ctx context.Context, svc *corev1.Service, nodes []*corev1.Node) (*ServiceOptions, error) {
	opts := &ServiceOptions{
		NetworkID:            getStringFromServiceAnnotation(svc, ServiceAnnotationLoadBalancerNetworkID, l.opts.NetworkID),
		SubnetID:             getStringFromServiceAnnotation(svc, ServiceAnnotationLoadBalancerSubnetID, l.opts.SubnetID),
		FloatingNetworkID:    getStringFromServiceAnnotation(svc, ServiceAnnotationLoadBalancerFloatingNetworkID, l.opts.FloatingNetworkID),
		CreateMonitor:        l.opts.CreateMonitor,
		ManageSecurityGroups: l.opts.ManageSecurityGroups,
	}

	if len(opts.SubnetID) == 0 && len(opts.NetworkID) == 0 {
		subnetID, err := getSubnetIDForLB(ctx, l.client, *nodes[0])
		if err != nil {
			return nil, fmt.Errorf("no subnet-id for service %s/%s: subnet-id not set in cloud provider config, and failed to find subnet-id from Edgecenter: %v",
				svc.Namespace, svc.Name, err)
		}
		opts.SubnetID = subnetID
	}

	var (
		lbClass            *LBClass
		internalAnnotation bool
		err                error
	)

	class := getStringFromServiceAnnotation(svc, ServiceAnnotationLoadBalancerClass, "")
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

		if lbClass.NetworkID != "" {
			opts.NetworkID = lbClass.NetworkID
		}
		if lbClass.SubnetID != "" {
			opts.SubnetID = lbClass.SubnetID
		}
		if lbClass.FloatingNetworkID != "" {
			opts.FloatingNetworkID = lbClass.FloatingNetworkID
		}
		opts.LBClass = lbClass
	} else {
		internalAnnotation, err = getBoolFromServiceAnnotation(svc, ServiceAnnotationLoadBalancerInternal, l.opts.InternalLB)
		if err != nil {
			return nil, err
		}
	}

	opts.Internal = internalAnnotation

	if len(opts.FloatingNetworkID) == 0 && !opts.Internal {
		floatingPool, err := getFloatingNetworkIDForLB(ctx, l.client)
		if err != nil {
			klog.Warningf("Failed to find floating-network-id for Service %s/%s: %v", svc.Namespace, svc.Name, err)
		} else {
			opts.FloatingNetworkID = floatingPool
		}
	}

	if !opts.Internal && len(opts.FloatingNetworkID) == 0 {
		return nil, fmt.Errorf("floating-network-id or %s should be specified when ensuring an external loadbalancer service",
			ServiceAnnotationLoadBalancerFloatingNetworkID)
	}

	if !l.opts.UseOctavia {
		// Check for TCP protocol on each port
		for _, port := range svc.Spec.Ports {
			if port.Protocol != corev1.ProtocolTCP {
				return nil, fmt.Errorf("only TCP LoadBalancer is supported for edgecenter load balancers")
			}
		}
	}

	sourceRanges, err := v1service.GetLoadBalancerSourceRanges(svc)
	if err != nil {
		return nil, fmt.Errorf("failed to get source ranges for loadbalancer service %s/%s: %v", svc.Namespace, svc.Name, err)
	}
	if !l.opts.UseOctavia && !v1service.IsAllowAll(sourceRanges) && !opts.ManageSecurityGroups {
		return nil, fmt.Errorf("source range restrictions are not supported for edgecenter load balancers without managing security groups")
	}

	affinity := svc.Spec.SessionAffinity
	switch affinity {
	case corev1.ServiceAffinityClientIP:
		opts.SessionPersistence = &edgecloud.LoadbalancerSessionPersistence{Type: edgecloud.SessionPersistenceSourceIP}
	case corev1.ServiceAffinityNone:
		opts.SessionPersistence = nil
	default:
		return nil, fmt.Errorf("unsupported load balancer affinity: %v", affinity)
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
	opts.LBMethod = lbMethod

	keepClientIP, err := getBoolFromServiceAnnotation(svc, ServiceAnnotationLoadBalancerXForwardedFor, false)
	if err != nil {
		return nil, fmt.Errorf("failed to get X-Forwarded-For annotation: %w", err)
	}
	opts.KeepClientIP = keepClientIP

	useProxyProtocol, err := getBoolFromServiceAnnotation(svc, ServiceAnnotationLoadBalancerProxyEnabled, false)
	if err != nil {
		return nil, err
	}
	if useProxyProtocol && keepClientIP {
		return nil, fmt.Errorf("annotation %s and %s cannot be used together",
			ServiceAnnotationLoadBalancerProxyEnabled, ServiceAnnotationLoadBalancerXForwardedFor)
	}
	opts.UseProxyProtocol = useProxyProtocol

	timeoutOverrides, err := parseListenerTimeouts(svc)
	if err != nil {
		klog.Warningf("Invalid timeout annotation on service %s/%s: %v", svc.Namespace, svc.Name, err)
	}
	opts.Timeouts = timeoutOverrides

	secretID, err := l.getSecretID(ctx, svc)
	if err != nil {
		return nil, err
	}
	opts.SecretID = secretID

	return opts, nil
}

// createLoadBalancer creates a load balancer without listeners.
// Listener, pool and member synchronization is handled in separate phases afterwards.
func (l *LbaasV2) createLoadBalancer(ctx context.Context, name string, opts *ServiceOptions) (*edgecloud.Loadbalancer, error) {
	createOpts := &edgecloud.LoadbalancerCreateRequest{
		Name:   name,
		Flavor: lbFlavor,
	}

	if opts.NetworkID != "" {
		createOpts.VipNetworkID = opts.NetworkID
	}
	if opts.SubnetID != "" {
		createOpts.VipSubnetID = opts.SubnetID
	}

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

// waitLoadbalancerActiveProvisioningStatus waits until the load balancer reaches ACTIVE provisioning state.
// It returns the last observed provisioning status for better error reporting.
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

// getLoadbalancerByName returns an existing load balancer in a non-deleted state.
// It first tries the current generated name and then falls back to the legacy name
// for backward compatibility with previously created resources.
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

// prepareHealthMonitorOpts builds health monitor parameters for a service port.
// If the protocol cannot be mapped to a supported monitor type, TCP is used as a safe default.
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

// deleteLoadBalancer deletes the load balancer and verifies that it no longer exists.
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

// GetLoadBalancer returns whether the specified load balancer exists and, if it does,
// returns its current ingress IP based on the LB VIP address.
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

// GetLoadBalancerName returns the generated load balancer name used by the current implementation.
func (l *LbaasV2) GetLoadBalancerName(_ context.Context, _ string, service *corev1.Service) string {
	name := fmt.Sprintf("svc_%s_%s_%s", l.edgecenter.ClusterID, service.Namespace, service.Name)
	return strings.TrimSuffix(cutString(name), "-")
}

// GetLoadBalancerLegacyName returns the legacy Kubernetes cloud-provider LB name.
// It is kept for backward compatibility with older resources.
func (l *LbaasV2) GetLoadBalancerLegacyName(_ context.Context, _ string, service *corev1.Service) string {
	return cloudprovider.DefaultLoadBalancerName(service)
}

// ensureServiceFloatingIP ensures that the load balancer VIP port has a floating IP attached.
// If the service already has a published ingress IP, it attempts to reuse it.
func (l *LbaasV2) ensureServiceFloatingIP(ctx context.Context, svc *corev1.Service, lb *edgecloud.Loadbalancer, opts *ServiceOptions) (net.IP, error) {
	var existingAddr net.IP
	var lbIP string

	if len(svc.Status.LoadBalancer.Ingress) > 0 {
		lbIP = svc.Status.LoadBalancer.Ingress[0].IP
	}
	if lbIP != "" {
		existingAddr = net.ParseIP(lbIP)
	}

	address, err := l.EnsureFloatingIP(ctx, l.client, false, lb.VipPortID, existingAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to create floating IP: %v", err)
	}

	return address, nil
}

// UpdateLoadBalancer reuses the same phased flow as EnsureLoadBalancer
// to keep create and update behavior consistent.
func (l *LbaasV2) UpdateLoadBalancer(ctx context.Context, clusterName string, svc *corev1.Service, nodes []*corev1.Node) error {
	klog.V(4).Infof("UpdateLoadBalancer redirected to EnsureLoadBalancer for %s/%s", svc.Namespace, svc.Name)
	_, err := l.EnsureLoadBalancer(ctx, clusterName, svc, nodes)
	return err
}

// EnsureLoadBalancerDeleted deletes the load balancer and all dependent resources
// associated with the Service, including listeners, pools, floating IPs and security groups.
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

	err = l.deleteLoadBalancer(ctx, loadbalancer.ID)
	if err != nil {
		return fmt.Errorf("failed to delete loadbalancer %s: %w", loadbalancer.ID, err)
	}

	err = waitLoadbalancerDeleted(ctx, l.client, loadbalancer.ID)
	if err != nil {
		return fmt.Errorf("failed to delete loadbalancer %s: %w", loadbalancer.ID, err)
	}

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

// waitLoadbalancerDeleted waits until the load balancer is no longer returned by the API.
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

// getSubnetIDForLB returns the subnet ID of the node address that should be used
// as the backend member address for the load balancer.
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
