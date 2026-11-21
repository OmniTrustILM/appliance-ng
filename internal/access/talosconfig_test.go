package access

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/siderolabs/crypto/x509"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

// fakeNode answers ClientConfig with a talosconfig whose endpoint is not the node address.
type fakeNode struct {
	talos.Node
	roles []string
	ttl   time.Duration
	data  []byte
	err   error
}

func (f *fakeNode) ClientConfig(_ context.Context, roles []string, ttl time.Duration) ([]byte, error) {
	f.roles, f.ttl = roles, ttl
	return f.data, f.err
}

func generated(t *testing.T) []byte {
	t.Helper()
	cfg := clientconfig.NewConfig("appliance", []string{"10.255.0.1"}, []byte("ca"), &x509.PEMEncodedCertificateAndKey{Crt: []byte("crt"), Key: []byte("key")})
	data, err := cfg.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestTalosconfigPointsAtTheNode(t *testing.T) {
	node := &fakeNode{data: generated(t)}
	data, err := Talosconfig(context.Background(), node, "192.0.2.10", RoleOSReader, 8*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(node.roles, []string{RoleOSReader}) || node.ttl != 8*time.Hour {
		t.Errorf("roles %v and TTL %v", node.roles, node.ttl)
	}
	cfg, err := clientconfig.FromBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	current := cfg.Contexts[cfg.Context]
	if !slices.Equal(current.Endpoints, []string{"192.0.2.10"}) || !slices.Equal(current.Nodes, []string{"192.0.2.10"}) {
		t.Errorf("endpoints %v and nodes %v", current.Endpoints, current.Nodes)
	}
	if current.Crt == "" || current.Key == "" {
		t.Error("the client certificate is lost")
	}
}

func TestTalosconfigRefusesBadRequests(t *testing.T) {
	for _, tc := range []struct {
		role  string
		ttl   time.Duration
		field string
	}{
		{"os:operator", time.Hour, FieldRole},
		{RoleOSAdmin, 0, FieldHours},
		{RoleOSAdmin, 9 * time.Hour, FieldHours},
	} {
		_, err := Talosconfig(context.Background(), &fakeNode{data: generated(t)}, "192.0.2.10", tc.role, tc.ttl)
		var invalid *InvalidError
		if !errors.As(err, &invalid) || invalid.Field != tc.field {
			t.Errorf("%s for %v: %v, want an invalid %s", tc.role, tc.ttl, err, tc.field)
		}
	}
}

func TestTalosconfigErrors(t *testing.T) {
	if _, err := Talosconfig(context.Background(), &fakeNode{err: errors.New("unavailable")}, "192.0.2.10", RoleOSAdmin, time.Hour); err == nil {
		t.Error("a node error: expected error")
	}
	_, err := Talosconfig(context.Background(), &fakeNode{data: []byte("key: [-----BEGIN PRIVATE")}, "192.0.2.10", RoleOSAdmin, time.Hour)
	if err == nil || err.Error() != "the node returned an invalid talosconfig" {
		t.Errorf("an unparsable talosconfig: %v", err)
	}
}

func TestRecordTalosconfigPrunesExpired(t *testing.T) {
	resetAccess(t)
	ctx := context.Background()
	defer func(f func() time.Time) { now = f }(now)
	clock := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }

	if err := RecordTalosconfig(ctx, testClient, RoleOSAdmin, clock.Add(time.Hour), "admin"); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Hour)
	if creds, err := List(ctx, testClient); err != nil || len(creds) != 0 {
		t.Errorf("list after expiry: %+v, %v", creds, err)
	}
	if err := RecordTalosconfig(ctx, testClient, RoleOSReader, clock.Add(time.Hour), "admin"); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: TalosconfigRecords}, cm); err != nil {
		t.Fatal(err)
	}
	records, err := decodeRecords(cm)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Role != RoleOSReader {
		t.Errorf("records %+v, want the os:reader one only", records)
	}

	clock = clock.Add(2 * time.Hour)
	if _, err := RemoveExpired(ctx, testClient, clock); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: TalosconfigRecords}, cm); err != nil {
		t.Fatal(err)
	}
	if records, _ := decodeRecords(cm); len(records) != 0 {
		t.Errorf("records after the expiry pass: %+v", records)
	}
}

func TestRecordsRefuseACorruptConfigMap(t *testing.T) {
	resetAccess(t)
	ctx := context.Background()
	cm := &corev1.ConfigMap{}
	cm.Namespace, cm.Name = Namespace, TalosconfigRecords
	cm.Data = map[string]string{recordsKey: "not json"}
	if err := testClient.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if _, err := List(ctx, testClient); !errors.Is(err, ErrCorruptRecords) || strings.Contains(err.Error(), "not json") {
		t.Errorf("List: %v, want ErrCorruptRecords without the content", err)
	}
	if err := RecordTalosconfig(ctx, testClient, RoleOSAdmin, time.Now().Add(time.Hour), "admin"); err == nil {
		t.Error("RecordTalosconfig: expected error")
	}
}

// racingClient records a talosconfig between its first read of the records and the create that follows, as a
// concurrent issuer would.
type racingClient struct {
	client.Client
	raced *bool
}

func (c racingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key.Name == TalosconfigRecords && !*c.raced {
		*c.raced = true
		if err := RecordTalosconfig(ctx, c.Client, RoleOSReader, time.Now().Add(time.Hour), "other"); err != nil {
			return err
		}
		return apierrors.NewNotFound(corev1.Resource("configmaps"), key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestRecordTalosconfigRetriesAfterAConcurrentCreate(t *testing.T) {
	resetAccess(t)
	ctx := context.Background()
	if err := RecordTalosconfig(ctx, racingClient{testClient, new(bool)}, RoleOSAdmin, time.Now().Add(time.Hour), "admin"); err != nil {
		t.Fatal(err)
	}
	creds, err := List(ctx, testClient)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 2 {
		t.Errorf("recorded %+v, want both talosconfigs", creds)
	}
}
