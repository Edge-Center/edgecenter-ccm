package edgecenter

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	edgecloudUtil "github.com/Edge-Center/edgecentercloud-go/v2/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// ListenerTimeoutConfig contains optional listener timeout overrides
// resolved from service annotations.
type ListenerTimeoutConfig struct {
	TimeoutClientData    *int
	TimeoutMemberData    *int
	TimeoutMemberConnect *int
}

// ensureLoadBalancerListeners ensures that a listener exists for each Service port.
// It returns the matched listeners keyed by stable service port key, along with
// listeners that are no longer needed and can be deleted later.
func (l *LbaasV2) ensureLoadBalancerListeners(ctx context.Context, loadbalancer *edgecloud.Loadbalancer, ports []corev1.ServicePort, _ *corev1.Service, opts *ServiceOptions) (map[string]*edgecloud.Listener, []edgecloud.Listener, error) {
	existing, err := getListenersByLoadBalancerID(ctx, l.client, loadbalancer.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("error getting LB %s listeners: %w", loadbalancer.Name, err)
	}

	result := make(map[string]*edgecloud.Listener, len(ports))
	used := make(map[string]struct{}, len(ports))

	for _, port := range ports {
		key := servicePortKey(port)
		listenerProtocol := resolveListenerProtocol(port.Protocol, opts.KeepClientIP, opts.SecretID)

		listener := findListenerForServicePort(existing, port, opts)
		if listener == nil {
			createReq := buildListenerCreateRequest(loadbalancer.ID, loadbalancer.Name, port, opts)

			klog.V(4).Infof("Creating listener for key=%s port=%d protocol=%s", key, int(port.Port), listenerProtocol)
			listener, err = l.createListener(ctx, createReq)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to create listener for key=%s: %w", key, err)
			}

			if _, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, loadbalancer.ID); err != nil {
				return nil, nil, fmt.Errorf("LB not ACTIVE after listener create for key=%s: %v", key, err)
			}

			listener, err = l.getListenerByID(ctx, listener.ID)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to reload listener %s: %v", listener.ID, err)
			}
		} else {
			if err := l.updateListenerIfNeeded(ctx, loadbalancer.ID, listener, port, opts); err != nil {
				return nil, nil, fmt.Errorf("failed updating listener for key=%s: %w", key, err)
			}

			listener, err = l.getListenerByID(ctx, listener.ID)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to reload listener %s after update: %v", listener.ID, err)
			}
		}

		result[key] = listener
		used[listener.ID] = struct{}{}
	}

	var obsolete []edgecloud.Listener
	for _, ln := range existing {
		if _, ok := used[ln.ID]; !ok {
			obsolete = append(obsolete, ln)
		}
	}

	return result, obsolete, nil
}

// parseListenerTimeouts parses listener timeout annotations from the Service.
// Missing annotations are treated as unset values.
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

// findListenerForServicePort returns an existing listener matching the Service port
// by resolved protocol and frontend port.
func findListenerForServicePort(existing []edgecloud.Listener, sp corev1.ServicePort, opts *ServiceOptions) *edgecloud.Listener {
	wantProtocol := resolveListenerProtocol(sp.Protocol, opts.KeepClientIP, opts.SecretID)

	for i := range existing {
		ln := &existing[i]
		if ln.Protocol == wantProtocol && ln.ProtocolPort == int(sp.Port) {
			return ln
		}
	}

	return nil
}

// buildListenerCreateRequest builds a listener create request for the given Service port.
func buildListenerCreateRequest(lbID string, lbName string, sp corev1.ServicePort, opts *ServiceOptions) *edgecloud.ListenerCreateRequest {
	req := &edgecloud.ListenerCreateRequest{
		Name:             buildListenerName(lbName, sp),
		Protocol:         resolveListenerProtocol(sp.Protocol, opts.KeepClientIP, opts.SecretID),
		ProtocolPort:     int(sp.Port),
		LoadbalancerID:   lbID,
		InsertXForwarded: opts.KeepClientIP,
		SecretID:         opts.SecretID,
	}

	if opts.Timeouts != nil {
		req.TimeoutClientData = opts.Timeouts.TimeoutClientData
		req.TimeoutMemberData = opts.Timeouts.TimeoutMemberData
		req.TimeoutMemberConnect = opts.Timeouts.TimeoutMemberConnect
	}

	return req
}

// updateListenerIfNeeded updates mutable listener settings when the actual listener
// does not match the desired configuration derived from the Service.
func (l *LbaasV2) updateListenerIfNeeded(ctx context.Context, loadbalancerID string, listener *edgecloud.Listener, sp corev1.ServicePort, opts *ServiceOptions) error {
	updateReq := &edgecloud.ListenerUpdateRequest{}
	needsUpdate := false

	if opts.Timeouts != nil {
		if !intPtrEqual(listener.TimeoutClientData, opts.Timeouts.TimeoutClientData) {
			updateReq.TimeoutClientData = opts.Timeouts.TimeoutClientData
			needsUpdate = true
		}
		if !intPtrEqual(listener.TimeoutMemberData, opts.Timeouts.TimeoutMemberData) {
			updateReq.TimeoutMemberData = opts.Timeouts.TimeoutMemberData
			needsUpdate = true
		}
		if !intPtrEqual(listener.TimeoutMemberConnect, opts.Timeouts.TimeoutMemberConnect) {
			updateReq.TimeoutMemberConnect = opts.Timeouts.TimeoutMemberConnect
			needsUpdate = true
		}
	}

	if listener.SecretID != opts.SecretID {
		updateReq.SecretID = opts.SecretID
		needsUpdate = true
	}

	if !needsUpdate {
		return nil
	}

	updateReq.Name = listener.Name

	klog.Infof("Updating listener %s: %+v", listener.ID, updateReq)
	_, _, err := l.client.Loadbalancers.ListenerUpdate(ctx, listener.ID, updateReq)
	if err != nil {
		return fmt.Errorf("failed updating listener %s: %v", listener.ID, err)
	}

	if _, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, loadbalancerID); err != nil {
		return fmt.Errorf("LB not ACTIVE after listener update: %v", err)
	}

	return nil
}

// ensureListenerDeleted deletes the listener and its bound pool, if present.
// This is used during cleanup when a listener is no longer part of the desired Service state.
func (l *LbaasV2) ensureListenerDeleted(ctx context.Context, loadbalancer *edgecloud.Loadbalancer, listener *edgecloud.Listener) error {
	pool, err := getPoolByListenerID(ctx, l.client, loadbalancer.ID, listener.ID, false)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("error getting pool for obsolete listener %s: %v", listener.ID, err)
		}
		pool = nil
	}

	if pool != nil {
		klog.V(4).Infof("Deleting obsolete pool %s for listener %s", pool.ID, listener.ID)

		err = l.deletePool(ctx, pool.ID)
		if err != nil {
			return fmt.Errorf("error deleting obsolete pool %s for listener %s: %v", pool.ID, listener.ID, err)
		}

		provisioningStatus, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, loadbalancer.ID)
		if err != nil {
			return fmt.Errorf("timeout when waiting for loadbalancer to be ACTIVE after deleting pool, current provisioning status %s", provisioningStatus)
		}
	}

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

// deleteObsoleteListenersAndPools deletes resources that were not matched to current Service ports.
// Pools are removed before listeners to preserve dependency order.
func deleteObsoleteListenersAndPools(ctx context.Context, l *LbaasV2, loadbalancer *edgecloud.Loadbalancer, obsoleteListeners []edgecloud.Listener, obsoletePools []edgecloud.Pool) error {
	for _, pool := range obsoletePools {
		klog.V(4).Infof("Deleting obsolete pool %s", pool.ID)
		if err := l.deletePool(ctx, pool.ID); err != nil {
			return fmt.Errorf("failed to delete obsolete pool %s: %w", pool.ID, err)
		}
		if _, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, loadbalancer.ID); err != nil {
			return fmt.Errorf("LB not ACTIVE after obsolete pool delete: %v", err)
		}
	}

	for _, listener := range obsoleteListeners {
		klog.V(4).Infof("Deleting obsolete listener %s", listener.ID)
		if err := l.deleteListener(ctx, listener.ID); err != nil {
			return fmt.Errorf("failed to delete obsolete listener %s: %w", listener.ID, err)
		}
		if _, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, loadbalancer.ID); err != nil {
			return fmt.Errorf("LB not ACTIVE after obsolete listener delete: %v", err)
		}
	}

	return nil
}

// deleteObsoleteListenersAndPools is a method wrapper around the package-level cleanup helper.
func (l *LbaasV2) deleteObsoleteListenersAndPools(ctx context.Context, loadbalancer *edgecloud.Loadbalancer, obsoleteListeners []edgecloud.Listener, obsoletePools []edgecloud.Pool) error {
	return deleteObsoleteListenersAndPools(ctx, l, loadbalancer, obsoleteListeners, obsoletePools)
}

// popListener removes the listener with the given ID from the slice in-place.
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

// getListenersByLoadBalancerID lists all listeners attached to the given load balancer.
func getListenersByLoadBalancerID(ctx context.Context, client *edgecloud.Client, loadbalancerID string) ([]edgecloud.Listener, error) {
	opts := &edgecloud.ListenerListOptions{LoadbalancerID: loadbalancerID}
	list, _, err := client.Loadbalancers.ListenerList(ctx, opts)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// resolveListenerProtocol determines the listener protocol to use for the Service port.
// TLS termination takes precedence over HTTP X-Forwarded-For mode, and both override raw transport protocol mapping.
func resolveListenerProtocol(protocol corev1.Protocol, keepClientIP bool, secretID string) edgecloud.LoadbalancerListenerProtocol {
	if secretID != "" {
		return edgecloud.ListenerProtocolTerminatedHTTPS
	}
	if keepClientIP {
		return edgecloud.ListenerProtocolHTTP
	}

	switch protocol {
	case corev1.ProtocolTCP:
		return edgecloud.ListenerProtocolTCP
	case corev1.ProtocolUDP:
		return edgecloud.ListenerProtocolUDP
	default:
		return defaultLBProtocol
	}
}

// createListener creates a new listener and returns its fully loaded representation.
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

// deleteListener deletes the listener and verifies that it no longer exists.
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

// getListenerByID returns the current listener representation by its ID.
func (l *LbaasV2) getListenerByID(ctx context.Context, id string) (*edgecloud.Listener, error) {
	listener, _, err := l.client.Loadbalancers.ListenerGet(ctx, id)
	if err != nil {
		return nil, err
	}
	return listener, nil
}
