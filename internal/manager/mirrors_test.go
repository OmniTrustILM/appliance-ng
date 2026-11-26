package manager

import (
	"context"
	"encoding/pem"
	"net"
	"slices"
	"strconv"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/config/types/cri"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/nodeconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/registry"
)

var (
	testRootCA    = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("built-in root")})
	testUpstreams = []string{"docker.io", "ghcr.io"}
	testMirrors   = []string{
		cri.RegistryMirrorConfig + "/docker.io", cri.RegistryMirrorConfig + "/ghcr.io", cri.RegistryTLSConfig + "/" + nodeconfig.MirrorHost,
	}
)

// mirrorReconciler is a node reconciler whose add-on steps end with zot's, which zot passes unless cluster says
// otherwise.
func mirrorReconciler(node *fakeNodeConfig, cluster *fakeCluster) *NodeReconciler {
	r := newNodeReconciler(node)
	r.Cluster, r.Upstreams = cluster, testUpstreams
	r.Steps = append(slices.Clone(testSteps), kube.Step{
		Name: registryStep,
		Wait: []kube.WaitFor{{Kind: "Deployment", Namespace: SystemNamespace, Name: "zot", For: "Available"}},
	})
	return r
}

// issueRootCA writes the leaf Secret with the built-in root CA, as cert-manager does.
func issueRootCA(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: certs.Namespace, Name: certs.SecretName},
		Data:       map[string][]byte{"ca.crt": testRootCA},
	}
	if err := testClient.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, s) })
}

func TestNoMirrorsBeforeZot(t *testing.T) {
	createAppliance(t, importedSpec())
	createNode(t, testBootID)
	node := &fakeNodeConfig{docs: firstBootDocuments(t, importedSpec())}

	// zot is not ready, and when it is the built-in CA is not issued yet: the settings are looked at again soon.
	for _, cluster := range []*fakeCluster{{unready: "zot"}, {}} {
		res, err := reconcileNode(t, mirrorReconciler(node, cluster))
		if err != nil || res.RequeueAfter != registryRequeue {
			t.Fatalf("reconcile while the mirrors wait = %+v, %v; want a requeue after %s", res, err, registryRequeue)
		}
	}
	if node.calls() != 0 {
		t.Fatalf("replaced %v before zot is ready, want nothing", node.replaced)
	}
	wantNodeConfigured(t, metav1.ConditionTrue, reasonApplied, false)

	// Without the registry step nothing tells that zot is ready.
	issueRootCA(t)
	r := mirrorReconciler(node, &fakeCluster{})
	r.Steps = testSteps
	if res, err := reconcileNode(t, r); err != nil || res.RequeueAfter != resync {
		t.Fatalf("reconcile without a registry step = %+v, %v; want the periodic pass", res, err)
	}
	if node.calls() != 0 {
		t.Fatalf("replaced %v without a registry step, want nothing", node.replaced)
	}
}

func TestMirrorTrustsBuiltInCA(t *testing.T) {
	createAppliance(t, importedSpec())
	createNode(t, testBootID)
	issueRootCA(t)
	node := &fakeNodeConfig{docs: firstBootDocuments(t, importedSpec())}
	cluster := &fakeCluster{}
	r := mirrorReconciler(node, cluster)

	if res, err := reconcileNode(t, r); err != nil || res.RequeueAfter != resync {
		t.Fatalf("reconcile once zot is ready = %+v, %v; want the periodic pass", res, err)
	}
	if replaced, removed := node.lastCall(); !slices.Equal(replaced, testMirrors) || len(removed) != 0 {
		t.Fatalf("replaced %v and removed %v, want the mirrors %v", replaced, removed, testMirrors)
	}
	tls := node.document(t, cri.RegistryTLSConfig+"/"+nodeconfig.MirrorHost).(*cri.RegistryTLSConfigV1Alpha1)
	if tls.TLSCA != string(testRootCA) {
		t.Errorf("zot's TLS trusts %q, want the built-in root CA", tls.TLSCA)
	}
	// The mirrors take effect without a reboot.
	wantNodeConfigured(t, metav1.ConditionTrue, reasonApplied, false)

	// A zot that restarts keeps the mirrors: a pull it cannot serve falls back to the upstream registry.
	cluster.unready = "zot"
	mustReconcileNode(t, r)
	if node.calls() != 1 {
		t.Errorf("%d changes while zot restarts, want the mirrors kept", node.calls())
	}

	// So does a node whose built-in CA cannot be read: the mirrors keep the CA they trust.
	leaf := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: certs.Namespace, Name: certs.SecretName}}
	if err := testClient.Delete(context.Background(), leaf); err != nil {
		t.Fatal(err)
	}
	mustReconcileNode(t, r)
	if node.calls() != 1 {
		t.Errorf("%d changes without the built-in CA, want the mirrors kept", node.calls())
	}
}

func TestMirrorHostIsZot(t *testing.T) {
	if want := net.JoinHostPort("127.0.0.1", strconv.Itoa(registry.Port)); nodeconfig.MirrorHost != want {
		t.Errorf("MirrorHost = %s, want zot at %s", nodeconfig.MirrorHost, want)
	}
}
