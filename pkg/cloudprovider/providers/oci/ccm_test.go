package oci

import (
	"errors"
	"fmt"
	"testing"

	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	providercfg "github.com/oracle/oci-cloud-controller-manager/pkg/cloudprovider/providers/oci/config"
)

// TestNewCloudProvider_MinimalInstancePrincipalConfig verifies that a minimal
// config (useInstancePrincipals + compartment only, no loadBalancer/vcn/subnet
// block) does not panic during NewCloudProvider. Regression test for the nil
// pointer dereference on config.LoadBalancer when the optional loadBalancer
// config block is absent.
func TestNewCloudProvider_MinimalInstancePrincipalConfig(t *testing.T) {
	cfg := &providercfg.Config{
		UseInstancePrincipals: true,
		CompartmentID:         "ocid1.compartment.oc1..testvalue",
		// LoadBalancer, RateLimiter, Metrics, Tags intentionally nil/zero to
		// exercise the minimal-config path.
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NewCloudProvider panicked with minimal instance-principal config: %v", r)
		}
	}()

	// NewCloudProvider will attempt real instance-principal auth setup and
	// OCI client construction, which is expected to fail in a non-OCI test
	// environment. The point of this test is solely that it returns an error
	// instead of panicking, and that the optional config blocks (LoadBalancer,
	// RateLimiter, Metrics, Tags) are handled safely when absent.
	_, err := NewCloudProvider(cfg)
	if err != nil {
		t.Logf("NewCloudProvider returned expected non-panic error in test environment: %v", err)
	}
}

type stubNodeInformer struct {
	lister listersv1.NodeLister
}

func (f *stubNodeInformer) Informer() cache.SharedIndexInformer {
	return nil
}

func (f *stubNodeInformer) Lister() listersv1.NodeLister {
	return f.lister
}

type stubNodeLister struct {
	nodes   []*v1.Node
	listErr error
}

func (f *stubNodeLister) List(selector labels.Selector) ([]*v1.Node, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if selector == nil {
		return append([]*v1.Node(nil), f.nodes...), nil
	}
	var filtered []*v1.Node
	for _, node := range f.nodes {
		if selector.Matches(labels.Set(node.Labels)) {
			filtered = append(filtered, node)
		}
	}
	return filtered, nil
}

func (f *stubNodeLister) Get(name string) (*v1.Node, error) {
	for _, node := range f.nodes {
		if node.Name == name {
			return node, nil
		}
	}
	return nil, fmt.Errorf("node %q not found", name)
}

func TestIsOpenShiftCluster(t *testing.T) {
	logger := zap.NewNop().Sugar()
	cp := &CloudProvider{logger: logger}

	workerNode := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-1",
			Labels: map[string]string{
				"node-role.kubernetes.io/worker": "",
			},
		},
	}
	infraNode := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "infra-1",
			Labels: map[string]string{
				"node-role.kubernetes.io/infra": "",
				"openshift.io/cluster":          "true",
			},
		},
	}

	tests := []struct {
		name     string
		labelEnv string
		lister   listersv1.NodeLister
		want     bool
	}{
		{
			name:     "empty label identifier falls back and returns true when default label exists",
			labelEnv: "",
			lister: &stubNodeLister{nodes: []*v1.Node{{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "n1",
					Labels: map[string]string{"node.openshift.io/os_id": "rhel"},
				},
			}}},
			want: true,
		},
		{
			name:     "empty label identifier falls back and returns false when default label missing",
			labelEnv: "",
			lister: &stubNodeLister{nodes: []*v1.Node{{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "n2",
					Labels: map[string]string{"some": "label"},
				},
			}}},
			want: false,
		},
		{
			name:     "empty label identifier falls back and returns false when default label value is not rhel",
			labelEnv: "",
			lister: &stubNodeLister{nodes: []*v1.Node{{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "n3",
					Labels: map[string]string{"node.openshift.io/os_id": "rhcos"},
				},
			}}},
			want: false,
		},
		{
			name:     "invalid label identifier returns false",
			labelEnv: "openshift.io/cluster=",
			lister:   &stubNodeLister{},
			want:     false,
		},
		{
			name:     "list error returns false",
			labelEnv: "openshift.io/cluster=true",
			lister: &stubNodeLister{
				listErr: errors.New("list failed"),
			},
			want: false,
		},
		{
			name:     "no matching nodes returns false",
			labelEnv: "openshift.io/cluster=true",
			lister: &stubNodeLister{
				nodes: []*v1.Node{workerNode},
			},
			want: false,
		},
		{
			name:     "equals selector matches node",
			labelEnv: " openshift.io/cluster = true ",
			lister: &stubNodeLister{
				nodes: []*v1.Node{workerNode, infraNode},
			},
			want: true,
		},
		{
			name:     "exists selector matches node",
			labelEnv: "node-role.kubernetes.io/worker",
			lister: &stubNodeLister{
				nodes: []*v1.Node{workerNode},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(openshiftNodeLabelId, tt.labelEnv)
			informer := &stubNodeInformer{lister: tt.lister}
			if got := cp.isOpenShiftCluster(informer); got != tt.want {
				t.Errorf("isOpenShiftCluster() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseOpenShiftLabelSelectorEquals(t *testing.T) {
	selector, err := parseOpenShiftLabelSelector(" role = infra ")
	if err != nil {
		t.Fatalf("parseOpenShiftLabelSelector returned error: %v", err)
	}

	if !selector.Matches(labels.Set{"role": "infra"}) {
		t.Fatalf("expected selector to match node with role=infra")
	}
	if selector.Matches(labels.Set{"role": "worker"}) {
		t.Fatalf("did not expect selector to match node with role=worker")
	}
}

func TestParseOpenShiftLabelSelectorExists(t *testing.T) {
	selector, err := parseOpenShiftLabelSelector("node-role.kubernetes.io/worker")
	if err != nil {
		t.Fatalf("parseOpenShiftLabelSelector returned error: %v", err)
	}

	if !selector.Matches(labels.Set{"node-role.kubernetes.io/worker": ""}) {
		t.Fatalf("expected selector to match node with worker role label")
	}
	if selector.Matches(labels.Set{"node-role.kubernetes.io/infra": ""}) {
		t.Fatalf("did not expect selector to match infra role label")
	}
}

func TestParseOpenShiftLabelSelectorInvalid(t *testing.T) {
	if _, err := parseOpenShiftLabelSelector("openshift.io/cluster="); err == nil {
		t.Fatalf("expected error for invalid label identifier")
	}
	if _, err := parseOpenShiftLabelSelector("="); err == nil {
		t.Fatalf("expected error for missing key and value")
	}
}

func TestEnableFlexCIDRControllerEnv(t *testing.T) {
	logger := zap.NewNop().Sugar()

	t.Run("defaults to enabled", func(t *testing.T) {
		t.Setenv(enableFlexCIDRController, "")
		if got := GetIsFeatureEnabledFromEnv(logger, enableFlexCIDRController, false); got {
			t.Fatalf("GetIsFeatureEnabledFromEnv(%q) = %v, want false", enableFlexCIDRController, got)
		}
	})

	t.Run("can be enabled explicitly", func(t *testing.T) {
		t.Setenv(enableFlexCIDRController, "true")
		if got := GetIsFeatureEnabledFromEnv(logger, enableFlexCIDRController, false); !got {
			t.Fatalf("GetIsFeatureEnabledFromEnv(%q) = %v, want true", enableFlexCIDRController, got)
		}
	})
}
