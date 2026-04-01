package edgecenter

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	edgecloudUtil "github.com/Edge-Center/edgecentercloud-go/v2/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

// ensureLoadBalancerPools ensures that each desired listener has a corresponding pool.
// It returns pools keyed by stable service port key and a list of obsolete pools
// that are no longer referenced by the current Service state.
func (l *LbaasV2) ensureLoadBalancerPools(ctx context.Context, lb *edgecloud.Loadbalancer, svc *corev1.Service, listeners map[string]*edgecloud.Listener, opts *ServiceOptions) (map[string]*edgecloud.Pool, []edgecloud.Pool, error) {
	allPools, err := l.listPoolsByLoadBalancerID(ctx, lb.ID, true)
	if err != nil {
		return nil, nil, fmt.Errorf("failed listing pools for LB %s: %w", lb.ID, err)
	}

	poolsByListenerID := make(map[string]*edgecloud.Pool, len(allPools))
	for i := range allPools {
		p := &allPools[i]
		for _, ref := range p.Listeners {
			if ref.ID == "" {
				continue
			}
			poolsByListenerID[ref.ID] = p
		}
	}

	result := make(map[string]*edgecloud.Pool, len(listeners))
	usedPoolIDs := make(map[string]struct{}, len(listeners))

	for i := range svc.Spec.Ports {
		sp := svc.Spec.Ports[i]
		key := servicePortKey(sp)

		listener := listeners[key]
		if listener == nil {
			return nil, nil, fmt.Errorf("listener for port key %s not found during pools phase", key)
		}

		pool := poolsByListenerID[listener.ID]
		if pool == nil {
			createReq := l.buildPoolCreateRequest(listener, lb.Name, sp, svc, opts)

			klog.V(4).Infof("Creating pool for key=%s listener=%s", key, listener.ID)
			pool, err = l.createPool(ctx, createReq)
			if err != nil {
				return nil, nil, fmt.Errorf("error creating pool for listener %s: %v", listener.ID, err)
			}

			if _, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, lb.ID); err != nil {
				return nil, nil, fmt.Errorf("LB not ACTIVE after creating pool for listener %s: %v", listener.ID, err)
			}
		}

		result[key] = pool
		usedPoolIDs[pool.ID] = struct{}{}
	}

	var obsolete []edgecloud.Pool
	for _, pool := range allPools {
		if _, ok := usedPoolIDs[pool.ID]; !ok {
			obsolete = append(obsolete, pool)
		}
	}

	return result, obsolete, nil
}

// buildPoolCreateRequest builds a pool create request for the given listener and Service port.
// Pool protocol is derived from the listener protocol, except for HTTP/TLS-terminated modes
// where HTTP is used for backend communication.
func (l *LbaasV2) buildPoolCreateRequest(listener *edgecloud.Listener, lbName string, sp corev1.ServicePort, _ *corev1.Service, opts *ServiceOptions) *edgecloud.PoolCreateRequest {
	poolProto := edgecloud.LoadbalancerPoolProtocol(listener.Protocol)
	if opts.KeepClientIP || opts.SecretID != "" {
		poolProto = edgecloud.LoadbalancerPoolProtocol(edgecloud.ListenerProtocolHTTP)
	}

	var healthMonitorOpts *edgecloud.HealthMonitorCreateRequest
	if opts.CreateMonitor {
		healthMonitorOpts = l.prepareHealthMonitorOpts(sp)
	}

	return &edgecloud.PoolCreateRequest{
		LoadbalancerPoolCreateRequest: edgecloud.LoadbalancerPoolCreateRequest{
			Name:                  buildPoolName(lbName, sp),
			Protocol:              poolProto,
			LoadbalancerAlgorithm: opts.LBMethod,
			ListenerID:            listener.ID,
			HealthMonitor:         healthMonitorOpts,
			SessionPersistence:    opts.SessionPersistence,
		},
	}
}

// reconcilePoolMembers synchronizes backend members for each pool with the current node set.
// Member addresses are derived from node addresses and Service nodePorts.
func (l *LbaasV2) reconcilePoolMembers(ctx context.Context, lb *edgecloud.Loadbalancer, svc *corev1.Service, pools map[string]*edgecloud.Pool, opts *ServiceOptions, nodes []*corev1.Node) error {
	desiredNodeIPs := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ip, err := nodeAddressForLB(n)
		if err != nil {
			return fmt.Errorf("nodeAddressForLB: %v", err)
		}
		desiredNodeIPs = append(desiredNodeIPs, ip)
	}

	for i := range svc.Spec.Ports {
		sp := &svc.Spec.Ports[i]
		key := servicePortKey(*sp)

		pool := pools[key]
		if pool == nil {
			return fmt.Errorf("pool for port key %s not found", key)
		}

		if sp.NodePort == 0 {
			return fmt.Errorf("service port %d has nodePort=0", sp.Port)
		}

		var desiredMembers []edgecloud.PoolMemberCreateRequest
		for _, ip := range desiredNodeIPs {
			desiredMembers = append(desiredMembers, edgecloud.PoolMemberCreateRequest{
				Address:      net.ParseIP(ip),
				ProtocolPort: int(sp.NodePort),
				SubnetID:     opts.SubnetID,
			})
		}

		if !membersChanged(pool.Members, desiredMembers) {
			klog.V(4).Infof("Pool %s skipped: no member changes", pool.ID)
			continue
		}

		klog.V(2).Infof("[LB][%s/%s] Pool %s member reconcile START (desired=%d)",
			svc.Namespace, svc.Name, pool.ID, len(desiredMembers))

		updateReq := &edgecloud.PoolUpdateRequest{
			ID:                    pool.ID,
			Name:                  pool.Name,
			Members:               desiredMembers,
			SessionPersistence:    pool.SessionPersistence,
			LoadbalancerAlgorithm: pool.LoadbalancerAlgorithm,
		}

		if err := l.updatePoolMembersWithRetry(ctx, lb.ID, pool.ID, updateReq); err != nil {
			return fmt.Errorf("PoolUpdate failed for pool %s: %v", pool.ID, err)
		}

		refreshed, err := l.waitPoolMembersApplied(ctx, lb.ID, pool.ID, desiredMembers)
		if err != nil {
			return fmt.Errorf("pool %s members were not observed after update: %w", pool.ID, err)
		}

		pools[key] = refreshed

		klog.V(2).Infof("[LB][%s/%s] Pool %s member reconcile DONE",
			svc.Namespace, svc.Name, pool.ID)
	}

	return nil
}

// listPoolsByLoadBalancerID returns all pools attached to the specified load balancer.
func (l *LbaasV2) listPoolsByLoadBalancerID(ctx context.Context, loadbalancerID string, details bool) ([]edgecloud.Pool, error) {
	opts := &edgecloud.PoolListOptions{
		LoadbalancerID: loadbalancerID,
		Details:        details,
	}
	list, _, err := l.client.Loadbalancers.PoolList(ctx, opts)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// memberExists reports whether a pool already contains a member with the given address and port.
func memberExists(members []edgecloud.PoolMember, addr string, port int) bool {
	for _, member := range members {
		if member.Address.String() == addr && member.ProtocolPort == port {
			return true
		}
	}

	return false
}

// membersChanged reports whether the current pool member set differs from the desired one.
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

// popMember removes the specified member from the slice in-place by address and port.
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

// createPool creates a new pool and returns its fully loaded representation.
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

// deletePool deletes the pool and verifies that it no longer exists.
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

// createPoolMember creates a single backend member in the specified pool.
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

// deletePoolMember deletes a single backend member from the specified pool
// and verifies that it is no longer present afterwards.
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

// getPoolByListenerID returns the pool bound to the specified listener.
// A listener is expected to have at most one pool.
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

// nodeAddressForLB returns the node address that should be used as a load balancer backend member.
// It prefers InternalIP and falls back to ExternalIP if needed.
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

func isLoadBalancerImmutableError(err error) bool {
	if err == nil {
		return false
	}

	msg := err.Error()
	return strings.Contains(msg, "immutable state") ||
		strings.Contains(msg, "is not allowed to process")
}

func (l *LbaasV2) updatePoolMembersWithRetry(ctx context.Context, lbID string, poolID string, updateReq *edgecloud.PoolUpdateRequest) error {
	backoff := wait.Backoff{
		Duration: 2 * time.Second,
		Factor:   1.5,
		Steps:    6,
	}

	var lastErr error

	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		if _, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, lbID); err != nil {
			lastErr = fmt.Errorf("LB not ACTIVE before PoolUpdate: %w", err)
			return false, nil
		}

		_, _, err := l.client.Loadbalancers.PoolUpdate(ctx, poolID, updateReq)
		if err != nil {
			if isLoadBalancerImmutableError(err) {
				lastErr = err
				klog.Warningf("PoolUpdate for pool %s hit immutable LB state, retrying", poolID)
				return false, nil
			}
			return false, err
		}

		if _, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, lbID); err != nil {
			lastErr = fmt.Errorf("LB not ACTIVE after PoolUpdate: %w", err)
			return false, nil
		}

		return true, nil
	})

	if err != nil {
		if lastErr != nil {
			return lastErr
		}
		return err
	}

	return nil
}

func (l *LbaasV2) waitPoolMembersApplied(ctx context.Context, lbID string, poolID string, desired []edgecloud.PoolMemberCreateRequest) (*edgecloud.Pool, error) {
	backoff := wait.Backoff{
		Duration: 2 * time.Second,
		Factor:   1.5,
		Steps:    6,
	}

	var lastPool *edgecloud.Pool
	var lastErr error

	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		if _, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, lbID); err != nil {
			lastErr = fmt.Errorf("LB not ACTIVE while waiting for pool %s members to apply: %w", poolID, err)
			return false, nil
		}

		pool, _, err := l.client.Loadbalancers.PoolGet(ctx, poolID)
		if err != nil {
			lastErr = fmt.Errorf("PoolGet failed for pool %s: %w", poolID, err)
			return false, nil
		}
		if pool == nil {
			lastErr = fmt.Errorf("pool %s not found after update", poolID)
			return false, nil
		}

		lastPool = pool

		if membersChanged(pool.Members, desired) {
			klog.V(4).Infof("Pool %s members not yet applied, retrying", poolID)
			return false, nil
		}

		return true, nil
	})

	if err != nil {
		if lastErr != nil {
			return lastPool, lastErr
		}
		return lastPool, err
	}

	if lastPool == nil {
		return nil, fmt.Errorf("pool %s returned nil after update", poolID)
	}

	return lastPool, nil
}
