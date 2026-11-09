package edgecenter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func tlsService(mode string) *corev1.Service {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "consul", Namespace: "test",
		Annotations: map[string]string{
			ServiceAnnotationLoadBalancerDefaultTLSContainerRef: "cloud-secret-id",
		},
	}}
	if mode != "" {
		svc.Annotations[ServiceAnnotationLoadBalancerTLSSecretMode] = mode
	}
	return svc
}

func tlsBalancer(t *testing.T, status int, body string, calls *int) *LbaasV2 {
	t.Helper()
	cfg := EdgecenterOpts{ApiURL: "https://cloud.invalid", ApiToken: "test", ProjectID: 11, RegionID: 22, ClusterID: "cluster-1"}
	client, err := NewEdgecenterClient(cfg, "test")
	require.NoError(t, err)
	client.HTTPClient.Transport = regionTransport(func(req *http.Request) (*http.Response, error) {
		*calls = *calls + 1
		require.Equal(t, http.MethodPost, req.Method)
		require.Equal(t, "/internal/mkaas/v2/clusters/11/22/cluster-1/secrets", req.URL.Path)
		var payload SecretSchema
		require.NoError(t, json.NewDecoder(req.Body).Decode(&payload))
		require.Equal(t, "cloud-secret-id", payload.SecretId)
		return &http.Response{
			StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: req,
		}, nil
	})
	return &LbaasV2{LoadBalancer: LoadBalancer{client: client, edgecenter: cfg}}
}

func TestTLSSecretDefaultAndExplicitMKaaSModeTranslate(t *testing.T) {
	for _, mode := range []string{"", "mkaas"} {
		t.Run("mode="+mode, func(t *testing.T) {
			calls := 0
			l := tlsBalancer(t, http.StatusOK, `{"secret_id":"technical-secret-id"}`, &calls)
			id, err := l.getSecretID(context.Background(), tlsService(mode))
			require.NoError(t, err)
			require.Equal(t, "technical-secret-id", id)
			require.Equal(t, 1, calls)
		})
	}
}

func TestTLSSecretDirectModeDoesNotCallMKaaS(t *testing.T) {
	calls := 0
	l := tlsBalancer(t, http.StatusNotFound, `{"message":"cluster not found"}`, &calls)
	id, err := l.getSecretID(context.Background(), tlsService("direct"))
	require.NoError(t, err)
	require.Equal(t, "cloud-secret-id", id)
	require.Zero(t, calls)
}

func TestTLSSecretDirectModeIsServiceLocalAndRegionIndependent(t *testing.T) {
	l := regionalBalancer(t)
	for _, region := range []nodeRegion{l.nodeRegions.regions[0], l.nodeRegions.regions[1]} {
		region.client.HTTPClient.Transport = regionTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("direct mode must not call MKaaS in either region")
			return nil, nil
		})
	}
	svc := tlsService("direct")
	svc.Annotations[ServiceAnnotationLoadBalancerRegionID] = "2"
	regional, err := l.forService(svc)
	require.NoError(t, err)
	id, err := regional.getSecretID(context.Background(), svc)
	require.NoError(t, err)
	require.Equal(t, "cloud-secret-id", id)
	require.Equal(t, 2, regional.edgecenter.RegionID)
	require.Equal(t, 1, l.edgecenter.RegionID)
	require.Equal(t, "dt-network", regional.opts.NetworkID)
	require.Equal(t, "dm-network", l.opts.NetworkID)
}

func TestTLSSecretMissingReferenceDoesNotCallMKaaS(t *testing.T) {
	calls := 0
	l := tlsBalancer(t, http.StatusNotFound, `{}`, &calls)
	svc := tlsService("direct")
	delete(svc.Annotations, ServiceAnnotationLoadBalancerDefaultTLSContainerRef)
	id, err := l.getSecretID(context.Background(), svc)
	require.NoError(t, err)
	require.Empty(t, id)
	require.Zero(t, calls)
}

func TestTLSSecretUnknownModeFailsWithoutAPICall(t *testing.T) {
	for _, mode := range []string{"auto", "DIRECT", "invalid", " "} {
		calls := 0
		l := tlsBalancer(t, http.StatusOK, `{}`, &calls)
		id, err := l.getSecretID(context.Background(), tlsService(mode))
		require.ErrorContains(t, err, ServiceAnnotationLoadBalancerTLSSecretMode)
		require.Empty(t, id)
		require.Zero(t, calls)
	}
}

func TestTLSSecretMKaaSErrorsNeverFallBackToDirectMode(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusInternalServerError} {
		calls := 0
		l := tlsBalancer(t, status, `{"message":"cannot resolve cluster secret"}`, &calls)
		id, err := l.getSecretID(context.Background(), tlsService(""))
		require.ErrorContains(t, err, "failed to get tech secret")
		require.Empty(t, id)
		require.Equal(t, 1, calls)
	}
}

func TestTLSSecretEmptyMKaaSResponseFailsClosed(t *testing.T) {
	for _, body := range []string{`{}`, `{"secret_id":""}`, `{"secret_id":" "}`} {
		calls := 0
		l := tlsBalancer(t, http.StatusOK, body, &calls)
		id, err := l.getSecretID(context.Background(), tlsService(""))
		require.ErrorContains(t, err, "empty tech secret ID")
		require.Empty(t, id)
		require.Equal(t, 1, calls)
	}
}
