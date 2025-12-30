package edgecenter

import (
	"context"
	"strings"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	corev1 "k8s.io/api/core/v1"
)

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

func toRuleProtocol(protocol corev1.Protocol) edgecloud.SecurityGroupRuleProtocol {
	tp := edgecloud.SecurityGroupRuleProtocol(strings.ToLower(string(protocol)))
	if err := tp.IsValid(); err != nil {
		return edgecloud.SGRuleProtocolTCP
	}
	return tp
}
