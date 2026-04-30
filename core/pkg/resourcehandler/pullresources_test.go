package resourcehandler

import (
	"context"
	"fmt"
	"testing"

	"github.com/kubescape/k8s-interface/k8sinterface"
	"github.com/kubescape/kubescape/v3/core/cautils"
	"github.com/kubescape/opa-utils/reporthandling/apis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"k8s.io/client-go/dynamic/fake"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
)

// gvrToListKind registers the GVRs used in these tests so the fake dynamic
// client doesn't panic when List is called on them.
var testGVRToListKind = map[schema.GroupVersionResource]string{
	{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: "ClusterRoleBindingList",
	{Group: "", Version: "v1", Resource: "pods"}: "PodList",
	{Group: "", Version: "v1", Resource: "somecrd"}:                                       "SomeCRDList",
}

// newHandlerWithReactor builds a K8sResourceHandler whose dynamic client
// prepends a reactor so tests can inject per-GVR errors.
func newHandlerWithReactor(t *testing.T, reactor k8stesting.ReactionFunc) *K8sResourceHandler {
	t.Helper()
	client := fakeclientset.NewClientset()
	dynClient := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testGVRToListKind)
	dynClient.Fake.PrependReactor("list", "*", reactor)

	k8s := &k8sinterface.KubernetesApi{
		KubernetesClient: client,
		DynamicClient:    dynClient,
		DiscoveryClient:  client.Discovery(),
		Context:          context.Background(),
	}
	return NewK8sResourceHandler(k8s, nil, nil, "test-cluster")
}

// TestPullResources_NonForbiddenErrorRecorded verifies that a non-404 API error
// (e.g. 403 Forbidden) is recorded in failedQueries so the caller can surface
// the affected control as skipped rather than falsely passed.
func TestPullResources_NonForbiddenErrorRecorded(t *testing.T) {
	forbiddenErr := fmt.Errorf("forbidden: User cannot list clusterrolebindings")

	handler := newHandlerWithReactor(t, func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, forbiddenErr
	})

	qrs := QueryableResources{
		"rbac.authorization.k8s.io/v1/clusterrolebindings": QueryableResource{
			GroupVersionResourceTriplet: "rbac.authorization.k8s.io/v1/clusterrolebindings",
		},
	}

	_, _, failedQueries := handler.pullResources(qrs, &EmptySelector{})

	require.Len(t, failedQueries, 1, "expected one failed query entry")
	for _, f := range failedQueries {
		assert.Equal(t, "rbac.authorization.k8s.io/v1/clusterrolebindings", f.gvr)
		assert.ErrorContains(t, f.err, "forbidden")
	}
}

// TestPullResources_NotFoundErrorIgnored verifies that a "server could not find
// the requested resource" error (CRD not installed) is silently ignored and does
// NOT appear in failedQueries — this is expected behaviour when a control
// references an optional CRD that isn't present on the cluster.
func TestPullResources_NotFoundErrorIgnored(t *testing.T) {
	handler := newHandlerWithReactor(t, func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("the server could not find the requested resource")
	})

	qrs := QueryableResources{
		"/v1/somecrd": QueryableResource{
			GroupVersionResourceTriplet: "/v1/somecrd",
		},
	}

	_, _, failedQueries := handler.pullResources(qrs, &EmptySelector{})

	assert.Empty(t, failedQueries, "404-style errors should not be recorded as failures")
}

// TestPullResources_PartialFailure verifies that when one GVR succeeds and
// another fails, only the failed GVR appears in failedQueries and allResources
// is still non-empty (scan continues).
func TestPullResources_PartialFailure(t *testing.T) {
	forbiddenGVR := "rbac.authorization.k8s.io/v1/clusterrolebindings"

	handler := newHandlerWithReactor(t, func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetResource().Resource == "clusterrolebindings" {
			return true, nil, fmt.Errorf("forbidden: cannot list clusterrolebindings")
		}
		// pods succeeds — return empty list
		return false, nil, nil
	})

	qrs := QueryableResources{
		forbiddenGVR: QueryableResource{
			GroupVersionResourceTriplet: forbiddenGVR,
		},
		"/v1/pods": QueryableResource{
			GroupVersionResourceTriplet: "/v1/pods",
		},
	}

	_, _, failedQueries := handler.pullResources(qrs, &EmptySelector{})

	assert.Len(t, failedQueries, 1)
	for _, f := range failedQueries {
		assert.Equal(t, forbiddenGVR, f.gvr)
	}
}

// TestPullResources_TotalFailure verifies that when every query fails,
// failedQueries contains all of them and allResources is empty.
func TestPullResources_TotalFailure(t *testing.T) {
	handler := newHandlerWithReactor(t, func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("forbidden: no permissions")
	})

	qrs := QueryableResources{
		"rbac.authorization.k8s.io/v1/clusterrolebindings": QueryableResource{
			GroupVersionResourceTriplet: "rbac.authorization.k8s.io/v1/clusterrolebindings",
		},
		"/v1/pods": QueryableResource{
			GroupVersionResourceTriplet: "/v1/pods",
		},
	}

	_, allResources, failedQueries := handler.pullResources(qrs, &EmptySelector{})

	assert.Empty(t, allResources, "no resources should be collected when all queries fail")
	assert.Len(t, failedQueries, 2, "both failed GVRs should be recorded")
}

// TestGetResources_InfoMapWrittenWhenGVRTotallyAbsent verifies that when a GVR
// fails and k8sResourcesMap has no data for it, the GVR is written to InfoMap
// so mapControlToInfo can mark the affected controls as skipped.
func TestGetResources_InfoMapWrittenWhenGVRTotallyAbsent(t *testing.T) {
	failedGVR := "rbac.authorization.k8s.io/v1/clusterrolebindings"
	infoMap := map[string]apis.StatusInfo{}

	// simulate: the GVR failed AND k8sResourcesMap has no entries for it
	k8sResourcesMap := cautils.K8SResources{
		failedGVR: []string{}, // empty — no successful pull
	}
	failedQueries := map[string]queryFailure{
		failedGVR: {gvr: failedGVR, err: fmt.Errorf("forbidden")},
	}

	for _, f := range failedQueries {
		if len(k8sResourcesMap[f.gvr]) > 0 {
			continue
		}
		cautils.SetInfoMapForResources(f.err.Error(), []string{f.gvr}, infoMap)
	}

	info, ok := infoMap[failedGVR]
	require.True(t, ok, "InfoMap should have an entry for the failed GVR")
	assert.Equal(t, apis.StatusSkipped, info.InnerStatus)
	assert.Contains(t, info.InnerInfo, "forbidden")
}

// TestGetResources_InfoMapNotWrittenWhenGVRHasData verifies that when a GVR
// failed for one field-selector query but another query for the same GVR
// succeeded and populated k8sResourcesMap, InfoMap is NOT written — preventing
// controls from being incorrectly marked skipped when they do have data.
func TestGetResources_InfoMapNotWrittenWhenGVRHasData(t *testing.T) {
	gvr := "/v1/pods"
	infoMap := map[string]apis.StatusInfo{}

	// One namespace selector succeeded and added a resource ID.
	k8sResourcesMap := cautils.K8SResources{
		gvr: []string{"default/pod-abc"},
	}
	failedQueries := map[string]queryFailure{
		gvr + "/metadata.namespace=prod": {gvr: gvr, err: fmt.Errorf("forbidden for prod")},
	}

	for _, f := range failedQueries {
		if len(k8sResourcesMap[f.gvr]) > 0 {
			continue
		}
		cautils.SetInfoMapForResources(f.err.Error(), []string{f.gvr}, infoMap)
	}

	assert.Empty(t, infoMap, "InfoMap should NOT be written when k8sResourcesMap already has data for the GVR")
}
