package kube

import (
	"testing"
)

const staleKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: ilm
  cluster:
    server: https://192.0.2.10:6443
    insecure-skip-tls-verify: true
users:
- name: admin
  user:
    token: test-token
contexts:
- name: admin
  context:
    cluster: ilm
    user: admin
current-context: admin
`

func TestLocalRESTConfigOverridesHostOnly(t *testing.T) {
	cfg, err := localRESTConfig([]byte(staleKubeconfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != KubePrismEndpoint {
		t.Errorf("host = %q, want %q", cfg.Host, KubePrismEndpoint)
	}
	if cfg.BearerToken != "test-token" || !cfg.Insecure {
		t.Errorf("credentials or TLS settings were not kept: %+v", cfg)
	}
}

func TestLocalRESTConfigRejectsInvalidKubeconfig(t *testing.T) {
	if _, err := localRESTConfig([]byte("not a kubeconfig")); err == nil {
		t.Fatal("expected an error")
	}
}
