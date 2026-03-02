package edgecenter

import (
	"context"
	"errors"
	"fmt"
	"net"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	edgecloudUtil "github.com/Edge-Center/edgecentercloud-go/v2/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

func (l *LbaasV2) ensureLoadBalancerPool(ctx context.Context, lb *edgecloud.Loadbalancer, listener *edgecloud.Listener, poolName string, port corev1.ServicePort,
	apiService *corev1.Service,
	persistence *edgecloud.LoadbalancerSessionPersistence,
	lbMethod edgecloud.LoadbalancerAlgorithm,
	nodes []*corev1.Node,
	keepClientIP bool,
	secretID string,
) (*edgecloud.Pool, error) {

	pool, err := getPoolByListenerID(ctx, l.client, lb.ID, listener.ID, true)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("error getting pool for listener %s: %v", listener.ID, err)
	}

	if pool != nil {
		if err := l.reconcilePools(ctx, lb, apiService, nodes); err != nil {
			return pool, fmt.Errorf("pool member sync failed: %w", err)
		}
		klog.V(4).Infof("Pool %s already exists for listener %s", pool.ID, listener.ID)
		return pool, nil
	}

	klog.V(4).Infof("Pool missing for listener %s — creating new pool %s", listener.ID, poolName)

	poolProto := edgecloud.LoadbalancerPoolProtocol(listener.Protocol)

	if keepClientIP || secretID != "" {
		poolProto = edgecloud.LoadbalancerPoolProtocol(edgecloud.ListenerProtocolHTTP)
	}

	useProxyProtocol, err := getBoolFromServiceAnnotation(apiService, ServiceAnnotationLoadBalancerProxyEnabled, false)
	if err != nil {
		return nil, err
	}
	if useProxyProtocol && keepClientIP {
		return nil, fmt.Errorf("annotation %s and %s cannot be used together",
			ServiceAnnotationLoadBalancerProxyEnabled, ServiceAnnotationLoadBalancerXForwardedFor)
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

	klog.V(4).Infof("Creating pool for listener %s (protocol=%s, lb_method=%s)",
		listener.ID, poolProto, lbMethod)

	pool, err = l.createPool(ctx, createOpt)
	if err != nil {
		return nil, fmt.Errorf("error creating pool for listener %s: %v", listener.ID, err)
	}

	if _, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, lb.ID); err != nil {
		return nil, fmt.Errorf(
			"timeout when waiting for LB ACTIVE after creating pool for listener %s: %v",
			listener.ID, err)
	}

	klog.V(4).Infof("Pool %s created for listener %s", pool.ID, listener.ID)

	if err := l.reconcilePools(ctx, lb, apiService, nodes); err != nil {
		return pool, fmt.Errorf("pool member sync failed after create: %w", err)
	}

	return pool, nil
}

func (l *LbaasV2) reconcilePools(ctx context.Context, lb *edgecloud.Loadbalancer, svc *corev1.Service, nodes []*corev1.Node) error {
	desired := make(map[string]*corev1.Node)
	for _, n := range nodes {
		ip, err := nodeAddressForLB(n)
		if err != nil {
			return fmt.Errorf("nodeAddressForLB: %v", err)
		}
		desired[ip] = n
	}

	listeners, err := getListenersByLoadBalancerID(ctx, l.client, lb.ID)
	if err != nil {
		return fmt.Errorf("failed listing listeners: %v", err)
	}

	for i := range listeners {
		ln := &listeners[i]

		nodePort, err := nodePortForListener(svc, ln)
		if err != nil {
			return err
		}

		klog.V(4).Infof("reconcilePools: listener=%s listener_port=%d nodePort=%d", ln.ID, ln.ProtocolPort, nodePort)

		pool, err := getPoolByListenerID(ctx, l.client, lb.ID, ln.ID, true)
		if err != nil {
			return fmt.Errorf("failed getting pool: %v", err)
		}

		var desiredMembers []edgecloud.PoolMemberCreateRequest
		for ip := range desired {
			desiredMembers = append(desiredMembers, edgecloud.PoolMemberCreateRequest{
				Address:      net.ParseIP(ip),
				ProtocolPort: nodePort,
				SubnetID:     l.opts.SubnetID,
			})
		}

		if !membersChanged(pool.Members, desiredMembers) {
			klog.V(4).Infof("Pool %s skipped: no member changes", pool.ID)
			continue
		}

		klog.V(4).Infof("Pool %s desired members count=%d (port=%d)", pool.ID, len(desiredMembers), nodePort)

		updateReq := &edgecloud.PoolUpdateRequest{
			ID:                    pool.ID,
			Name:                  pool.Name,
			Members:               desiredMembers,
			SessionPersistence:    pool.SessionPersistence,
			LoadbalancerAlgorithm: pool.LoadbalancerAlgorithm,
		}

		klog.V(4).Infof("PoolUpdate: %s with %d members (port=%d)", pool.ID, len(updateReq.Members), nodePort)

		_, _, err = l.client.Loadbalancers.PoolUpdate(ctx, pool.ID, updateReq)
		if err != nil {
			return fmt.Errorf("PoolUpdate failed for pool %s: %v", pool.ID, err)
		}

		if _, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, lb.ID); err != nil {
			return fmt.Errorf("LB not ACTIVE after PoolUpdate: %v", err)
		}

		klog.V(4).Infof("Pool %s successfully updated", pool.ID)
	}

	return nil
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

func membersChanged(current []edgecloud.PoolMember, desired []edgecloud.PoolMemberCreateRequest) bool {
	if len(current) != len(desired) {
		return true
	}

	curSet := make(map[string]struct{}, len(current))
	for _, m := range current {
		curSet[poolMemberKey(m)] = struct{}{}
	}

	for _, d := range desired {
		if _, ok := curSet[poolMemberCreateKey(d)]; !ok {
			return true
		}
	}

	return false
}

func popMember(members []edgecloud.PoolMember, addr string, port int) []edgecloud.PoolMember {
	j := len(members) - 1

	for i := 0; i <= j; {
		m := members[i]

		if m.Address.String() == addr && m.ProtocolPort == port {
			members[i] = members[j]
			j--
		} else {
			i++
		}
	}

	return members[:j+1]
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

// Get pool for a listener. A listener always has exactly one pool.
func getPoolByListenerID(ctx context.Context, client *edgecloud.Client, loadbalancerID, listenerID string, details bool) (*edgecloud.Pool, error) {
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
