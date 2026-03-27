package edgecenter

import (
	"context"
	"errors"
	"fmt"
	"net"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	edgecloudUtil "github.com/Edge-Center/edgecentercloud-go/v2/util"
	"k8s.io/klog/v2"
)

// EnsureFloatingIP ensures that the load balancer VIP port has the expected floating IP state.
// It can either delete an existing floating IP mapping or create/assign one to the given port.
func (l *LbaasV2) EnsureFloatingIP(ctx context.Context, client *edgecloud.Client, needDelete bool, portID string, existingFIP net.IP) (net.IP, error) {
	klog.Info("Ensuring floating IP start")

	if needDelete {
		if existingFIP == nil {
			return nil, nil
		}

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

	if len(fips) == 1 {
		klog.Infof("Floating IP already exists for port %s: %s", portID, fips[0].FloatingIPAddress)
		return net.ParseIP(fips[0].FloatingIPAddress), nil
	}

	if existingFIP != nil {
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

		assignOpts := &edgecloud.AssignFloatingIPRequest{
			PortID: portID,
		}

		klog.Infof("Assigning existing floating IP %s to port %s", existingFIP.String(), portID)

		fip, _, err := client.Floatingips.Assign(ctx, fipID, assignOpts)
		if err != nil {
			return nil, err
		}

		return net.ParseIP(fip.FloatingIPAddress), nil
	}

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
		return nil, fmt.Errorf("cannot get floating IP with ID: %s. Error: %w", task.FloatingIPs[0], err)
	}

	return net.ParseIP(fip.FloatingIPAddress), nil
}

// getFloatingIPsByAddr returns all floating IPs matching the provided address.
func (l *LbaasV2) getFloatingIPsByAddr(ctx context.Context, client *edgecloud.Client, addr string) ([]edgecloud.FloatingIP, error) {
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

// getFloatingNetworkIDForLB returns an external network ID suitable for floating IP allocation.
// If multiple external networks exist, the first one is used.
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
