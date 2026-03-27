package edgecenter

import (
	"fmt"
	"time"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// Note: when creating a new Loadbalancer (VM), it can take some time before it is ready for use,
// this timeout is used for waiting until the Loadbalancer provisioning status goes to ACTIVE state.
const (
	// loadbalancerActive* configures exponential backoff while waiting
	// for the load balancer provisioning status to become ACTIVE.
	loadbalancerActiveInitDelay = 1 * time.Second
	loadbalancerActiveFactor    = 1.2
	loadbalancerActiveSteps     = 19

	// loadbalancerDelete* configures exponential backoff while waiting
	// for load balancer deletion to complete.
	loadbalancerDeleteInitDelay = 1 * time.Second
	loadbalancerDeleteFactor    = 1.2
	loadbalancerDeleteSteps     = 13

	// activeStatus and errorStatus are legacy string constants used in status checks.
	activeStatus = "ACTIVE" // nolint
	errorStatus  = "ERROR"  // nolint

	// defaultLBProtocol is used when a service protocol cannot be mapped explicitly.
	defaultLBProtocol = edgecloud.ListenerProtocolTCP

	// maxNameLength is the maximum resource name length accepted by Edgecenter.
	maxNameLength = 63

	// Service annotations supported by the Edgecenter CCM load balancer implementation.
	ServiceAnnotationLoadBalancerConnLimit              = "loadbalancer.edgecenter.com/connection-limit"                 // nolint
	ServiceAnnotationLoadBalancerFloatingNetworkID      = "loadbalancer.edgecenter.com/floating-network-id"              // nolint
	ServiceAnnotationLoadBalancerFloatingSubnet         = "loadbalancer.edgecenter.com/floating-subnet"                  // nolint
	ServiceAnnotationLoadBalancerFloatingSubnetID       = "loadbalancer.edgecenter.com/floating-subnet-id"               // nolint
	ServiceAnnotationLoadBalancerInternal               = "service.beta.kubernetes.io/edgecenter-internal-load-balancer" // nolint
	ServiceAnnotationSaveFloating                       = "loadbalancer.edgecenter.com/save-floating"                    // nolint
	ServiceAnnotationLoadBalancerClass                  = "loadbalancer.edgecenter.com/class"                            // nolint
	ServiceAnnotationLoadBalancerKeepFloatingIP         = "loadbalancer.edgecenter.com/keep-floatingip"                  // nolint
	ServiceAnnotationLoadBalancerPortID                 = "loadbalancer.edgecenter.com/port-id"                          // nolint
	ServiceAnnotationLoadBalancerProxyEnabled           = "loadbalancer.edgecenter.com/proxy-protocol"                   // nolint
	ServiceAnnotationLoadBalancerSubnetID               = "loadbalancer.edgecenter.com/subnet-id"                        // nolint
	ServiceAnnotationLoadBalancerNetworkID              = "loadbalancer.edgecenter.com/network-id"                       // nolint
	ServiceAnnotationLoadBalancerTimeoutClientData      = "loadbalancer.edgecenter.com/timeout-client-data"              // nolint
	ServiceAnnotationLoadBalancerTimeoutMemberConnect   = "loadbalancer.edgecenter.com/timeout-member-connect"           // nolint
	ServiceAnnotationLoadBalancerTimeoutMemberData      = "loadbalancer.edgecenter.com/timeout-member-data"              // nolint
	ServiceAnnotationLoadBalancerTimeoutTCPInspect      = "loadbalancer.edgecenter.com/timeout-tcp-inspect"              // nolint
	ServiceAnnotationLoadBalancerXForwardedFor          = "loadbalancer.edgecenter.com/x-forwarded-for"                  // nolint
	ServiceAnnotationLoadBalancerDefaultTLSContainerRef = "loadbalancer.edgecenter.com/default-tls-container-ref"        // nolint

	// k8sIngressTag is a legacy/internal tag value used for Kubernetes ingress-related resources.
	k8sIngressTag = "k8s_ingress"
)

// getStringFromServiceAnnotation returns the Service annotation value for the given key,
// or the provided default value when the annotation is not set.
func getStringFromServiceAnnotation(service *corev1.Service, annotationKey string, defaultSetting string) string {
	if annotationValue, ok := service.Annotations[annotationKey]; ok {
		// annotationValue can intentionally be empty, for example to allow provisioning
		// a load balancer without a floating IP.
		klog.V(4).Infof("Found a Service Annotation: %v = %v", annotationKey, annotationValue)
		return annotationValue
	}

	klog.V(4).Infof("Could not find a Service Annotation; falling back on cloud-config setting: %v = %v", annotationKey, defaultSetting)
	return defaultSetting
}

// getBoolFromServiceAnnotation returns the boolean value of the given Service annotation,
// or the provided default value when the annotation is not set.
func getBoolFromServiceAnnotation(service *corev1.Service, annotationKey string, defaultSetting bool) (bool, error) {
	klog.V(4).Infof("getBoolFromServiceAnnotation(%v, %v, %v)", service, annotationKey, defaultSetting)
	if annotationValue, ok := service.Annotations[annotationKey]; ok {
		returnValue := false
		switch annotationValue {
		case "true":
			returnValue = true
		case "false":
			returnValue = false
		default:
			return returnValue, fmt.Errorf("unknown %s annotation: %v, specify \"true\" or \"false\" ", annotationKey, annotationValue)
		}

		klog.V(4).Infof("Found a Service Annotation: %v = %v", annotationKey, returnValue)
		return returnValue, nil
	}

	klog.V(4).Infof("Could not find a Service Annotation; falling back to default setting: %v = %v", annotationKey, defaultSetting)
	return defaultSetting, nil
}
