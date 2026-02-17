package edgecenter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	edgecloudUtil "github.com/Edge-Center/edgecentercloud-go/v2/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type ListenerTimeoutConfig struct {
	TimeoutClientData    *int
	TimeoutMemberData    *int
	TimeoutMemberConnect *int
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

func (l *LbaasV2) ensureLoadBalancerListeners(ctx context.Context, loadbalancer *edgecloud.Loadbalancer, ports []corev1.ServicePort, apiService *corev1.Service,
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

	tlsSecretID, err := l.getTLSSecretID(ctx, apiService)
	if err != nil {
		return err
	}
	if tlsSecretID != "" && keepClientIP {
		klog.Warningf("Both TLS secret and x-forwarded-for are set for service %s/%s; "+
			"TLS termination takes precedence for protocol selection", apiService.Namespace, apiService.Name)
	}

	for portIndex, port := range ports {
		var listener *edgecloud.Listener
		listenerProtocol := resolveListenerProtocol(port.Protocol, keepClientIP, tlsSecretID)
		listener = getListenerForPort(oldListeners, listenerProtocol, int(port.Port))

		if listener == nil {
			listenerName := cutString(fmt.Sprintf("%d_%s_listener", portIndex, loadbalancer.Name))
			listenerCreateOpt := &edgecloud.ListenerCreateRequest{
				Name:             strings.TrimSuffix(listenerName, "-"),
				Protocol:         listenerProtocol,
				ProtocolPort:     int(port.Port),
				LoadbalancerID:   loadbalancer.ID,
				InsertXForwarded: keepClientIP,
				SecretID:         tlsSecretID,
			}

			if timeoutOverrides != nil {
				listenerCreateOpt.TimeoutClientData = timeoutOverrides.TimeoutClientData
				listenerCreateOpt.TimeoutMemberData = timeoutOverrides.TimeoutMemberData
				listenerCreateOpt.TimeoutMemberConnect = timeoutOverrides.TimeoutMemberConnect
			}

			klog.V(4).Infof("Creating listener for port %d using protocol: %s", int(port.Port), listenerProtocol)
			listener, err = l.createListener(ctx, listenerCreateOpt)
			if err != nil {
				return fmt.Errorf("failed to create listener: %w", err)
			}

			listener, err = l.getListenerByID(ctx, listener.ID)
			if err != nil {
				return fmt.Errorf("failed to reload listener %s: %v", listener.ID, err)
			}

			klog.V(4).Infof("Listener %s created for loadbalancer %s", listener.ID, loadbalancer.ID)

		} else {
			if timeoutOverrides != nil {
				updateReq := &edgecloud.ListenerUpdateRequest{}
				needsUpdate := false

				if !intPtrEqual(listener.TimeoutClientData, timeoutOverrides.TimeoutClientData) {
					updateReq.TimeoutClientData = timeoutOverrides.TimeoutClientData
					needsUpdate = true
				}
				if !intPtrEqual(listener.TimeoutMemberData, timeoutOverrides.TimeoutMemberData) {
					updateReq.TimeoutMemberData = timeoutOverrides.TimeoutMemberData
					needsUpdate = true
				}
				if !intPtrEqual(listener.TimeoutMemberConnect, timeoutOverrides.TimeoutMemberConnect) {
					updateReq.TimeoutMemberConnect = timeoutOverrides.TimeoutMemberConnect
					needsUpdate = true
				}

				if needsUpdate {
					updateReq.Name = listener.Name

					klog.Infof("Updating listener %s timeouts: %+v", listener.ID, updateReq)
					_, _, err := l.client.Loadbalancers.ListenerUpdate(ctx, listener.ID, updateReq)
					if err != nil {
						return fmt.Errorf("failed updating listener %s: %v", listener.ID, err)
					}

					if _, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, loadbalancer.ID); err != nil {
						return fmt.Errorf("LB not ACTIVE after listener update: %v", err)
					}
				}
			}
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

func (l *LbaasV2) ensureListenerDeleted(ctx context.Context, loadbalancer *edgecloud.Loadbalancer, listener *edgecloud.Listener) error {
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

func resolveListenerProtocol(
	protocol corev1.Protocol,
	keepClientIP bool,
	tlsSecretID string,
) edgecloud.LoadbalancerListenerProtocol {
	switch {
	case tlsSecretID != "":
		return edgecloud.ListenerProtocolTerminatedHTTPS
	case keepClientIP:
		return edgecloud.ListenerProtocolHTTP
	default:
		return toListenersProtocol(protocol)
	}
}

func (l *LbaasV2) createLoadBalancerWithListeners(ctx context.Context, name string, lbClass *LBClass, ports []corev1.ServicePort, apiService *corev1.Service) (*edgecloud.Loadbalancer, error) {
	keepClientIP, err := getBoolFromServiceAnnotation(apiService, ServiceAnnotationLoadBalancerXForwardedFor, false)
	if err != nil {
		return nil, err
	}

	//meta := map[string]string{InternalClusterNameMeta: l.edgecenter.ClusterID}
	//if l.edgecenter.NoBillingTag {
	//	meta[NoBillingMeta] = "true"
	//}

	tlsSecretID, err := l.getTLSSecretID(ctx, apiService)
	if err != nil {
		return nil, err
	}
	if tlsSecretID != "" && keepClientIP {
		klog.Warningf("Both TLS secret and x-forwarded-for are set for service %s/%s; "+
			"TLS termination takes precedence for protocol selection", apiService.Namespace, apiService.Name)
	}

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
		listenerProtocol := resolveListenerProtocol(port.Protocol, keepClientIP, tlsSecretID)
		listenerName := cutString(fmt.Sprintf("%d_%s_listener", portIndex, name))
		listenerCreateOpt := edgecloud.LoadbalancerListenerCreateRequest{
			Name:             strings.TrimSuffix(listenerName, "-"),
			ProtocolPort:     int(port.Port),
			Protocol:         listenerProtocol,
			InsertXForwarded: keepClientIP,
			SecretID:         tlsSecretID,
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

func (l *LbaasV2) getListenerByID(ctx context.Context, id string) (*edgecloud.Listener, error) {
	listener, _, err := l.client.Loadbalancers.ListenerGet(ctx, id)

	if err != nil {
		return nil, err
	}

	return listener, nil
}

// getTLSSecretID gets tech secret ID from client secret ID
func (l *LbaasV2) getTLSSecretID(ctx context.Context, apiService *corev1.Service) (string, error) {
	clientSecretID := getStringFromServiceAnnotation(
		apiService,
		ServiceAnnotationLoadBalancerTLSSecretID,
		"",
	)
	if clientSecretID == "" {
		return "", nil
	}
	klog.V(4).Infof(
		"TLS secret ID %s found for service %s/%s",
		clientSecretID,
		apiService.Namespace,
		apiService.Name,
	)

	techSecretID, err := l.getOrCreateTechSecretByID(ctx, clientSecretID)
	if err != nil {
		return "", fmt.Errorf(
			"failed to get tech secret for service %s/%s: %w",
			apiService.Namespace,
			apiService.Name,
			err,
		)
	}

	klog.V(4).Infof("Tech secret ID %s obtained for client secret %s", techSecretID, clientSecretID)
	return techSecretID, nil
}

func (l *LbaasV2) getOrCreateTechSecretByID(ctx context.Context, secretID string) (string, error) {
	type CopySecretSchema struct {
		SecretId string `json:"secret_id"`
	}
	path := fmt.Sprintf(
		"/internal%s/%d/%d/%s/secrets",
		edgecloud.MKaaSClustersBasePathV2,
		l.edgecenter.ProjectID,
		l.edgecenter.RegionID,
		l.edgecenter.ClusterID,
	)

	req, err := l.client.NewRequest(ctx, http.MethodPost, path, &CopySecretSchema{SecretId: secretID})
	if err != nil {
		return "", err
	}

	secretResp := new(CopySecretSchema)
	if _, err = l.client.Do(ctx, req, secretResp); err != nil {
		return "", err
	}

	return secretResp.SecretId, nil
}
