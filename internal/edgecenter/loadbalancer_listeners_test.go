package edgecenter

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sort"
	"testing"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// The real CCM reconciliation and SDK task-wait helpers run against this
// in-memory cloud. Mutations finish only when their task is polled. Unexpected
// LB deletion and overlapping frontend listeners fail the test immediately.
type listenerTestCloud struct {
	edgecloud.LoadbalancersService
	t               *testing.T
	lb              edgecloud.Loadbalancer
	listeners       map[string]edgecloud.Listener
	pools           map[string]edgecloud.Pool
	events          []string
	pending         func()
	result          map[string]interface{}
	failDelete      string
	failCreateOnce  bool
	loseCreateReply bool
	createCount     int
	replacementGets int
	failGetAt       int
}

type listenerTestTasks struct {
	edgecloud.TasksService
	cloud *listenerTestCloud
}

func listenerResponse(status int) *edgecloud.Response {
	return &edgecloud.Response{Response: &http.Response{StatusCode: status}}
}

func (f *listenerTestCloud) task(result map[string]interface{}, complete func()) (*edgecloud.TaskResponse, *edgecloud.Response, error) {
	require.Nil(f.t, f.pending, "cloud mutations must not overlap")
	f.result, f.pending = result, complete
	return &edgecloud.TaskResponse{Tasks: []string{"task"}}, listenerResponse(http.StatusAccepted), nil
}

func (f *listenerTestTasks) Get(context.Context, string) (*edgecloud.Task, *edgecloud.Response, error) {
	f.cloud.pending()
	f.cloud.pending = nil
	return &edgecloud.Task{State: edgecloud.TaskStateFinished, CreatedResources: f.cloud.result}, listenerResponse(http.StatusOK), nil
}

func (f *listenerTestCloud) Get(_ context.Context, id string) (*edgecloud.Loadbalancer, *edgecloud.Response, error) {
	require.Equal(f.t, f.lb.ID, id)
	require.Nil(f.t, f.pending, "must wait for cloud task completion before polling ACTIVE")
	lb := f.lb
	return &lb, listenerResponse(http.StatusOK), nil
}

func (f *listenerTestCloud) Delete(context.Context, string) (*edgecloud.TaskResponse, *edgecloud.Response, error) {
	f.t.Fatal("listener replacement must never delete the load balancer/VIP")
	return nil, nil, errors.New("unexpected LB deletion")
}

func (f *listenerTestCloud) ListenerList(context.Context, *edgecloud.ListenerListOptions) ([]edgecloud.Listener, *edgecloud.Response, error) {
	var ids []string
	for id := range f.listeners {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var listeners []edgecloud.Listener
	for _, id := range ids {
		listeners = append(listeners, f.listeners[id])
	}
	return listeners, listenerResponse(http.StatusOK), nil
}

func (f *listenerTestCloud) ListenerGet(_ context.Context, id string) (*edgecloud.Listener, *edgecloud.Response, error) {
	if id == "replacement" {
		f.replacementGets++
		if f.replacementGets == f.failGetAt {
			return nil, listenerResponse(http.StatusServiceUnavailable), errors.New("listener lookup temporarily failed")
		}
	}
	if listener, ok := f.listeners[id]; ok {
		return &listener, listenerResponse(http.StatusOK), nil
	}
	return nil, listenerResponse(http.StatusNotFound), errors.New("listener not found")
}

func (f *listenerTestCloud) ListenerCreate(_ context.Context, req *edgecloud.ListenerCreateRequest) (*edgecloud.TaskResponse, *edgecloud.Response, error) {
	require.Nil(f.t, f.pending, "must finish deletion before creating replacement")
	for _, existing := range f.listeners {
		require.False(f.t, existing.ProtocolPort == req.ProtocolPort &&
			listenerTransportProtocol(existing.Protocol) == listenerTransportProtocol(req.Protocol),
			"create-before-delete would cause cloud HTTP 409")
	}
	f.createCount++
	f.events = append(f.events, "create-listener")
	if f.failCreateOnce {
		f.failCreateOnce = false
		return nil, nil, errors.New("create temporarily failed")
	}
	listener := edgecloud.Listener{ID: "replacement", LoadbalancerID: req.LoadbalancerID,
		Name: req.Name, Protocol: req.Protocol, ProtocolPort: req.ProtocolPort, SecretID: req.SecretID}
	if f.loseCreateReply {
		f.loseCreateReply = false
		f.listeners[listener.ID] = listener
		return nil, nil, errors.New("create response lost")
	}
	return f.task(map[string]interface{}{"listeners": []string{listener.ID}}, func() {
		f.events = append(f.events, "created-listener")
		f.listeners[listener.ID] = listener
	})
}

func (f *listenerTestCloud) ListenerDelete(_ context.Context, id string) (*edgecloud.TaskResponse, *edgecloud.Response, error) {
	require.Nil(f.t, f.pending)
	for _, pool := range f.pools {
		for _, ref := range pool.Listeners {
			require.NotEqual(f.t, id, ref.ID, "pool must be removed before listener")
		}
	}
	f.events = append(f.events, "delete-listener")
	if f.failDelete == "listener" {
		return nil, nil, errors.New("listener deletion refused")
	}
	return f.task(map[string]interface{}{"listeners": []string{id}}, func() {
		f.events = append(f.events, "deleted-listener")
		delete(f.listeners, id)
	})
}

func (f *listenerTestCloud) PoolList(_ context.Context, opts *edgecloud.PoolListOptions) ([]edgecloud.Pool, *edgecloud.Response, error) {
	var pools []edgecloud.Pool
	for _, pool := range f.pools {
		if opts.ListenerID == "" {
			pools = append(pools, pool)
			continue
		}
		for _, ref := range pool.Listeners {
			if ref.ID == opts.ListenerID {
				pools = append(pools, pool)
			}
		}
	}
	return pools, listenerResponse(http.StatusOK), nil
}

func (f *listenerTestCloud) PoolGet(_ context.Context, id string) (*edgecloud.Pool, *edgecloud.Response, error) {
	if pool, ok := f.pools[id]; ok {
		return &pool, listenerResponse(http.StatusOK), nil
	}
	return nil, listenerResponse(http.StatusNotFound), errors.New("pool not found")
}

func (f *listenerTestCloud) PoolDelete(_ context.Context, id string) (*edgecloud.TaskResponse, *edgecloud.Response, error) {
	f.events = append(f.events, "delete-pool")
	if f.failDelete == "pool" {
		return nil, nil, errors.New("pool deletion refused")
	}
	return f.task(map[string]interface{}{}, func() {
		f.events = append(f.events, "deleted-pool")
		delete(f.pools, id)
	})
}

func (f *listenerTestCloud) PoolCreate(_ context.Context, req *edgecloud.PoolCreateRequest) (*edgecloud.TaskResponse, *edgecloud.Response, error) {
	f.events = append(f.events, "create-pool")
	pool := edgecloud.Pool{ID: "replacement-pool", Protocol: req.Protocol, Listeners: []edgecloud.ID{{ID: req.ListenerID}}}
	return f.task(map[string]interface{}{"pools": []string{pool.ID}}, func() {
		f.pools[pool.ID] = pool
	})
}

func newListenerTestCloud(t *testing.T, oldProtocol edgecloud.LoadbalancerListenerProtocol) (*LbaasV2, *listenerTestCloud) {
	t.Helper()
	f := &listenerTestCloud{
		t: t, lb: edgecloud.Loadbalancer{ID: "lb", Name: "consul", VipAddress: net.ParseIP("10.101.177.153"), ProvisioningStatus: edgecloud.ProvisioningStatusActive},
		listeners: map[string]edgecloud.Listener{"old": {ID: "old", Protocol: oldProtocol, ProtocolPort: 443}},
		pools:     map[string]edgecloud.Pool{"old-pool": {ID: "old-pool", Listeners: []edgecloud.ID{{ID: "old"}}}},
	}
	l := &LbaasV2{LoadBalancer: LoadBalancer{client: &edgecloud.Client{
		Loadbalancers: f, Tasks: &listenerTestTasks{cloud: f},
	}}}
	return l, f
}

var listenerTestPort = corev1.ServicePort{Name: "https", Protocol: corev1.ProtocolTCP, Port: 443, NodePort: 30578}

func TestListenerProtocolReplacementDeletesConflictBeforeCreation(t *testing.T) {
	for _, tc := range []struct {
		name string
		old  edgecloud.LoadbalancerListenerProtocol
		opts ServiceOptions
	}{
		{"tcp-to-tls", edgecloud.ListenerProtocolTCP, ServiceOptions{SecretID: "certificate"}},
		{"http-to-tls", edgecloud.ListenerProtocolHTTP, ServiceOptions{SecretID: "certificate"}},
		{"https-to-tls", edgecloud.ListenerProtocolHTTPS, ServiceOptions{SecretID: "certificate"}},
		{"tls-to-tcp", edgecloud.ListenerProtocolTerminatedHTTPS, ServiceOptions{}},
		{"tcp-to-http", edgecloud.ListenerProtocolTCP, ServiceOptions{KeepClientIP: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, cloud := newListenerTestCloud(t, tc.old)
			cloud.listeners["udp"] = edgecloud.Listener{ID: "udp", Protocol: edgecloud.ListenerProtocolUDP, ProtocolPort: 443}
			cloud.listeners["other-port"] = edgecloud.Listener{ID: "other-port", Protocol: edgecloud.ListenerProtocolTCP, ProtocolPort: 9092}
			listeners, obsolete, err := l.ensureLoadBalancerListeners(context.Background(), &cloud.lb, []corev1.ServicePort{listenerTestPort}, nil, &tc.opts)
			require.NoError(t, err)
			require.Equal(t, []string{"delete-pool", "deleted-pool", "delete-listener", "deleted-listener", "create-listener", "created-listener"}, cloud.events)
			require.Len(t, obsolete, 2, "deleted listener must not enter late cleanup")
			for _, listener := range obsolete {
				require.NotEqual(t, "old", listener.ID)
			}
			require.Contains(t, cloud.listeners, "udp", "same-number UDP frontend must not be removed during replacement")
			require.Contains(t, cloud.listeners, "other-port")
			pools, obsoletePools, err := l.ensureLoadBalancerPools(context.Background(), &cloud.lb,
				&corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{listenerTestPort}}}, listeners, &tc.opts)
			require.NoError(t, err)
			require.Empty(t, obsoletePools)
			pool := pools[servicePortKey(listenerTestPort)]
			wantPool := edgecloud.LoadbalancerPoolProtocol("TCP")
			if tc.opts.SecretID != "" || tc.opts.KeepClientIP {
				wantPool = edgecloud.LoadbalancerPoolProtocol("HTTP")
			}
			require.Equal(t, wantPool, pool.Protocol)
			require.Equal(t, "10.101.177.153", cloud.lb.VipAddress.String())
		})
	}
}

func TestListenerReplacementStopsOnDeletionFailure(t *testing.T) {
	for _, step := range []string{"pool", "listener"} {
		t.Run(step, func(t *testing.T) {
			l, cloud := newListenerTestCloud(t, edgecloud.ListenerProtocolTCP)
			cloud.failDelete = step
			_, _, err := l.ensureLoadBalancerListeners(context.Background(), &cloud.lb, []corev1.ServicePort{listenerTestPort}, nil, &ServiceOptions{SecretID: "certificate"})
			require.ErrorContains(t, err, "failed removing conflicting listener")
			require.Zero(t, cloud.createCount)
			require.Contains(t, cloud.listeners, "old")
			// A later reconcile can continue even if only the pool was removed.
			cloud.failDelete = ""
			_, _, err = l.ensureLoadBalancerListeners(context.Background(), &cloud.lb, []corev1.ServicePort{listenerTestPort}, nil, &ServiceOptions{SecretID: "certificate"})
			require.NoError(t, err)
		})
	}
}

func TestListenerReplacementRecoversFromCreateFailure(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		l, cloud := newListenerTestCloud(t, edgecloud.ListenerProtocolTCP)
		cloud.failCreateOnce, cloud.loseCreateReply = !lostReply, lostReply
		opts := &ServiceOptions{SecretID: "certificate"}
		_, _, err := l.ensureLoadBalancerListeners(context.Background(), &cloud.lb, []corev1.ServicePort{listenerTestPort}, nil, opts)
		require.Error(t, err)
		require.NotContains(t, cloud.listeners, "old")
		_, obsolete, err := l.ensureLoadBalancerListeners(context.Background(), &cloud.lb, []corev1.ServicePort{listenerTestPort}, nil, opts)
		require.NoError(t, err)
		require.Empty(t, obsolete)
		wantCreates := 2
		if lostReply {
			wantCreates = 1
		}
		require.Equal(t, wantCreates, cloud.createCount)
		// The completed transition must be idempotent on another reconcile.
		before := append([]string(nil), cloud.events...)
		_, _, err = l.ensureLoadBalancerListeners(context.Background(), &cloud.lb, []corev1.ServicePort{listenerTestPort}, nil, opts)
		require.NoError(t, err)
		require.Equal(t, before, cloud.events)
	}
}

func TestListenerReplacementRefusesSharedPoolDeletion(t *testing.T) {
	l, cloud := newListenerTestCloud(t, edgecloud.ListenerProtocolTCP)
	pool := cloud.pools["old-pool"]
	pool.Listeners = append(pool.Listeners, edgecloud.ID{ID: "unrelated"})
	cloud.pools[pool.ID] = pool
	_, _, err := l.ensureLoadBalancerListeners(context.Background(), &cloud.lb, []corev1.ServicePort{listenerTestPort}, nil, &ServiceOptions{SecretID: "certificate"})
	require.ErrorContains(t, err, "shared with listener unrelated")
	require.Empty(t, cloud.events)
	require.Contains(t, cloud.pools, "old-pool")
}

func TestListenerReplacementRecoversFromReloadFailure(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		l, cloud := newListenerTestCloud(t, edgecloud.ListenerProtocolTCP)
		cloud.failGetAt = failAt
		opts := &ServiceOptions{SecretID: "certificate"}
		_, _, err := l.ensureLoadBalancerListeners(context.Background(), &cloud.lb, []corev1.ServicePort{listenerTestPort}, nil, opts)
		require.ErrorContains(t, err, "listener lookup temporarily failed")
		before := append([]string(nil), cloud.events...)
		_, _, err = l.ensureLoadBalancerListeners(context.Background(), &cloud.lb, []corev1.ServicePort{listenerTestPort}, nil, opts)
		require.NoError(t, err)
		require.Equal(t, 1, cloud.createCount, "retry must reuse the created listener")
		require.Equal(t, before, cloud.events)
	}
}

func TestExistingListenerReloadFailureDoesNotPanicOrDelete(t *testing.T) {
	l, cloud := newListenerTestCloud(t, edgecloud.ListenerProtocolTerminatedHTTPS)
	delete(cloud.listeners, "old")
	cloud.listeners["replacement"] = edgecloud.Listener{ID: "replacement", Protocol: edgecloud.ListenerProtocolTerminatedHTTPS, ProtocolPort: 443, SecretID: "certificate"}
	cloud.failGetAt = 1
	_, _, err := l.ensureLoadBalancerListeners(context.Background(), &cloud.lb, []corev1.ServicePort{listenerTestPort}, nil, &ServiceOptions{SecretID: "certificate"})
	require.ErrorContains(t, err, "failed to reload listener replacement after update")
	require.Empty(t, cloud.events)
}

func TestListenerDifferentTransportDoesNotConflict(t *testing.T) {
	l, cloud := newListenerTestCloud(t, edgecloud.ListenerProtocolUDP)
	delete(cloud.pools, "old-pool")
	_, obsolete, err := l.ensureLoadBalancerListeners(context.Background(), &cloud.lb, []corev1.ServicePort{listenerTestPort}, nil, &ServiceOptions{})
	require.NoError(t, err)
	require.Contains(t, cloud.listeners, "old")
	require.Len(t, obsolete, 1)
	require.Equal(t, []string{"create-listener", "created-listener"}, cloud.events)
}
