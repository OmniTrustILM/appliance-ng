package cbom

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

var update = flag.Bool("update", false, "write the golden files")

const testCA = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"

func chart(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "addons", "charts", "cbom-repository-1.2.0.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

const storeImage = "docker.io/versity/versitygw:v1.11.0@sha256:8319a530d08d537426e2f855bae5280709795dc73a3f0c347973ef915d6343c5"

func inputs() Inputs {
	return Inputs{
		ILMHost:         "ilm.example.com",
		RepositoryImage: "hub.omnitrustregistry.com/ilm/cbom-repository:1.2.0@sha256:97e6510e2a024b873b9f9ef25dc1f061cc96f5c5685c91601214ca307d4e8e6b",
	}
}

// renderAll is what the manager applies for spec: the store for appliance storage, then the repository.
func renderAll(t *testing.T, spec *v1alpha1.CBOMSpec, in Inputs) []*unstructured.Unstructured {
	t.Helper()
	var out []*unstructured.Unstructured
	if spec.Storage != v1alpha1.CBOMStorageS3 {
		store, err := StoreObjects(storeImage)
		if err != nil {
			t.Fatal(err)
		}
		out = store
	}
	objs, err := Render(spec, chart(t), in)
	if err != nil {
		t.Fatal(err)
	}
	return append(out, objs...)
}

func applianceStorage() *v1alpha1.CBOMSpec {
	return &v1alpha1.CBOMSpec{Enabled: true, Storage: v1alpha1.CBOMStorageAppliance}
}

func externalStorage() *v1alpha1.CBOMSpec {
	return &v1alpha1.CBOMSpec{
		Enabled: true, Storage: v1alpha1.CBOMStorageS3, CredentialsRef: "appliance-cbom",
		S3: &v1alpha1.S3Target{Endpoint: "https://s3.example.com", Bucket: "cboms", Region: "eu-central-1"},
	}
}

func encode(t *testing.T, objs []*unstructured.Unstructured) []byte {
	t.Helper()
	var out bytes.Buffer
	for _, o := range objs {
		data, err := yaml.Marshal(o.Object)
		if err != nil {
			t.Fatal(err)
		}
		out.WriteString("---\n")
		out.Write(data)
	}
	return out.Bytes()
}

func TestRenderMatchesPinnedChart(t *testing.T) {
	withCA := inputs()
	withCA.CA = testCA
	for name, tc := range map[string]struct {
		spec *v1alpha1.CBOMSpec
		in   Inputs
	}{
		"appliance": {applianceStorage(), inputs()},
		"s3":        {externalStorage(), withCA},
	} {
		t.Run(name, func(t *testing.T) {
			got := encode(t, renderAll(t, tc.spec, tc.in))
			golden := filepath.Join("testdata", name+".golden.yaml")
			if *update {
				if err := os.WriteFile(golden, got, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("rendered objects differ from %s; run go test ./internal/cbom -update and review the diff", golden)
			}
		})
	}
}

func TestRenderPublishesNoRoute(t *testing.T) {
	for name, spec := range map[string]*v1alpha1.CBOMSpec{"appliance": applianceStorage(), "s3": externalStorage()} {
		t.Run(name, func(t *testing.T) {
			objs := renderAll(t, spec, inputs())
			if len(objs) == 0 {
				t.Fatal("nothing rendered")
			}
			for _, o := range objs {
				switch o.GetKind() {
				case "Deployment", "Service", "Secret":
				default:
					t.Errorf("%s %s rendered; only Deployments, Services and Secrets are, nothing is published at the edge", o.GetKind(), o.GetName())
				}
				if o.GetNamespace() != Namespace {
					t.Errorf("%s %s in namespace %q, want %s", o.GetKind(), o.GetName(), o.GetNamespace(), Namespace)
				}
				if o.GetKind() == "Service" {
					if kind, _, _ := unstructured.NestedString(o.Object, "spec", "type"); kind != "" && kind != "ClusterIP" {
						t.Errorf("service %s has type %s", o.GetName(), kind)
					}
				}
				if o.GetKind() == "Secret" && o.GetName() != "trusted-certificates-cbom-repository" {
					t.Errorf("secret %s rendered; the credentials are generated or copied by the manager", o.GetName())
				}
			}
		})
	}
}

func TestRenderSetsTheOriginAndTheStore(t *testing.T) {
	for name, tc := range map[string]struct {
		spec     *v1alpha1.CBOMSpec
		endpoint string
		secret   string
		store    bool
	}{
		"appliance": {applianceStorage(), "http://cbom-s3:7070", StoreSecret, true},
		"s3":        {externalStorage(), "https://s3.example.com", ExternalSecret, false},
	} {
		t.Run(name, func(t *testing.T) {
			text := string(encode(t, renderAll(t, tc.spec, inputs())))
			for _, want := range []string{"https://ilm.example.com", tc.endpoint, "name: " + tc.secret, "@sha256:97e6510e"} {
				if !strings.Contains(text, want) {
					t.Errorf("rendered objects lack %q", want)
				}
			}
			if got := strings.Contains(text, "name: "+StoreName+"\n"); got != tc.store {
				t.Errorf("store rendered %v, want %v", got, tc.store)
			}
		})
	}
}

func TestRenderNothingWhenOff(t *testing.T) {
	off := applianceStorage()
	off.Enabled = false
	for _, spec := range []*v1alpha1.CBOMSpec{nil, off} {
		objs, err := Render(spec, chart(t), inputs())
		if err != nil || len(objs) != 0 {
			t.Errorf("Render(%+v) = %d objects, %v; want none", spec, len(objs), err)
		}
	}
}

func TestRenderRefusesWhatItCannotUse(t *testing.T) {
	bad := inputs()
	bad.RepositoryImage = "cbom-repository"
	if _, err := Render(applianceStorage(), chart(t), bad); err == nil {
		t.Error("an image without a digest rendered")
	}
	if _, err := Render(applianceStorage(), []byte("not a chart"), inputs()); err == nil {
		t.Error("a broken chart rendered")
	}
	if _, err := StoreObjects("versity/versitygw:v1.11.0"); err == nil {
		t.Error("a store image without a digest rendered")
	}
}

func TestWorkloadsCoverEveryRenderedObject(t *testing.T) {
	known := map[string]bool{}
	for _, o := range Workloads() {
		known[o.GetKind()+"/"+o.GetName()] = true
	}
	in := inputs()
	in.CA = testCA
	for _, spec := range []*v1alpha1.CBOMSpec{applianceStorage(), externalStorage()} {
		for _, o := range renderAll(t, spec, in) {
			if !known[o.GetKind()+"/"+o.GetName()] {
				t.Errorf("%s %s is rendered but not among the workloads, so turning off would leave it", o.GetKind(), o.GetName())
			}
		}
	}
}

func TestURLIsTheInClusterService(t *testing.T) {
	if got := URL(); got != "http://cbom-repository-service.ilm.svc:8080" {
		t.Errorf("URL() = %s", got)
	}
}

func TestRenderGivesASchemelessEndpointHTTPS(t *testing.T) {
	spec := externalStorage()
	spec.S3.Endpoint = "s3.example.com:9000"
	text := string(encode(t, renderAll(t, spec, inputs())))
	if !strings.Contains(text, "value: https://s3.example.com:9000\n") {
		t.Errorf("APP_S3_ENDPOINT is not https://s3.example.com:9000:\n%s", text)
	}
}

func TestRenderSetsTheRepositoryResources(t *testing.T) {
	text := string(encode(t, renderAll(t, externalStorage(), inputs())))
	if !strings.Contains(text, "limits:\n            memory: 256Mi") {
		t.Errorf("the repository has no memory limit:\n%s", text)
	}
}

func TestCBOMNetworkPolicy(t *testing.T) {
	const want = `apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: cbom-repository-ingress
  namespace: ilm
spec:
  endpointSelector:
    matchExpressions:
    - key: app.kubernetes.io/name
      operator: In
      values:
      - cbom-repository
      - cbom-s3
  ingress:
  - fromEndpoints:
    - matchLabels:
        k8s:io.kubernetes.pod.namespace: ilm
  - fromEntities:
    - host
`
	got, err := yaml.Marshal(NetworkPolicy().Object)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("policy:\n%s\nwant:\n%s", got, want)
	}
	known := false
	for _, o := range Workloads() {
		known = known || (o.GetKind() == "CiliumNetworkPolicy" && o.GetName() == NetworkPolicy().GetName())
	}
	if !known {
		t.Error("the policy is not among the workloads, so turning off would leave it")
	}
}
