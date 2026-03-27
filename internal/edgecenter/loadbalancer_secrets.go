package edgecenter

import (
	"context"
	"fmt"
	"net/http"

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

// getSecretID resolves a client-facing secret ID from the Service annotation
// into the corresponding internal tech secret ID used by Edgecenter LB APIs.
func (l *LbaasV2) getSecretID(ctx context.Context, svc *corev1.Service) (string, error) {
	clientSecretID := getStringFromServiceAnnotation(svc, ServiceAnnotationLoadBalancerDefaultTLSContainerRef, "")
	if clientSecretID == "" {
		return "", nil
	}

	svcRef := fmt.Sprintf("%s/%s", svc.Namespace, svc.Name)
	klog.V(4).Infof("TLS secret ID %s found for service %s", clientSecretID, svcRef)

	techSecretID, err := l.resolveTechSecretID(ctx, clientSecretID)
	if err != nil {
		return "", fmt.Errorf("failed to get tech secret for service %s: %w", svcRef, err)
	}

	klog.V(4).Infof("Tech secret ID %s obtained for client secret %s", techSecretID, clientSecretID)
	return techSecretID, nil
}
