package edgecenter

import (
	"fmt"
	"strings"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// cutString makes sure the string length doesn't exceed 63, which is usually the maximum string length in Edgecenter.
func cutString(original string) string {
	ret := original
	if len(original) > maxNameLength {
		ret = original[:maxNameLength]
	}
	return ret
}

func poolMemberKey(m edgecloud.PoolMember) string {
	addr := ""
	if m.Address != nil {
		addr = m.Address.String()
	}
	return fmt.Sprintf("%s:%d|%s", addr, m.ProtocolPort, m.SubnetID)
}

func poolMemberCreateKey(m edgecloud.PoolMemberCreateRequest) string {
	addr := ""
	if m.Address != nil {
		addr = m.Address.String()
	}
	return fmt.Sprintf("%s:%d|%s", addr, m.ProtocolPort, m.SubnetID)
}

func servicePortKey(sp corev1.ServicePort) string {
	if sp.Name != "" {
		return fmt.Sprintf("%s-%s-%d", sp.Name, strings.ToLower(string(sp.Protocol)), sp.Port)
	}
	return fmt.Sprintf("%s-%d", strings.ToLower(string(sp.Protocol)), sp.Port)
}

func buildListenerName(lbName string, sp corev1.ServicePort) string {
	return strings.TrimSuffix(
		cutString(fmt.Sprintf("listener_%s_%s", servicePortKey(sp), lbName)),
		"-",
	)
}

func buildPoolName(lbName string, sp corev1.ServicePort) string {
	return strings.TrimSuffix(
		cutString(fmt.Sprintf("pool_%s_%s", servicePortKey(sp), lbName)),
		"-",
	)
}

func intPtrEqual(a, b *int) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

func logLBPhaseStart(serviceName, phase string) {
	klog.V(2).Infof("[LB][%s] Phase: %s START", serviceName, phase)
}

func logLBPhaseDone(serviceName, phase string, format string, args ...any) {
	msg := ""
	if format != "" {
		msg = fmt.Sprintf(" "+format, args...)
	}
	klog.V(2).Infof("[LB][%s] Phase: %s DONE%s", serviceName, phase, msg)
}

func logLBPhaseFailed(serviceName, phase string, err error) {
	klog.Errorf("[LB][%s] Phase: %s FAILED: %v", serviceName, phase, err)
}

func logLBWaitStart(serviceName, what string) {
	klog.V(2).Infof("[LB][%s] Wait: %s START", serviceName, what)
}

func logLBWaitDone(serviceName, what string) {
	klog.V(2).Infof("[LB][%s] Wait: %s DONE", serviceName, what)
}

func logLBWaitFailed(serviceName, what string, err error) {
	klog.Errorf("[LB][%s] Wait: %s FAILED: %v", serviceName, what, err)
}
