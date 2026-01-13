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

func (l *LbaasV2) EnsureFloatingIP(ctx context.Context, client *edgecloud.Client, needDelete bool, portID string, existingFIP net.IP) (net.IP, error) {

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
