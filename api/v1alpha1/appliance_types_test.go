package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
		Spec: v1alpha1.ApplianceSpec{Cluster: v1alpha1.ClusterSpec{
			Nodes: []v1alpha1.ClusterNode{{HostName: "ilm.example.com", Role: v1alpha1.NodeRoleVoting}},
		}},
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

func TestNodeEntryImmutable(t *testing.T) {
	ctx := context.Background()
	a := newAppliance(v1alpha1.ApplianceName)
	if err := create(t, a); err != nil {
		t.Fatal(err)
	}
	a.Spec.FirstNode().Address = "10.0.2.15/24"
	if err := testClient.Update(ctx, a); err != nil {
		t.Fatalf("setting an address once must be accepted: %v", err)
	}
	a.Spec.FirstNode().Gateway = "10.0.2.1"
	a.Spec.FirstNode().Tags = []string{"hsm"}
	a.Spec.FirstNode().Maintenance = true
	if err := testClient.Update(ctx, a); err != nil {
		t.Fatalf("an unchanged address must be accepted: %v", err)
	}
	a.Spec.Cluster.Nodes = append(a.Spec.Cluster.Nodes, v1alpha1.ClusterNode{HostName: "node2.example.com", Address: "10.0.2.16/24", Role: v1alpha1.NodeRoleVoting})
	if err := testClient.Update(ctx, a); err != nil {
		t.Fatalf("a second node must be accepted: %v", err)
	}
	for _, address := range []string{"10.0.2.17/24", ""} {
		a.Spec.NodeNamed("node2.example.com").Address = address
		wantInvalid(t, testClient.Update(ctx, a))
	}
	a.Spec.NodeNamed("node2.example.com").Address = "10.0.2.16/24"
	a.Spec.FirstNode().Address = "10.0.2.18/24"
	wantInvalid(t, testClient.Update(ctx, a))
}

func TestNodeEntryHostNamesUnique(t *testing.T) {
	a := newAppliance(v1alpha1.ApplianceName)
	a.Spec.Cluster.Nodes = append(a.Spec.Cluster.Nodes, *a.Spec.FirstNode())
	wantInvalid(t, create(t, a))
}

func TestNodeEntryFirstLabelsUnique(t *testing.T) {
	a := newAppliance(v1alpha1.ApplianceName)
	a.Spec.Cluster.Nodes = []v1alpha1.ClusterNode{{HostName: "ilm.site-a.example"}, {HostName: "ilm.site-b.example"}}
	wantInvalid(t, create(t, a))
	a = newAppliance(v1alpha1.ApplianceName)
	a.Spec.Cluster.Nodes = []v1alpha1.ClusterNode{{HostName: "ilm1.site-a.example"}, {HostName: "ilm2.site-a.example"}}
	if err := create(t, a); err != nil {
		t.Fatalf("different first labels must be accepted: %v", err)
	}
}

func TestClusterAddressOneOf(t *testing.T) {
	for _, tc := range []struct {
		name    string
		address v1alpha1.ClusterAddress
		ok      bool
	}{
		{name: "none", ok: true},
		{name: "virtual", address: v1alpha1.ClusterAddress{Virtual: "10.0.2.100"}, ok: true},
		{name: "external", address: v1alpha1.ClusterAddress{External: "ilm.example.com"}, ok: true},
		{name: "both", address: v1alpha1.ClusterAddress{Virtual: "10.0.2.100", External: "ilm.example.com"}},
		{name: "virtual CIDR", address: v1alpha1.ClusterAddress{Virtual: "10.0.2.100/24"}},
		{name: "virtual IPv6", address: v1alpha1.ClusterAddress{Virtual: "fd00::1"}},
		{name: "external not a name", address: v1alpha1.ClusterAddress{External: "ilm_example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAppliance(v1alpha1.ApplianceName)
			a.Spec.Cluster.Address = tc.address
			err := create(t, a)
			if tc.ok && err != nil || !tc.ok && !apierrors.IsInvalid(err) {
				t.Errorf("create: %v, want accepted %v", err, tc.ok)
			}
		})
	}
}

func TestTagPattern(t *testing.T) {
	for tag, ok := range map[string]bool{
		"hsm": true, "site-a": true, "0": true, strings.Repeat("a", 31): true,
		strings.Repeat("a", 32): false, "HSM": false, "-hsm": false, "site.a": false,
	} {
		t.Run("node "+tag, func(t *testing.T) {
			a := newAppliance(v1alpha1.ApplianceName)
			a.Spec.FirstNode().Tags = []string{tag}
			if err := create(t, a); ok && err != nil || !ok && !apierrors.IsInvalid(err) {
				t.Errorf("create: %v, want accepted %v", err, ok)
			}
		})
		t.Run("component "+tag, func(t *testing.T) {
			a := newAppliance(v1alpha1.ApplianceName)
			a.Spec.Platform = &v1alpha1.PlatformSpec{Components: []v1alpha1.ComponentSpec{{Name: "software-cryptography-provider", NodeTag: tag}}}
			if err := create(t, a); ok && err != nil || !ok && !apierrors.IsInvalid(err) {
				t.Errorf("create: %v, want accepted %v", err, ok)
			}
		})
	}
	for _, tc := range []struct {
		name string
		tags []string
		ok   bool
	}{
		{name: "eight", tags: []string{"a", "b", "c", "d", "e", "f", "g", "h"}, ok: true},
		{name: "nine", tags: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}},
		{name: "duplicate", tags: []string{"hsm", "hsm"}},
		{name: "empty", tags: []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAppliance(v1alpha1.ApplianceName)
			a.Spec.FirstNode().Tags = tc.tags
			if err := create(t, a); tc.ok && err != nil || !tc.ok && !apierrors.IsInvalid(err) {
				t.Errorf("create: %v, want accepted %v", err, tc.ok)
			}
		})
	}
}

func TestFirstAndNamedNode(t *testing.T) {
	var spec v1alpha1.ApplianceSpec
	if spec.FirstNode() != nil || spec.NodeNamed("a.example.com") != nil || spec.HostName() != "" {
		t.Fatal("an Appliance without nodes has no first or named node")
	}
	spec.Cluster.Nodes = []v1alpha1.ClusterNode{{HostName: "a.example.com"}, {HostName: "b.example.com"}}
	if n := spec.FirstNode(); n != &spec.Cluster.Nodes[0] || spec.HostName() != "a.example.com" {
		t.Errorf("first node %+v", n)
	}
	if n := spec.NodeNamed("b.example.com"); n != &spec.Cluster.Nodes[1] {
		t.Errorf("named node %+v", n)
	}
	if spec.NodeNamed("c.example.com") != nil {
		t.Error("an unknown host name names a node")
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
	a.Spec.FirstNode().HostName = "ilm"
	wantInvalid(t, create(t, a))
	a.Spec.FirstNode().HostName = strings.Repeat("a", 64) + ".example.com"
	wantInvalid(t, create(t, a))
	a.Spec.FirstNode().HostName = strings.Repeat("a", 63) + ".example.com"
	if err := create(t, a); err != nil {
		t.Fatalf("an FQDN with 63-character labels must be accepted: %v", err)
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
	a.Spec.Platform = &v1alpha1.PlatformSpec{Components: []v1alpha1.ComponentSpec{{Name: "software-cryptography-provider"}, {Name: "software-cryptography-provider"}}}
	wantInvalid(t, create(t, a))
	a.Spec.Platform.Components = []v1alpha1.ComponentSpec{{Name: "software-cryptography-provider"}, {Name: "x509-compliance-provider"}}
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
		{"IPv4 CIDR", func(a *v1alpha1.Appliance) { a.Spec.FirstNode().Address = "10.0.2.15/24" }, true},
		{"address without prefix", func(a *v1alpha1.Appliance) { a.Spec.FirstNode().Address = "10.0.2.15" }, false},
		{"address octet out of range", func(a *v1alpha1.Appliance) { a.Spec.FirstNode().Address = "10.0.2.300/24" }, false},
		{"IPv6 address", func(a *v1alpha1.Appliance) { a.Spec.FirstNode().Address = "fd00::1/64" }, false},
		{"IPv4 gateway", func(a *v1alpha1.Appliance) { a.Spec.FirstNode().Gateway = "10.0.2.2" }, true},
		{"gateway not an IP", func(a *v1alpha1.Appliance) { a.Spec.FirstNode().Gateway = "gw" }, false},
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

func s3Target(name string) v1alpha1.BackupTarget {
	return v1alpha1.BackupTarget{
		Name: name, Type: v1alpha1.BackupTypeS3, Retention: 14, CredentialsRef: "backup-" + name,
		S3: &v1alpha1.S3Target{Endpoint: "https://s3.example.com", Bucket: "ilm"},
	}
}

func sftpTarget(name string) v1alpha1.BackupTarget {
	return v1alpha1.BackupTarget{
		Name: name, Type: v1alpha1.BackupTypeSFTP, Retention: 14, CredentialsRef: "backup-" + name,
		SFTP: &v1alpha1.SFTPTarget{Host: "sftp.example.com", Port: 22, Path: "/backups", User: "ilm", HostKey: "SHA256:abc"},
	}
}

func nfsTarget(name string) v1alpha1.BackupTarget {
	return v1alpha1.BackupTarget{
		Name: name, Type: v1alpha1.BackupTypeNFS, Retention: 14,
		NFS: &v1alpha1.NFSTarget{Server: "nfs.example.com", Export: "/export", Version: "4.1"},
	}
}

func applianceWithTargets(targets ...v1alpha1.BackupTarget) *v1alpha1.Appliance {
	a := newAppliance(v1alpha1.ApplianceName)
	a.Spec.Backup = &v1alpha1.BackupSpec{Targets: targets}
	return a
}

func TestBackupValidTargetsAreAccepted(t *testing.T) {
	a := applianceWithTargets(s3Target("s3"), sftpTarget("sftp"), nfsTarget("nfs"))
	a.Spec.Backup.Targets[0].Schedule = "0 2 * * *"
	a.Spec.Backup.Targets[0].TimeZone = "Europe/Prague"
	if err := create(t, a); err != nil {
		t.Fatalf("valid targets must be accepted: %v", err)
	}
}

func TestBackupTargetBlockMatchesType(t *testing.T) {
	cases := map[string]func(*v1alpha1.BackupTarget){
		"block of another type":   func(b *v1alpha1.BackupTarget) { b.SFTP = sftpTarget("x").SFTP },
		"block of its type unset": func(b *v1alpha1.BackupTarget) { b.S3 = nil },
		"unknown type":            func(b *v1alpha1.BackupTarget) { b.Type = "ftp" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			target := s3Target("s3")
			mutate(&target)
			wantInvalid(t, create(t, applianceWithTargets(target)))
		})
	}
}

func TestBackupTargetNamesUnique(t *testing.T) {
	wantInvalid(t, create(t, applianceWithTargets(s3Target("same"), sftpTarget("same"))))
}

func TestBackupTargetNameIsALabel(t *testing.T) {
	for _, name := range []string{"Upper", "under_score", "-lead", "trail-", strings.Repeat("a", 64)} {
		wantInvalid(t, create(t, applianceWithTargets(s3Target(name))))
	}
}

func TestBackupRetentionRange(t *testing.T) {
	for _, retention := range []int32{0, 366} {
		target := s3Target("s3")
		target.Retention = retention
		wantInvalid(t, create(t, applianceWithTargets(target)))
	}
	for _, retention := range []int32{1, 365} {
		target := s3Target("s3")
		target.Retention = retention
		if err := create(t, applianceWithTargets(target)); err != nil {
			t.Fatalf("retention %d must be accepted: %v", retention, err)
		}
		_ = testClient.Delete(context.Background(), newAppliance(v1alpha1.ApplianceName))
	}
}

func TestBackupScheduleHasFiveFields(t *testing.T) {
	for _, schedule := range []string{"0 2 * *", "0 2 * * * *", "daily"} {
		target := s3Target("s3")
		target.Schedule = schedule
		wantInvalid(t, create(t, applianceWithTargets(target)))
	}
}

func TestSFTPNeedsHostKey(t *testing.T) {
	for _, hostKey := range []string{"", "MD5:abc", "abc"} {
		target := sftpTarget("sftp")
		target.SFTP.HostKey = hostKey
		wantInvalid(t, create(t, applianceWithTargets(target)))
	}
}

func TestSFTPPortRange(t *testing.T) {
	for _, port := range []int32{0, 65536} {
		target := sftpTarget("sftp")
		target.SFTP.Port = port
		wantInvalid(t, create(t, applianceWithTargets(target)))
	}
}

func TestNFSVersion(t *testing.T) {
	target := nfsTarget("nfs")
	target.NFS.Version = "2"
	wantInvalid(t, create(t, applianceWithTargets(target)))
}

func TestExternalDependencyTLSNeedsCA(t *testing.T) {
	ca := &v1alpha1.SecretKeyRef{Name: "appliance-database", Key: v1alpha1.CAKey}
	for _, tc := range []struct {
		name string
		db   *v1alpha1.ExternalDatabase
		mq   *v1alpha1.ExternalMessaging
		ok   bool
	}{
		{name: "default", db: &v1alpha1.ExternalDatabase{}, ok: true},
		{name: "require", db: &v1alpha1.ExternalDatabase{TLSMode: v1alpha1.DatabaseTLSRequire}, ok: true},
		{name: "verify-full with CA", db: &v1alpha1.ExternalDatabase{TLSMode: v1alpha1.DatabaseTLSVerifyFull, CA: ca}, ok: true},
		{name: "verify-full without CA", db: &v1alpha1.ExternalDatabase{TLSMode: v1alpha1.DatabaseTLSVerifyFull}},
		{name: "unknown mode", db: &v1alpha1.ExternalDatabase{TLSMode: "prefer"}},
		{name: "plain broker", mq: &v1alpha1.ExternalMessaging{}, ok: true},
		{name: "TLS broker with CA", mq: &v1alpha1.ExternalMessaging{TLS: true, CA: ca}, ok: true},
		{name: "TLS broker without CA", mq: &v1alpha1.ExternalMessaging{TLS: true}},
		{name: "CA without TLS", mq: &v1alpha1.ExternalMessaging{CA: ca}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAppliance(v1alpha1.ApplianceName)
			a.Spec.Platform = &v1alpha1.PlatformSpec{}
			if tc.db != nil {
				tc.db.Host, tc.db.Port, tc.db.Name, tc.db.CredentialsRef = "db.example.com", 5432, "ilm", "appliance-database"
				a.Spec.Platform.Database = tc.db
			}
			if tc.mq != nil {
				tc.mq.Host, tc.mq.Port, tc.mq.CredentialsRef = "mq.example.com", 5671, "appliance-messaging"
				a.Spec.Platform.Messaging = tc.mq
			}
			err := create(t, a)
			if tc.ok && err != nil || !tc.ok && !apierrors.IsInvalid(err) {
				t.Errorf("create: %v, want accepted %v", err, tc.ok)
			}
		})
	}
}

func TestExternalDependencyHostAndName(t *testing.T) {
	for name, db := range map[string]v1alpha1.ExternalDatabase{
		"IPv6 host":              {Host: "[::1]", Name: "ilm"},
		"JDBC parameter in name": {Host: "db.example.com", Name: "ilm?socketFactory=x"},
	} {
		t.Run(name, func(t *testing.T) {
			a := newAppliance(v1alpha1.ApplianceName)
			db.Port, db.CredentialsRef = 5432, "appliance-database"
			a.Spec.Platform = &v1alpha1.PlatformSpec{Database: &db}
			wantInvalid(t, create(t, a))
		})
	}
}

func TestCBOMS3SetExactlyForS3Storage(t *testing.T) {
	bucket := &v1alpha1.S3Target{Endpoint: "https://s3.example.com", Bucket: "cboms"}
	for _, tc := range []struct {
		name string
		cbom v1alpha1.CBOMSpec
		ok   bool
	}{
		{name: "appliance", cbom: v1alpha1.CBOMSpec{Enabled: true, Storage: v1alpha1.CBOMStorageAppliance}, ok: true},
		{name: "s3", cbom: v1alpha1.CBOMSpec{Enabled: true, Storage: v1alpha1.CBOMStorageS3, S3: bucket, CredentialsRef: "appliance-cbom"}, ok: true},
		{name: "s3 without bucket", cbom: v1alpha1.CBOMSpec{Storage: v1alpha1.CBOMStorageS3, CredentialsRef: "appliance-cbom"}},
		{name: "s3 without credentials", cbom: v1alpha1.CBOMSpec{Storage: v1alpha1.CBOMStorageS3, S3: bucket}},
		{name: "bucket with appliance storage", cbom: v1alpha1.CBOMSpec{Storage: v1alpha1.CBOMStorageAppliance, S3: bucket}},
		{name: "credentials with appliance storage", cbom: v1alpha1.CBOMSpec{Storage: v1alpha1.CBOMStorageAppliance, CredentialsRef: "appliance-cbom"}},
		{name: "unknown storage", cbom: v1alpha1.CBOMSpec{Storage: "nfs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAppliance(v1alpha1.ApplianceName)
			a.Spec.Platform = &v1alpha1.PlatformSpec{CBOM: &tc.cbom}
			err := create(t, a)
			if tc.ok && err != nil || !tc.ok && !apierrors.IsInvalid(err) {
				t.Errorf("create: %v, want accepted %v", err, tc.ok)
			}
		})
	}
}

func TestUpdateStatusRoundTrip(t *testing.T) {
	ctx := context.Background()
	a := newAppliance(v1alpha1.ApplianceName)
	if err := create(t, a); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	a.Status.Versions.PlatformBundle = "1.2.0"
	a.Status.Updates = &v1alpha1.UpdatesStatus{
		LastCheck:      &now,
		LastCheckError: "unreachable",
		Available: []v1alpha1.BundleInfo{{
			Kind: "platform", Version: "1.3.0", Arch: "amd64", ILM: "2.18.0", Operator: "2.4.0", MinAppliance: "1.2.0",
			SizeBytes: 1 << 30, Blocked: "needsAppliance", Needs: "1.2.0",
		}},
		Loaded: []v1alpha1.BundleInfo{{Kind: "system", Version: "1.2.0", Arch: "amd64", OperatorRange: ">=2.0.0 <3.0.0", LoadedAt: &now}},
	}
	a.Status.Update = &v1alpha1.UpdateStatus{
		Kind: "system", From: "1.2.0", To: "1.3.0", Step: "pushing", Message: "Loading the images", Backup: "pre-update",
		StartedAt: now, PointOfNoReturn: true,
	}
	if err := testClient.Status().Update(ctx, a); err != nil {
		t.Fatalf("status update: %v", err)
	}
	var got v1alpha1.Appliance
	if err := testClient.Get(ctx, client.ObjectKeyFromObject(a), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Versions.PlatformBundle != "1.2.0" || got.Status.Updates == nil || got.Status.Update == nil {
		t.Fatalf("status lost: %+v", got.Status)
	}
	u := got.Status.Updates
	if u.LastCheckError != "unreachable" || len(u.Available) != 1 || u.Available[0].Needs != "1.2.0" ||
		u.Available[0].SizeBytes != 1<<30 || len(u.Loaded) != 1 || u.Loaded[0].LoadedAt == nil {
		t.Fatalf("updates lost: %+v", u)
	}
	if s := got.Status.Update; s.To != "1.3.0" || s.Backup != "pre-update" || !s.PointOfNoReturn || s.StartedAt.IsZero() {
		t.Fatalf("update lost: %+v", s)
	}
}

func TestComponentNames(t *testing.T) {
	p := v1alpha1.PlatformSpec{Components: []v1alpha1.ComponentSpec{{Name: "b"}, {Name: "a", NodeTag: "hsm"}}}
	if got := p.ComponentNames(); !slices.Equal(got, []string{"b", "a"}) {
		t.Errorf("ComponentNames() = %v", got)
	}
}
