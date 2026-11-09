package edgecenter

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// SecretSchema is used to resolve a user-facing secret ID
// into the internal technical secret ID expected by MKaaS APIs.
type SecretSchema struct {
	SecretId string `json:"secret_id"`
}

// techSecretsPath returns the MKaaS internal API path used
// to resolve service TLS secrets for the current cluster.
func (l *LbaasV2) techSecretsPath() string {
	return fmt.Sprintf(
		"/internal%s/%d/%d/%s/secrets",
		edgecloud.MKaaSClustersBasePathV2,
		l.edgecenter.ProjectID,
		l.edgecenter.RegionID,
		l.edgecenter.ClusterID,
	)
}

// resolveTechSecretID resolves a client-visible secret ID to the internal
// technical secret ID used by the load balancer API.
func (l *LbaasV2) resolveTechSecretID(ctx context.Context, secretID string) (string, error) {
	req, err := l.client.NewRequest(ctx, http.MethodPost, l.techSecretsPath(), &SecretSchema{SecretId: secretID})
	if err != nil {
		return "", fmt.Errorf("creating tech secret request: %w", err)
	}

	var secretResp SecretSchema
	if _, err = l.client.Do(ctx, req, &secretResp); err != nil {
		return "", fmt.Errorf("resolving tech secret: %w", err)
	}

	return secretResp.SecretId, nil
}

// getSecretID resolves a Service TLS secret. MKaaS translation remains the default;
// direct mode explicitly selects a cloud secret in the load balancer's own scope.
func (l *LbaasV2) getSecretID(ctx context.Context, svc *corev1.Service) (string, error) {
	clientSecretID := getStringFromServiceAnnotation(svc, ServiceAnnotationLoadBalancerDefaultTLSContainerRef, "")
	if clientSecretID == "" {
		return "", nil
	}

	svcRef := fmt.Sprintf("%s/%s", svc.Namespace, svc.Name)
	mode := strings.TrimSpace(getStringFromServiceAnnotation(svc, ServiceAnnotationLoadBalancerTLSSecretMode, "mkaas"))
	switch mode {
	case "direct":
		// Self-managed clusters have no MKaaS cluster record. Their credentials
		// already address the project/region that owns the certificate and LB.
		// The LB API still validates the secret's ownership and permissions.
		return clientSecretID, nil
	case "mkaas":
		// Preserve translation for managed clusters; never silently fall back
		// to direct mode on API errors, including cluster-not-found responses.
	default:
		return "", fmt.Errorf("invalid %s value %q for service %s: use mkaas or direct", ServiceAnnotationLoadBalancerTLSSecretMode, mode, svcRef)
	}
	klog.V(4).Infof("TLS secret ID %s found for service %s", clientSecretID, svcRef)

	techSecretID, err := l.resolveTechSecretID(ctx, clientSecretID)
	if err != nil {
		return "", fmt.Errorf("failed to get tech secret for service %s: %w", svcRef, err)
	}
	if strings.TrimSpace(techSecretID) == "" {
		return "", fmt.Errorf("empty tech secret ID returned for service %s", svcRef)
	}

	klog.V(4).Infof("Tech secret ID %s obtained for client secret %s", techSecretID, clientSecretID)
	return techSecretID, nil
}
