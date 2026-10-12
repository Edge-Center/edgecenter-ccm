package edgecenter

import (
	"context"
	"fmt"
	"net"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
)

type staticIPSelection struct {
	ReservedVIPPortID   string
	ReservedVIPSubnetID string
}

func (l *LbaasV2) resolveStaticIP(ctx context.Context, address net.IP, currentLB *edgecloud.Loadbalancer) (*staticIPSelection, error) {
	if address == nil {
		return nil, nil
	}

	reservedIPs, _, err := l.client.ReservedFixedIP.List(ctx, &edgecloud.ReservedFixedIPListOptions{
		ExternalOnly:   true,
		SearchPrefixIP: address.String(),
		Limit:          100,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to find reserved IP %s in Edgecenter region %d: %w", address, l.edgecenter.RegionID, err)
	}
	selection, found, err := selectReservedVIPPort(reservedIPs, address, currentLB)
	if err != nil {
		return nil, fmt.Errorf("reserved IP %s in Edgecenter region %d: %w", address, l.edgecenter.RegionID, err)
	}
	if found {
		return selection, nil
	}

	floatingIPs, _, err := l.client.Floatingips.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to find floating IP %s in Edgecenter region %d: %w", address, l.edgecenter.RegionID, err)
	}
	selection, found, err = selectFloatingIP(floatingIPs, address, currentLB)
	if err != nil {
		return nil, fmt.Errorf("floating IP %s in Edgecenter region %d: %w", address, l.edgecenter.RegionID, err)
	}
	if !found {
		return nil, fmt.Errorf("IP %s was not found in Edgecenter region %d", address, l.edgecenter.RegionID)
	}
	return selection, nil
}

func selectReservedVIPPort(reservedIPs []edgecloud.ReservedFixedIP, address net.IP, currentLB *edgecloud.Loadbalancer) (*staticIPSelection, bool, error) {
	var match *edgecloud.ReservedFixedIP
	for index := range reservedIPs {
		if reservedIPs[index].FixedIPAddress == nil || !reservedIPs[index].FixedIPAddress.Equal(address) {
			continue
		}
		if match != nil {
			return nil, false, fmt.Errorf("multiple reserved IP resources match the address")
		}
		match = &reservedIPs[index]
	}
	if match == nil {
		return nil, false, nil
	}
	if !match.IsExternal || match.PortID == "" {
		return nil, false, fmt.Errorf("reserved IP has no external VIP port")
	}
	if currentLB == nil {
		if match.Reservation.ResourceID != "" {
			return nil, false, fmt.Errorf("reserved IP is attached to load balancer %s", match.Reservation.ResourceID)
		}
	} else {
		if match.PortID != currentLB.VipPortID || currentLB.VipAddress == nil || !currentLB.VipAddress.Equal(address) {
			return nil, false, fmt.Errorf("reserved IP does not match the Service load balancer")
		}
		if match.Reservation.ResourceID != "" && match.Reservation.ResourceID != currentLB.ID {
			return nil, false, fmt.Errorf("reserved IP is attached to load balancer %s", match.Reservation.ResourceID)
		}
	}

	return &staticIPSelection{ReservedVIPPortID: match.PortID, ReservedVIPSubnetID: match.SubnetID}, true, nil
}

func selectFloatingIP(floatingIPs []edgecloud.FloatingIP, address net.IP, currentLB *edgecloud.Loadbalancer) (*staticIPSelection, bool, error) {
	var match *edgecloud.FloatingIP
	for index := range floatingIPs {
		if floatingIPs[index].FloatingIPAddress != address.String() {
			continue
		}
		if match != nil {
			return nil, false, fmt.Errorf("multiple floating IP resources match the address")
		}
		match = &floatingIPs[index]
	}
	if match == nil {
		return nil, false, nil
	}
	if currentLB == nil {
		if match.PortID != "" {
			return nil, false, fmt.Errorf("floating IP is already assigned to port %s", match.PortID)
		}
	} else if match.PortID != "" && match.PortID != currentLB.VipPortID {
		return nil, false, fmt.Errorf("floating IP is already assigned to port %s", match.PortID)
	}

	return &staticIPSelection{}, true, nil
}
