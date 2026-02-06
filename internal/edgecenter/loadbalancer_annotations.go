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
	// loadbalancerActive* is configuration of exponential backoff for
	// going into ACTIVE loadbalancer provisioning status. Starting with 1
	// seconds, multiplying by 1.2 with each step and taking 19 steps at maximum
	// it will time out after 128s, which roughly corresponds to 120s
	loadbalancerActiveInitDelay = 1 * time.Second
	loadbalancerActiveFactor    = 1.2
	loadbalancerActiveSteps     = 19

	// loadbalancerDelete* is configuration of exponential backoff for
	// waiting for delete operation to complete. Starting with 1
	// seconds, multiplying by 1.2 with each step and taking 13 steps at maximum
	// it will time out after 32s, which roughly corresponds to 30s
	loadbalancerDeleteInitDelay = 1 * time.Second
	loadbalancerDeleteFactor    = 1.2
	loadbalancerDeleteSteps     = 13

	activeStatus = "ACTIVE" // nolint
	errorStatus  = "ERROR"  // nolint

	defaultLBProtocol = edgecloud.ListenerProtocolTCP
	maxNameLength     = 63

	ServiceAnnotationLoadBalancerConnLimit            = "loadbalancer.edgecenter.com/connection-limit"                 // nolint
	ServiceAnnotationLoadBalancerFloatingNetworkID    = "loadbalancer.edgecenter.com/floating-network-id"              // nolint
	ServiceAnnotationLoadBalancerFloatingSubnet       = "loadbalancer.edgecenter.com/floating-subnet"                  // nolint
	ServiceAnnotationLoadBalancerFloatingSubnetID     = "loadbalancer.edgecenter.com/floating-subnet-id"               // nolint
	ServiceAnnotationLoadBalancerInternal             = "service.beta.kubernetes.io/edgecenter-internal-load-balancer" // nolint
	ServiceAnnotationSaveFloating                     = "loadbalancer.edgecenter.com/save-floating"                    // nolint
	ServiceAnnotationLoadBalancerClass                = "loadbalancer.edgecenter.com/class"                            // nolint
	ServiceAnnotationLoadBalancerKeepFloatingIP       = "loadbalancer.edgecenter.com/keep-floatingip"                  // nolint
	ServiceAnnotationLoadBalancerPortID               = "loadbalancer.edgecenter.com/port-id"                          // nolint
	ServiceAnnotationLoadBalancerProxyEnabled         = "loadbalancer.edgecenter.com/proxy-protocol"                   // nolint
	ServiceAnnotationLoadBalancerSubnetID             = "loadbalancer.edgecenter.com/subnet-id"                        // nolint
	ServiceAnnotationLoadBalancerNetworkID            = "loadbalancer.edgecenter.com/network-id"                       // nolint
	ServiceAnnotationLoadBalancerTimeoutClientData    = "loadbalancer.edgecenter.com/timeout-client-data"              // nolint
	ServiceAnnotationLoadBalancerTimeoutMemberConnect = "loadbalancer.edgecenter.com/timeout-member-connect"           // nolint
	ServiceAnnotationLoadBalancerTimeoutMemberData    = "loadbalancer.edgecenter.com/timeout-member-data"              // nolint
	ServiceAnnotationLoadBalancerTimeoutTCPInspect    = "loadbalancer.edgecenter.com/timeout-tcp-inspect"              // nolint
	ServiceAnnotationLoadBalancerXForwardedFor        = "loadbalancer.edgecenter.com/x-forwarded-for"                  // nolint

	// ServiceAnnotationLoadBalancerInternal is the annotation used on the service
	// to indicate that we want an internal loadbalancer service.
	// If the value of ServiceAnnotationLoadBalancerInternal is false, it indicates that we want an external loadbalancer service. Default to false.
	k8sIngressTag = "k8s_ingress"
)

// getStringFromServiceAnnotation searches a given v1.Service for a specific annotationKey and either returns the annotation's value or a specified defaultSetting
func getStringFromServiceAnnotation(service *corev1.Service, annotationKey string, defaultSetting string) string {
	klog.V(4).Infof("getStringFromServiceAnnotation(%v, %v, %v)", service, annotationKey, defaultSetting)
	if annotationValue, ok := service.Annotations[annotationKey]; ok {
		// if there is an annotation for this setting, set the "setting" var to it
		// annotationValue can be empty, it is working as designed
		// it makes possible for instance provisioning loadbalancer without floatingip
		klog.V(4).Infof("Found a Service Annotation: %v = %v", annotationKey, annotationValue)
		return annotationValue
	}
	//if there is no annotation, set "settings" var to the value from cloud config
	klog.V(4).Infof("Could not find a Service Annotation; falling back on cloud-config setting: %v = %v", annotationKey, defaultSetting)
	return defaultSetting
}

// getBoolFromServiceAnnotation searches a given v1.Service for a specific annotationKey and either returns the annotation's value or a specified defaultSetting
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
