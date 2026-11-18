package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

var testClient client.Client

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		println("KUBEBUILDER_ASSETS is not set; run the tests with make test")
		os.Exit(1)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		println("start envtest:", err.Error())
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		println("add scheme:", err.Error())
		os.Exit(1)
	}
	testClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		println("create client:", err.Error())
		os.Exit(1)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func newAppliance(name string) *v1alpha1.Appliance {
	return &v1alpha1.Appliance{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.ApplianceSpec{Node: v1alpha1.NodeSpec{HostName: "ilm.example.com"}},
	}
}

// create stores the appliance and removes it again when the test ends.
func create(t *testing.T, a *v1alpha1.Appliance) error {
	t.Helper()
	err := testClient.Create(context.Background(), a)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(context.Background(), a) })
	}
	return err
}

func wantInvalid(t *testing.T, err error) {
	t.Helper()
	if !apierrors.IsInvalid(err) {
		t.Fatalf("want an invalid error, got %v", err)
	}
}

func TestApplianceRejectsOtherName(t *testing.T) {
	wantInvalid(t, create(t, newAppliance("other")))
	if err := create(t, newAppliance(v1alpha1.ApplianceName)); err != nil {
		t.Fatalf("the appliance name must be accepted: %v", err)
	}
}

func TestApplianceAddressImmutable(t *testing.T) {
	a := newAppliance(v1alpha1.ApplianceName)
	a.Spec.Node.Address = "10.0.2.15/24"
	if err := create(t, a); err != nil {
		t.Fatal(err)
	}
	a.Spec.Node.Address = "10.0.2.16/24"
	wantInvalid(t, testClient.Update(context.Background(), a))
	a.Spec.Node.Address = ""
	wantInvalid(t, testClient.Update(context.Background(), a))
}

func TestApplianceAddressMayBeSetOnce(t *testing.T) {
	a := newAppliance(v1alpha1.ApplianceName)
	if err := create(t, a); err != nil {
		t.Fatal(err)
	}
	a.Spec.Node.Address = "10.0.2.15/24"
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatalf("setting an empty address must be accepted: %v", err)
	}
	a.Spec.Node.Gateway = "10.0.2.1"
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatalf("an unchanged address must be accepted: %v", err)
	}
}

func TestApplianceTLSSecretNeedsRef(t *testing.T) {
	a := newAppliance(v1alpha1.ApplianceName)
	a.Spec.TLS = v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceSecret}
	wantInvalid(t, create(t, a))
	a.Spec.TLS.SecretRef = "ilm-tls"
	if err := create(t, a); err != nil {
		t.Fatalf("a secret source with a reference must be accepted: %v", err)
	}
}

func TestApplianceHostNameMustBeFQDN(t *testing.T) {
	a := newAppliance(v1alpha1.ApplianceName)
	a.Spec.Node.HostName = "ilm"
	wantInvalid(t, create(t, a))
	a.Spec.Node.HostName = "ilm.example.com"
	if err := create(t, a); err != nil {
		t.Fatalf("an FQDN must be accepted: %v", err)
	}
}

func TestApplianceMailPortRange(t *testing.T) {
	a := newAppliance(v1alpha1.ApplianceName)
	a.Spec.Mail = &v1alpha1.MailSpec{Host: "smtp.example.com", Port: 70000, Security: v1alpha1.MailSecurityTLS, From: "ilm@example.com"}
	wantInvalid(t, create(t, a))
	a.Spec.Mail.Port = 587
	if err := create(t, a); err != nil {
		t.Fatalf("a valid port must be accepted: %v", err)
	}
}

func TestApplianceComponentsMustBeUnique(t *testing.T) {
	a := newAppliance(v1alpha1.ApplianceName)
	a.Spec.Platform = &v1alpha1.PlatformSpec{Components: []string{"software-cryptography-provider", "software-cryptography-provider"}}
	wantInvalid(t, create(t, a))
	a.Spec.Platform.Components = []string{"software-cryptography-provider", "x509-compliance-provider"}
	if err := create(t, a); err != nil {
		t.Fatalf("distinct components must be accepted: %v", err)
	}
}

func TestApplianceStatusSubresource(t *testing.T) {
	a := newAppliance(v1alpha1.ApplianceName)
	if err := create(t, a); err != nil {
		t.Fatal(err)
	}
	a.Status.Setup = v1alpha1.SetupPart1
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatalf("status update: %v", err)
	}
	a.Status.Setup = "bogus"
	wantInvalid(t, testClient.Status().Update(context.Background(), a))
}

func TestApplianceNetworkFormat(t *testing.T) {
	cases := []struct {
		name  string
		set   func(*v1alpha1.Appliance)
		valid bool
	}{
		{"default first boot", func(*v1alpha1.Appliance) {}, true},
		{"IPv4 CIDR", func(a *v1alpha1.Appliance) { a.Spec.Node.Address = "10.0.2.15/24" }, true},
		{"address without prefix", func(a *v1alpha1.Appliance) { a.Spec.Node.Address = "10.0.2.15" }, false},
		{"address octet out of range", func(a *v1alpha1.Appliance) { a.Spec.Node.Address = "10.0.2.300/24" }, false},
		{"IPv6 address", func(a *v1alpha1.Appliance) { a.Spec.Node.Address = "fd00::1/64" }, false},
		{"IPv4 gateway", func(a *v1alpha1.Appliance) { a.Spec.Node.Gateway = "10.0.2.2" }, true},
		{"gateway not an IP", func(a *v1alpha1.Appliance) { a.Spec.Node.Gateway = "gw" }, false},
		{"DNS IP", func(a *v1alpha1.Appliance) { a.Spec.DNS.Servers = []string{"1.1.1.1"} }, true},
		{"DNS host name", func(a *v1alpha1.Appliance) { a.Spec.DNS.Servers = []string{"dns.example"} }, false},
		{"NTP host name", func(a *v1alpha1.Appliance) { a.Spec.NTP.Servers = []string{"pool.ntp.org"} }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newAppliance(v1alpha1.ApplianceName)
			c.set(a)
			err := create(t, a)
			if c.valid && err != nil {
				t.Fatalf("want accepted, got %v", err)
			}
			if !c.valid {
				wantInvalid(t, err)
			}
		})
	}
}
