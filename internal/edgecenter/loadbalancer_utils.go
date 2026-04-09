package edgecenter

import (
	"context"
	"fmt"
	"strings"
	"time"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
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

// poolMemberKey returns a stable comparable key for an existing pool member.
// The key includes address, protocol port and subnet so member sets can be compared reliably.
func poolMemberKey(m edgecloud.PoolMember) string {
	addr := ""
	if m.Address != nil {
		addr = m.Address.String()
	}
	return fmt.Sprintf("%s:%d|%s", addr, m.ProtocolPort, m.SubnetID)
}

// poolMemberCreateKey returns a stable comparable key for a desired pool member request.
// It mirrors poolMemberKey so current and desired member sets can be matched using the same format.
func poolMemberCreateKey(m edgecloud.PoolMemberCreateRequest) string {
	addr := ""
	if m.Address != nil {
		addr = m.Address.String()
	}
	return fmt.Sprintf("%s:%d|%s", addr, m.ProtocolPort, m.SubnetID)
}

// servicePortKey returns a stable identifier for a Service port.
// If the port has a name, it is included to distinguish ports that may share protocol semantics.
func servicePortKey(sp corev1.ServicePort) string {
	if sp.Name != "" {
		return fmt.Sprintf("%s-%s-%d", sp.Name, strings.ToLower(string(sp.Protocol)), sp.Port)
	}
	return fmt.Sprintf("%s-%d", strings.ToLower(string(sp.Protocol)), sp.Port)
}

// buildListenerName returns the listener name derived from the service port key and load balancer name.
// The name is trimmed to the Edgecenter maximum allowed length and normalized to avoid trailing dashes.
func buildListenerName(lbName string, sp corev1.ServicePort) string {
	return strings.TrimSuffix(
		cutString(fmt.Sprintf("listener_%s_%s", servicePortKey(sp), lbName)),
		"-",
	)
}

// buildPoolName returns the pool name derived from the service port key and load balancer name.
// The name is trimmed to the Edgecenter maximum allowed length and normalized to avoid trailing dashes.
func buildPoolName(lbName string, sp corev1.ServicePort) string {
	return strings.TrimSuffix(
		cutString(fmt.Sprintf("pool_%s_%s", servicePortKey(sp), lbName)),
		"-",
	)
}

// intPtrEqual compares two integer pointers by value and treats two nil pointers as equal.
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

// logLBPhaseStart writes a standard debug log line marking the start of a reconciliation phase.
func logLBPhaseStart(serviceName, phase string) {
	klog.V(2).Infof("[LB][%s] Phase: %s START", serviceName, phase)
}

// logLBPhaseDone writes a standard debug log line marking successful completion of a reconciliation phase.
// An optional formatted suffix may be included for counts or additional context.
func logLBPhaseDone(serviceName, phase string, format string, args ...any) {
	msg := ""
	if format != "" {
		msg = fmt.Sprintf(" "+format, args...)
	}
	klog.V(2).Infof("[LB][%s] Phase: %s DONE%s", serviceName, phase, msg)
}

// logLBPhaseFailed writes a standard error log line for a failed reconciliation phase.
func logLBPhaseFailed(serviceName, phase string, err error) {
	klog.Errorf("[LB][%s] Phase: %s FAILED: %v", serviceName, phase, err)
}

// logLBWaitStart writes a standard debug log line marking the start of a wait step.
func logLBWaitStart(serviceName, what string) {
	klog.V(2).Infof("[LB][%s] Wait: %s START", serviceName, what)
}

// logLBWaitDone writes a standard debug log line marking successful completion of a wait step.
func logLBWaitDone(serviceName, what string) {
	klog.V(2).Infof("[LB][%s] Wait: %s DONE", serviceName, what)
}

// logLBWaitFailed writes a standard error log line for a failed wait step.
func logLBWaitFailed(serviceName, what string, err error) {
	klog.Errorf("[LB][%s] Wait: %s FAILED: %v", serviceName, what, err)
}

// isLoadBalancerImmutableError reports whether an operation failed because the load balancer
// is temporarily in a backend immutable/processing state and the operation should be retried.
func isLoadBalancerImmutableError(err error) bool {
	if err == nil {
		return false
	}

	msg := err.Error()
	return strings.Contains(msg, "immutable state") ||
		strings.Contains(msg, "is not allowed to process")
}

// runLBOperationWithRetry executes a mutating load balancer operation with retry on transient
// immutable-state errors. It waits for the LB to be ACTIVE before and after the operation so
// sequential API mutations are less likely to fail due to backend state lag.
func (l *LbaasV2) runLBOperationWithRetry(ctx context.Context, lbID string, opName string, resourceID string, op func(context.Context) error) error {
	backoff := wait.Backoff{
		Duration: 2 * time.Second,
		Factor:   1.5,
		Steps:    6,
	}

	var lastErr error

	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		if _, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, lbID); err != nil {
			lastErr = fmt.Errorf("LB not ACTIVE before %s for %s: %w", opName, resourceID, err)
			return false, nil
		}

		if err := op(ctx); err != nil {
			if isLoadBalancerImmutableError(err) {
				lastErr = err
				klog.Warningf("%s for %s hit immutable LB state, retrying", opName, resourceID)
				return false, nil
			}
			return false, err
		}

		if _, err := waitLoadbalancerActiveProvisioningStatus(ctx, l.client, lbID); err != nil {
			lastErr = fmt.Errorf("LB not ACTIVE after %s for %s: %w", opName, resourceID, err)
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
