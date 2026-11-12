package talos

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/inmem"
	"github.com/cosi-project/runtime/pkg/state/impl/namespaced"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	"github.com/siderolabs/talos/pkg/machinery/config/types/v1alpha1"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
	"github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

const kubeconfigBody = "apiVersion: v1\nkind: Config\n"

type fakeMachine struct {
	machineapi.UnimplementedMachineServiceServer

	applied      []byte
	applyMode    machineapi.ApplyConfigurationRequest_Mode
	bootstrapErr error
	members      *machineapi.EtcdMemberListResponse
	membersErr   error
}

func authorize(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("talos-role"); len(v) != 1 || v[0] != "os:admin" {
		return status.Error(codes.PermissionDenied, "not authorized")
	}
	return nil
}

func (f *fakeMachine) ApplyConfiguration(ctx context.Context, req *machineapi.ApplyConfigurationRequest) (*machineapi.ApplyConfigurationResponse, error) {
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	f.applied, f.applyMode = req.GetData(), req.GetMode()
	return &machineapi.ApplyConfigurationResponse{}, nil
}

func (f *fakeMachine) Bootstrap(ctx context.Context, _ *machineapi.BootstrapRequest) (*machineapi.BootstrapResponse, error) {
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	return &machineapi.BootstrapResponse{}, f.bootstrapErr
}

func (f *fakeMachine) EtcdMemberList(ctx context.Context, _ *machineapi.EtcdMemberListRequest) (*machineapi.EtcdMemberListResponse, error) {
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	return f.members, f.membersErr
}

func (f *fakeMachine) Kubeconfig(_ *emptypb.Empty, stream grpc.ServerStreamingServer[common.Data]) error {
	if err := authorize(stream.Context()); err != nil {
		return err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "kubeconfig", Mode: 0o600, Size: int64(len(kubeconfigBody))})
	_, _ = tw.Write([]byte(kubeconfigBody))
	_ = tw.Close()
	_ = gz.Close()
	return stream.Send(&common.Data{Bytes: buf.Bytes()})
}

// dialFake starts the fake service on a Unix socket and returns a Node connected to it.
func dialFake(t *testing.T, fake *fakeMachine) *node {
	t.Helper()
	// Unix socket paths are limited to about 100 bytes, which t.TempDir() exceeds on macOS for subtests.
	dir, err := os.MkdirTemp("", "ts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "m.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	machineapi.RegisterMachineServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	n, err := Dial(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.(*node).Close() })
	return n.(*node)
}

func TestDialSendsRoleMetadata(t *testing.T) {
	fake := &fakeMachine{}
	n := dialFake(t, fake)
	ctx := context.Background()

	if err := n.ApplyConfig(ctx, []byte("cfg")); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if string(fake.applied) != "cfg" || fake.applyMode != machineapi.ApplyConfigurationRequest_AUTO {
		t.Fatalf("applied %q in mode %v", fake.applied, fake.applyMode)
	}
	if err := n.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	kc, err := n.Kubeconfig(ctx)
	if err != nil {
		t.Fatalf("Kubeconfig (stream): %v", err)
	}
	if string(kc) != kubeconfigBody {
		t.Fatalf("kubeconfig = %q", kc)
	}
}

func TestBootstrapAlreadyDoneReportsAlreadyBootstrapped(t *testing.T) {
	for name, code := range map[string]codes.Code{"AlreadyExists": codes.AlreadyExists, "Unknown": codes.Unknown} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeMachine{bootstrapErr: status.Error(code, "etcd is already bootstrapped")}
			if err := dialFake(t, fake).Bootstrap(context.Background()); !errors.Is(err, ErrAlreadyBootstrapped) {
				t.Fatalf("Bootstrap: %v, want ErrAlreadyBootstrapped", err)
			}
		})
	}
}

func TestBootstrapOtherErrorIsReturned(t *testing.T) {
	fake := &fakeMachine{bootstrapErr: status.Error(codes.Internal, "boom")}
	if err := dialFake(t, fake).Bootstrap(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestIsBootstrapped(t *testing.T) {
	tests := map[string]struct {
		members *machineapi.EtcdMemberListResponse
		err     error
		want    bool
		wantErr bool
	}{
		"member present": {
			members: &machineapi.EtcdMemberListResponse{Messages: []*machineapi.EtcdMembers{{Members: []*machineapi.EtcdMember{{Id: 1}}}}},
			want:    true,
		},
		"no members":       {members: &machineapi.EtcdMemberListResponse{Messages: []*machineapi.EtcdMembers{{}}}},
		"etcd not running": {err: status.Error(codes.Unknown, "etcd service is not running")},
		"not found":        {err: status.Error(codes.NotFound, "no such file")},
		"unexpected":       {err: status.Error(codes.Internal, "boom"), wantErr: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			n := dialFake(t, &fakeMachine{members: tc.members, membersErr: tc.err})
			got, err := n.IsBootstrapped(context.Background())
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got %v, %v; want %v, error=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func newState(t *testing.T) state.State {
	t.Helper()
	return state.WrapCore(namespaced.NewState(inmem.Build))
}

func TestStage(t *testing.T) {
	tests := map[runtime.MachineStage]Stage{
		runtime.MachineStageMaintenance: StageMaintenance,
		runtime.MachineStageBooting:     StageBooting,
		runtime.MachineStageRunning:     StageRunning,
		runtime.MachineStageInstalling:  StageInstalling,
		runtime.MachineStageRebooting:   StageOther,
		runtime.MachineStageUpgrading:   StageOther,
		runtime.MachineStageResetting:   StageOther,
		runtime.MachineStageUnknown:     StageOther,
	}
	for in, want := range tests {
		t.Run(in.String(), func(t *testing.T) {
			st := newState(t)
			ms := runtime.NewMachineStatus()
			ms.TypedSpec().Stage = in
			if err := st.Create(context.Background(), ms); err != nil {
				t.Fatal(err)
			}
			got, err := (&node{st: st}).Stage(context.Background())
			if err != nil || got != want {
				t.Fatalf("got %q, %v; want %q", got, err, want)
			}
		})
	}
}

func TestStageMissingResource(t *testing.T) {
	if _, err := (&node{st: newState(t)}).Stage(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestAddresses(t *testing.T) {
	st := newState(t)
	na := network.NewNodeAddress(network.NamespaceName, network.NodeAddressCurrentID)
	want := []netip.Prefix{netip.MustParsePrefix("10.0.0.5/24"), netip.MustParsePrefix("fd00::5/64")}
	na.TypedSpec().Addresses = want
	other := network.NewNodeAddress(network.NamespaceName, network.NodeAddressDefaultID)
	other.TypedSpec().Addresses = []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")}
	for _, r := range []resource.Resource{na, other} {
		if err := st.Create(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := (&node{st: st}).Addresses(context.Background())
	if err != nil || len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestAddressesMissingResource(t *testing.T) {
	if _, err := (&node{st: newState(t)}).Addresses(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestConfigured(t *testing.T) {
	st := newState(t)
	n := &node{st: st}
	if got, err := n.Configured(context.Background()); err != nil || got {
		t.Fatalf("without a configuration: got %v, %v; want false, nil", got, err)
	}
	if err := st.Create(context.Background(), config.NewMachineConfig(container.NewV1Alpha1(&v1alpha1.Config{ConfigVersion: "v1alpha1"}))); err != nil {
		t.Fatal(err)
	}
	if got, err := n.Configured(context.Background()); err != nil || !got {
		t.Fatalf("with a configuration: got %v, %v; want true, nil", got, err)
	}
}

func TestConfiguredError(t *testing.T) {
	if _, err := (&node{st: failingState{newState(t)}}).Configured(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

// failingState fails every Get with an error other than NotFound.
type failingState struct{ state.State }

func (failingState) Get(context.Context, resource.Pointer, ...state.GetOption) (resource.Resource, error) {
	return nil, status.Error(codes.Unavailable, "machined unavailable")
}
