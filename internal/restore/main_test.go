package restore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

var testClient client.WithWatch

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		println("KUBEBUILDER_ASSETS is not set; run the tests with make test")
		return 1
	}
	crds := []*apiextensionsv1.CustomResourceDefinition{
		openCRD("postgresql.cnpg.io", "v1", "Cluster", "clusters"),
		openCRD("k8s.keycloak.org", "v2beta1", "Keycloak", "keycloaks"),
	}
	for _, kind := range []string{"Connector", "Platform"} {
		crd, err := operatorCRD(filepath.Join("..", "..", "addons", "ilm-operator.yaml"), kind)
		if err != nil {
			println("read the", kind, "CRD:", err.Error())
			return 1
		}
		crds = append(crds, crd)
	}
	env := &envtest.Environment{
		CRDs: crds,
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd"), filepath.Join("..", "..", "addons", "cert-manager.yaml"),
		},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		println("start envtest:", err.Error())
		return 1
	}
	defer func() { _ = env.Stop() }()
	cfg.QPS, cfg.Burst = 1000, 1000
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, v1alpha1.AddToScheme, otilm.AddToScheme} {
		if err := add(scheme); err != nil {
			println(err.Error())
			return 1
		}
	}
	if testClient, err = client.NewWithWatch(cfg, client.Options{Scheme: scheme}); err != nil {
		println("create client:", err.Error())
		return 1
	}
	for _, ns := range []string{"appliance-system", "ilm", "cert-manager", "ilm-operator-system"} {
		if err := testClient.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			println(err.Error())
			return 1
		}
	}
	return m.Run()
}

// operatorCRD returns the CRD of kind in the operator manifest.
func operatorCRD(manifest, kind string) (*apiextensionsv1.CustomResourceDefinition, error) {
	data, err := os.ReadFile(manifest)
	if err != nil {
		return nil, err
	}
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := decoder.Decode(crd); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("no " + kind + " CRD in the manifest")
			}
			return nil, err
		}
		if crd.Kind == "CustomResourceDefinition" && crd.Spec.Names.Kind == kind {
			return crd, nil
		}
	}
}

// openCRD is a namespaced kind with an open schema.
func openCRD(group, version, kind, plural string) *apiextensionsv1.CustomResourceDefinition {
	open := true
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + "." + group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: kind, ListKind: kind + "List", Plural: plural},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: &open,
				}},
			}},
		},
	}
}
