package edgecenter

import (
	"fmt"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	corev1 "k8s.io/api/core/v1"
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

func nodePortForListener(svc *corev1.Service, ln *edgecloud.Listener) (int, error) {
	for i := range svc.Spec.Ports {
		sp := &svc.Spec.Ports[i]
		if int32(ln.ProtocolPort) == sp.Port {
			if sp.NodePort == 0 {
				return 0, fmt.Errorf("service port %d matched listener %s but nodePort is 0", sp.Port, ln.ID)
			}
			return int(sp.NodePort), nil
		}
	}
	return 0, fmt.Errorf("no ServicePort matched listener %s (listener protocolPort=%d)", ln.ID, ln.ProtocolPort)
}
