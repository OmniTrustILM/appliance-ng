package talos

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	coreconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// target returns the node a call was addressed to, empty for the node apid runs on.
func target(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("node"); len(v) == 1 {
		return v[0]
	}
	return ""
}

func (f *fakeMachine) EtcdStatus(ctx context.Context, _ *emptypb.Empty) (*machineapi.EtcdStatusResponse, error) {
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	s, ok := f.etcdStatus[target(ctx)]
	if !ok {
		return nil, status.Error(codes.Unavailable, "connection refused")
	}
	return &machineapi.EtcdStatusResponse{Messages: []*machineapi.EtcdStatus{{MemberStatus: s}}}, nil
}

func (f *fakeMachine) EtcdRemoveMemberByID(ctx context.Context, req *machineapi.EtcdRemoveMemberByIDRequest) (*machineapi.EtcdRemoveMemberByIDResponse, error) {
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	f.removed = append(f.removed, req.GetMemberId())
	return &machineapi.EtcdRemoveMemberByIDResponse{}, f.removeErr
}

// addresses resolves the host names of a three-node cluster.
func addresses(_ context.Context, name string) (string, error) {
	addr, ok := map[string]string{"ilm": "10.0.1.11", "ilm2": "10.0.1.12", "ilm3": "10.0.1.13"}[name]
	if !ok {
		return "", fmt.Errorf("no node %s", name)
	}
	return addr, nil
}

func newTestCluster(t *testing.T, fake *fakeMachine) *cluster {
	t.Helper()
	c, err := NewCluster(dialFake(t, fake), addresses)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*cluster)
}

func TestNodeTargetsByAddress(t *testing.T) {
	ctx := context.Background()
	fake := &fakeMachine{}
	c := newTestCluster(t, fake)

	if err := c.Node("ilm2").Reboot(ctx); err != nil {
		t.Fatalf("Reboot of ilm2: %v", err)
	}
	if err := c.Local().Reboot(ctx); err != nil {
		t.Fatalf("Reboot of the local node: %v", err)
	}
	if want := []string{"10.0.1.12", ""}; !slices.Equal(fake.rebootTargets, want) {
		t.Errorf("reboots went to %q, want %q", fake.rebootTargets, want)
	}

	if err := c.Node("missing").Reboot(ctx); err == nil || len(fake.rebootTargets) != 2 {
		t.Errorf("Reboot of an unknown node = %v after %d calls; want an error and no call", err, len(fake.rebootTargets))
	}
	if _, err := NewCluster(fakeNode{}, addresses); err == nil {
		t.Error("NewCluster accepted a node without a Talos API connection")
	}
}

// fakeNode is a Node that is not connected to a Talos API.
type fakeNode struct{ Node }

func TestEtcdMembers(t *testing.T) {
	ctx := context.Background()
	fake := &fakeMachine{
		members: &machineapi.EtcdMemberListResponse{Messages: []*machineapi.EtcdMembers{{Members: []*machineapi.EtcdMember{
			{Id: 1, Hostname: "ilm", PeerUrls: []string{"https://10.0.1.11:2380"}},
			{Id: 2, Hostname: "ilm2", PeerUrls: []string{"https://10.0.1.12:2380"}, IsLearner: true},
			{Id: 3, Hostname: "ilm3", PeerUrls: []string{"https://10.0.1.13:2380"}},
			{Id: 4, Hostname: "ilm4", PeerUrls: []string{"https://10.0.1.14:2380"}},
			{Id: 5, Hostname: "ilm5"},
		}}}},
		etcdStatus: map[string]*machineapi.EtcdMemberStatus{
			"10.0.1.11": {MemberId: 1, Leader: 1},
			"10.0.1.12": {MemberId: 2, Leader: 1, IsLearner: true},
			"10.0.1.13": {MemberId: 3, Leader: 1, Errors: []string{"NOSPACE"}},
			// 10.0.1.14 does not answer; ilm5 has no peer address.
		},
	}
	c := newTestCluster(t, fake)

	got, err := c.EtcdMembers(ctx)
	if err != nil {
		t.Fatalf("EtcdMembers: %v", err)
	}
	want := []EtcdMember{
		{ID: 1, HostName: "ilm", Healthy: true},
		{ID: 2, HostName: "ilm2", Learner: true, Healthy: true},
		{ID: 3, HostName: "ilm3"},
		{ID: 4, HostName: "ilm4"},
		{ID: 5, HostName: "ilm5"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("EtcdMembers = %+v, want %+v", got, want)
	}

	fake.membersErr = status.Error(codes.Unavailable, "etcd is down")
	if _, err := c.EtcdMembers(ctx); err == nil {
		t.Error("a member list error was not reported")
	}
}

func TestEtcdMembersWithoutLeaderAreUnhealthy(t *testing.T) {
	fake := &fakeMachine{
		members: &machineapi.EtcdMemberListResponse{Messages: []*machineapi.EtcdMembers{{Members: []*machineapi.EtcdMember{
			{Id: 1, Hostname: "ilm", PeerUrls: []string{"https://10.0.1.11:2380"}},
		}}}},
		etcdStatus: map[string]*machineapi.EtcdMemberStatus{"10.0.1.11": {MemberId: 1}},
	}
	got, err := newTestCluster(t, fake).EtcdMembers(context.Background())
	if err != nil || len(got) != 1 || got[0].Healthy {
		t.Errorf("EtcdMembers = %+v, %v; want one unhealthy member", got, err)
	}
}

func TestRemoveEtcdMember(t *testing.T) {
	ctx := context.Background()
	fake := &fakeMachine{}
	c := newTestCluster(t, fake)
	if err := c.RemoveEtcdMember(ctx, 0x175560fd668d0203); err != nil {
		t.Fatalf("RemoveEtcdMember: %v", err)
	}
	if want := []uint64{0x175560fd668d0203}; !slices.Equal(fake.removed, want) {
		t.Errorf("removed %x, want %x", fake.removed, want)
	}
	fake.removeErr = status.Error(codes.FailedPrecondition, "no quorum")
	if err := c.RemoveEtcdMember(ctx, 2); err == nil {
		t.Error("a remove error was not reported")
	}
}

func TestResetWipesTheNamedNode(t *testing.T) {
	ctx := context.Background()
	fake := &fakeMachine{}
	c := newTestCluster(t, fake)
	if err := c.Reset(ctx, "ilm3"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if len(fake.resets) != 1 || fake.resetTargets[0] != "10.0.1.13" {
		t.Fatalf("resets %v to %q, want one to 10.0.1.13", fake.resets, fake.resetTargets)
	}
	r := fake.resets[0]
	var wiped []string
	for _, p := range r.GetSystemPartitionsToWipe() {
		wiped = append(wiped, p.GetLabel())
	}
	if r.GetGraceful() || !r.GetReboot() || r.GetMode() != machineapi.ResetRequest_ALL ||
		!slices.Equal(wiped, []string{constants.StatePartitionLabel, constants.EphemeralPartitionLabel}) {
		t.Errorf("reset request %v, want a forced reboot wiping STATE and EPHEMERAL", r)
	}
	fake.resetErr = errors.New("boom")
	if err := c.Reset(ctx, "ilm3"); err == nil {
		t.Error("a reset error was not reported")
	}
	if err := c.Reset(ctx, "missing"); err == nil {
		t.Error("the reset of an unknown node was not refused")
	}
}

func TestSecretsBundleFromConfig(t *testing.T) {
	ctx := context.Background()
	bundle, err := secrets.NewBundle(secrets.NewFixedClock(time.Now()), coreconfig.TalosVersionCurrent)
	if err != nil {
		t.Fatal(err)
	}
	input, err := generate.NewInput("ilm", "https://10.0.1.11:6443", constants.DefaultKubernetesVersion,
		generate.WithSecretsBundle(bundle))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := input.Config(machine.TypeControlPlane)
	if err != nil {
		t.Fatal(err)
	}
	c := newTestCluster(t, &fakeMachine{})
	c.local.st = newState(t)
	if _, err := c.SecretsBundle(ctx); err == nil {
		t.Error("SecretsBundle without a running configuration returned no error")
	}
	if err := c.local.st.Create(ctx, config.NewMachineConfig(cfg)); err != nil {
		t.Fatal(err)
	}

	got, err := c.SecretsBundle(ctx)
	if err != nil {
		t.Fatalf("SecretsBundle: %v", err)
	}
	if got.Cluster.ID != bundle.Cluster.ID || got.TrustdInfo.Token != bundle.TrustdInfo.Token ||
		got.Secrets.BootstrapToken != bundle.Secrets.BootstrapToken ||
		string(got.Certs.Etcd.Crt) != string(bundle.Certs.Etcd.Crt) ||
		string(got.Certs.OS.Key) != string(bundle.Certs.OS.Key) {
		t.Error("the secrets bundle differs from the one the configuration was generated with")
	}
}
