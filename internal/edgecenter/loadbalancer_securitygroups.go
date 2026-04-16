package edgecenter

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// SecurityGroupRuleListOpts describes filters used when selecting security group rules
// from Edgecenter API responses.
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

// ensureSecurityGroup ensures that the service-specific security group exists
// and contains ingress rules allowing traffic from the LB subnet to nodePorts.
func (l *LbaasV2) ensureSecurityGroup(ctx context.Context, client *edgecloud.Client, clusterName string, apiService *corev1.Service, nodes []*corev1.Node, opts *ServiceOptions) error {
	ports := apiService.Spec.Ports
	if len(ports) == 0 {
		return fmt.Errorf("no ports provided to edgecenter load balancer")
	}

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
			return fmt.Errorf("failed to create Security Group for loadbalancer service %s/%s: opts: %+v %w",
				apiService.Namespace, apiService.Name, createOpts, err)
		}
		lbSecGroupID = lbSecGroup.ID
	}

	for _, port := range ports {
		subnet, _, err := client.Subnetworks.Get(ctx, opts.SubnetID)
		if err != nil {
			return fmt.Errorf("failed to find subnet %s from edgecenter API: %w", opts.SubnetID, err)
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
			return fmt.Errorf("failed to find security group rules in %s: %w", lbSecGroupID, err)
		}

		if len(sgRules) == 0 {
			cidr := subnet.CIDR
			rPort := int(port.NodePort)

			description := fmt.Sprintf("Security Group for %s/%s Service LoadBalancer in cluster %s",
				apiService.Namespace, apiService.Name, clusterName)

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
				return fmt.Errorf("failed to create rule for security group %s: %w", lbSecGroupID, err)
			}
		}
	}

	if err := applyNodeSecurityGroupIDForLB(ctx, l.client, nodes, lbSecGroupName); err != nil {
		return err
	}

	return nil
}

// updateSecurityGroup updates node security group rules after node set changes.
// It removes stale ingress rules and creates missing ones for current node security groups.
func (l *LbaasV2) updateSecurityGroup(ctx context.Context, apiService *corev1.Service, nodes []*corev1.Node, opts *ServiceOptions) error {
	currentNodeSecurityGroupIDs, err := getNodeSecurityGroupID(ctx, l.client, nodes)
	if err != nil {
		return fmt.Errorf("failed to find node-security-group for loadbalancer service %s/%s: %w",
			apiService.Namespace, apiService.Name, err)
	}

	klog.V(4).Infof("find node-security-group %v for loadbalancer service %s/%s",
		currentNodeSecurityGroupIDs, apiService.Namespace, apiService.Name)

	original := sets.NewString(opts.NodeSecurityGroupIDs...)
	current := sets.NewString(currentNodeSecurityGroupIDs...)
	removals := original.Difference(current)

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
		return fmt.Errorf("error occurred finding security group: %s", lbSecGroupName)
	}

	ports := apiService.Spec.Ports
	if len(ports) == 0 {
		return fmt.Errorf("no ports provided to edgecenter load balancer")
	}

	for _, port := range ports {
		for removal := range removals {
			ruleOpts := SecurityGroupRuleListOpts{
				Direction:       edgecloud.SGRuleDirectionIngress,
				SecurityGroupID: removal,
				RemoteGroupID:   lbSecGroupID,
				PortRangeMax:    int(port.NodePort),
				PortRangeMin:    int(port.NodePort),
				Protocol:        edgecloud.SecurityGroupRuleProtocol(port.Protocol),
			}

			secGroupRules, err := getSecurityGroupRules(ctx, l.client, ruleOpts)
			if err != nil {
				return fmt.Errorf("error finding rules for remote group id %s in security group id %s: %w", lbSecGroupID, removal, err)
			}

			for _, rule := range secGroupRules {
				_, err = l.client.SecurityGroups.Delete(ctx, rule.ID)
				if err != nil {
					return fmt.Errorf("error occurred deleting security group rule: %s: %w", rule.ID, err)
				}
			}
		}

		for _, nodeSecurityGroupID := range currentNodeSecurityGroupIDs {
			ruleOpts := SecurityGroupRuleListOpts{
				Direction:       edgecloud.SGRuleDirectionIngress,
				SecurityGroupID: nodeSecurityGroupID,
				RemoteGroupID:   lbSecGroupID,
				PortRangeMax:    int(port.NodePort),
				PortRangeMin:    int(port.NodePort),
				Protocol:        edgecloud.SecurityGroupRuleProtocol(port.Protocol),
			}

			secGroupRules, err := getSecurityGroupRules(ctx, l.client, ruleOpts)
			if err != nil {
				return fmt.Errorf("error finding rules for remote group id %s in security group id %s: %w",
					lbSecGroupID, nodeSecurityGroupID, err)
			}

			if len(secGroupRules) != 0 {
				continue
			}

			err = createNodeSecurityGroupRules(ctx, l.client, nodeSecurityGroupID, int(port.NodePort), port.Protocol, lbSecGroupID, apiService)
			if err != nil {
				return fmt.Errorf("error occurred creating security group for loadbalancer service %s/%s: %w",
					apiService.Namespace, apiService.Name, err)
			}
		}
	}

	if err := applyNodeSecurityGroupIDForLB(ctx, l.client, nodes, lbSecGroupName); err != nil {
		return err
	}

	return nil
}

// EnsureSecurityGroupDeleted removes the service-specific security group
// and cleans up related rules and associations on nodes.
func (l *LbaasV2) EnsureSecurityGroupDeleted(ctx context.Context, service *corev1.Service) error {
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
		if err := disassociateSecurityGroupForLB(ctx, l.client.Instances, lbSecGroupID, lbSecGroupName); err != nil {
			return fmt.Errorf("failed to disassociate security group %s: %v", lbSecGroupID, err)
		}
	}

	_, err = l.client.SecurityGroups.Delete(ctx, lbSecGroupID)
	if err != nil {
		return err
	}

	return nil
}

// compareSecurityGroup reports whether an existing rule matches the given filter options.
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

// getSecurityGroupRules returns security group rules matching the supplied filters.
// If SecurityGroupID is set, only that group is inspected; otherwise all groups are scanned.
func getSecurityGroupRules(ctx context.Context, client *edgecloud.Client, opts SecurityGroupRuleListOpts) ([]edgecloud.SecurityGroupRule, error) {
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

// getSecurityGroupName returns the deterministic name used for the Service LB security group.
func getSecurityGroupName(service *corev1.Service) string {
	securityGroupName := fmt.Sprintf("lb-sg-%s-%s-%s", service.UID, service.Namespace, service.Name)
	// Edgecenter requires that the name of a security group is shorter than 63 bytes.
	if len(securityGroupName) > maxNameLength {
		securityGroupName = securityGroupName[:maxNameLength]
	}

	return strings.TrimSuffix(securityGroupName, "-")
}

// createNodeSecurityGroupRules creates ingress rules in the node security group
// allowing traffic from the LB security group to the service nodePort.
func createNodeSecurityGroupRules(ctx context.Context, client *edgecloud.Client, nodeSecurityGroupID string, port int, protocol corev1.Protocol,
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

// applyNodeSecurityGroupIDForLB associates the LB security group with eligible node ports.
// It resolves the backing cloud instance primarily via node.spec.providerID and falls back
// to node name lookup only when providerID is not yet initialized.
func applyNodeSecurityGroupIDForLB(ctx context.Context, client *edgecloud.Client, nodes []*corev1.Node, securityGroup string) error {
	for _, node := range nodes {
		instance, err := getInstanceByNode(ctx, client, node)
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

		interfaces, _, err := client.Instances.InterfaceList(ctx, instance.ID)
		if err != nil {
			return fmt.Errorf("failed to list interfaces for instance %s: %w", instance.ID, err)
		}

		portsSGNames := make([]edgecloud.PortsSecurityGroupNames, 0, len(interfaces))
		for _, iface := range interfaces {
			allow := false
			for _, subnet := range iface.NetworkDetails.Subnets {
				if !strings.HasPrefix(subnet.CIDR, "100.") {
					allow = true
					break
				}
			}
			if !allow {
				continue
			}

			portsSGNames = append(portsSGNames, edgecloud.PortsSecurityGroupNames{
				SecurityGroupNames: []string{securityGroup},
				PortID:             iface.PortID,
			})
		}

		if len(portsSGNames) == 0 {
			klog.Warningf("no eligible ports found to assign SG %s for instance %s (node=%s)",
				securityGroup, instance.ID, node.Name)
			continue
		}

		opts := &edgecloud.AssignSecurityGroupRequest{
			Name:                    securityGroup,
			PortsSecurityGroupNames: portsSGNames,
		}

		_, err = client.Instances.SecurityGroupAssign(ctx, instance.ID, opts)
		if err != nil {
			return fmt.Errorf("failed to assign security group %s to instance %s: %w", securityGroup, instance.ID, err)
		}
	}

	return nil
}

// disassociateSecurityGroupForLB removes the given security group from instances where it is attached.
func disassociateSecurityGroupForLB(ctx context.Context, svc edgecloud.InstancesService, securityGroupID string, securityGroupName string) error {
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

// getNodeSecurityGroupID returns the set of security group IDs attached to the given nodes.
func getNodeSecurityGroupID(ctx context.Context, client *edgecloud.Client, nodes []*corev1.Node) ([]string, error) {
	secGroupIDs := sets.NewString()

	for _, node := range nodes {
		instanceID, err := getNodeInstanceID(ctx, client, node)
		if err != nil {
			return nil, err
		}

		list, _, err := client.Instances.SecurityGroupList(ctx, instanceID)
		if err != nil {
			return nil, err
		}

		for _, sg := range list {
			secGroupIDs.Insert(sg.ID)
		}
	}

	return secGroupIDs.List(), nil
}

// isSecurityGroupNotFound reports whether the error is an Edgecenter ResourceNotFoundError.
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

// toRuleProtocol converts a Kubernetes Service protocol to a security group rule protocol.
// If conversion fails, TCP is used as a safe default.
func toRuleProtocol(protocol corev1.Protocol) edgecloud.SecurityGroupRuleProtocol {
	tp := edgecloud.SecurityGroupRuleProtocol(strings.ToLower(string(protocol)))
	if err := tp.IsValid(); err != nil {
		return edgecloud.SGRuleProtocolTCP
	}
	return tp
}

// getInstanceByNode resolves the backing Edgecenter instance for the given Kubernetes node.
// It primarily uses node.spec.providerID and falls back to lookup by node name when providerID
// is not yet initialized.
func getInstanceByNode(ctx context.Context, client *edgecloud.Client, node *corev1.Node) (*edgecloud.Instance, error) {
	instanceID, err := getNodeInstanceID(ctx, client, node)
	if err != nil {
		return nil, err
	}

	instance, _, err := client.Instances.Get(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to get instance %s for node %s: %w", instanceID, node.Name, err)
	}

	return instance, nil
}
