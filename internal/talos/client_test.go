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
	"slices"
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
	"github.com/siderolabs/talos/pkg/machinery/meta"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/etcd"
	"github.com/siderolabs/talos/pkg/machinery/resources/k8s"
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
	membersHang  bool // EtcdMemberList never answers, as when etcd has no leader
	kubeHang     bool // Kubeconfig never answers
	services     []*machineapi.ServiceInfo
	resets       []*machineapi.ResetRequest
	resetErr     error
	deadline     time.Time // deadline of the last Bootstrap call
	meta         map[uint32]string
	metaErr      error
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
	f.deadline, _ = ctx.Deadline()
	return &machineapi.BootstrapResponse{}, f.bootstrapErr
}

func (f *fakeMachine) EtcdMemberList(ctx context.Context, _ *machineapi.EtcdMemberListRequest) (*machineapi.EtcdMemberListResponse, error) {
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	if f.membersHang {
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	return f.members, f.membersErr
}

func (f *fakeMachine) Kubeconfig(_ *emptypb.Empty, stream grpc.ServerStreamingServer[common.Data]) error {
	if err := authorize(stream.Context()); err != nil {
		return err
	}
	if f.kubeHang {
		<-stream.Context().Done()
		return stream.Context().Err()
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

func (f *fakeMachine) ServiceList(ctx context.Context, _ *emptypb.Empty) (*machineapi.ServiceListResponse, error) {
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	return &machineapi.ServiceListResponse{Messages: []*machineapi.ServiceList{{Services: f.services}}}, nil
}

func (f *fakeMachine) MetaWrite(ctx context.Context, req *machineapi.MetaWriteRequest) (*machineapi.MetaWriteResponse, error) {
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	if f.metaErr != nil {
		return nil, f.metaErr
	}
	if f.meta == nil {
		f.meta = map[uint32]string{}
	}
	f.meta[req.GetKey()] = string(req.GetValue())
	return &machineapi.MetaWriteResponse{}, nil
}

func (f *fakeMachine) MetaDelete(ctx context.Context, req *machineapi.MetaDeleteRequest) (*machineapi.MetaDeleteResponse, error) {
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	if f.metaErr != nil {
		return nil, f.metaErr
	}
	if _, ok := f.meta[req.GetKey()]; !ok {
		return nil, status.Error(codes.NotFound, "meta key not found")
	}
	delete(f.meta, req.GetKey())
	return &machineapi.MetaDeleteResponse{}, nil
}

func (f *fakeMachine) Reset(ctx context.Context, req *machineapi.ResetRequest) (*machineapi.ResetResponse, error) {
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	f.resets = append(f.resets, req)
	return &machineapi.ResetResponse{}, f.resetErr
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

const (
	etcdImage         = "registry.k8s.io/etcd:v3.7.3"
	kubeletImage      = "ghcr.io/siderolabs/kubelet:v1.37.1"
	execFormatFailure = `Error running Containerd(etcd), going to restart forever: task "etcd" failed: exit code 255 (last log "exec /usr/local/bin/etcd: exec format error")`
	ioFailure         = `Error running Containerd(kubelet), going to restart forever: task "kubelet" failed: exit code 255 (last log "exec /usr/local/bin/kubelet: input/output error")`
	appFailure        = `Error running Containerd(kubelet), going to restart forever: task "kubelet" failed: exit code 1 (last log "E1112 failed to run Kubelet: exec plugin: exit status 1")`
)

func service(id string, msgs ...string) *machineapi.ServiceInfo {
	events := make([]*machineapi.ServiceEvent, 0, len(msgs))
	for _, m := range msgs {
		events = append(events, &machineapi.ServiceEvent{Msg: m, State: "Waiting"})
	}
	return &machineapi.ServiceInfo{Id: id, State: "Waiting", Events: &machineapi.ServiceEvents{Events: events}}
}

// specState holds the etcd and kubelet specs that name the images the services run from.
func specState(t *testing.T) state.State {
	t.Helper()
	st := newState(t)
	es := etcd.NewSpec(etcd.NamespaceName, etcd.SpecID)
	es.TypedSpec().Image = etcdImage
	ks := k8s.NewKubeletSpec(k8s.NamespaceName, k8s.KubeletID)
	ks.TypedSpec().Image = kubeletImage
	for _, r := range []resource.Resource{es, ks} {
		if err := st.Create(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func TestBrokenSystemImages(t *testing.T) {
	fake := &fakeMachine{services: []*machineapi.ServiceInfo{
		service("apid", execFormatFailure),
		service("etcd", "Starting", "Started task etcd", execFormatFailure),
		service("kubelet", ioFailure, "Started task kubelet", "Health check successful"),
	}}
	n := dialFake(t, fake)
	n.st = specState(t)

	got, err := n.BrokenSystemImages(context.Background())
	if err != nil || !slices.Equal(got, []string{etcdImage}) {
		t.Fatalf("got %v, %v; want [%s]", got, err, etcdImage)
	}

	fake.services = append(fake.services, service("kubelet", appFailure))
	if got, err = n.BrokenSystemImages(context.Background()); err != nil || !slices.Equal(got, []string{etcdImage}) {
		t.Fatalf("got %v, %v; a failure of the program itself is not a broken image", got, err)
	}

	fake.services = append(fake.services, service("kubelet", ioFailure))
	got, err = n.BrokenSystemImages(context.Background())
	if err != nil || !slices.Equal(got, []string{etcdImage, kubeletImage}) {
		t.Fatalf("got %v, %v; want both images", got, err)
	}
}

func TestBrokenSystemImagesHealthy(t *testing.T) {
	n := dialFake(t, &fakeMachine{services: []*machineapi.ServiceInfo{service("etcd", execFormatFailure, "Started task etcd")}})
	n.st = newState(t)
	if got, err := n.BrokenSystemImages(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want none", got, err)
	}
}

func TestBrokenSystemImagesWithoutSpec(t *testing.T) {
	n := dialFake(t, &fakeMachine{services: []*machineapi.ServiceInfo{service("kubelet", execFormatFailure)}})
	n.st = newState(t)
	if _, err := n.BrokenSystemImages(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestResetRecorded(t *testing.T) {
	n := dialFake(t, &fakeMachine{})
	n.st = newState(t)
	if got, err := n.ResetRecorded(context.Background()); err != nil || got {
		t.Fatalf("got %v, %v; want no record", got, err)
	}

	mk := runtime.NewMetaKey(runtime.NamespaceName, runtime.MetaKeyTagToID(meta.UserReserved1))
	mk.TypedSpec().Value = "reset"
	if err := n.st.Create(context.Background(), mk); err != nil {
		t.Fatal(err)
	}
	if got, err := n.ResetRecorded(context.Background()); err != nil || !got {
		t.Fatalf("got %v, %v; want the record", got, err)
	}
}

func TestResetRecordedError(t *testing.T) {
	n := dialFake(t, &fakeMachine{})
	n.st = failingState{newState(t)}
	if _, err := n.ResetRecorded(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestRecordReset(t *testing.T) {
	fake := &fakeMachine{}
	if err := dialFake(t, fake).RecordReset(context.Background()); err != nil {
		t.Fatalf("RecordReset: %v", err)
	}
	if _, ok := fake.meta[uint32(meta.UserReserved1)]; !ok || len(fake.meta) != 1 {
		t.Fatalf("META %v, want the record under UserReserved1", fake.meta)
	}

	fake.metaErr = status.Error(codes.ResourceExhausted, "meta write failed")
	if err := dialFake(t, fake).RecordReset(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestClearResetRecord(t *testing.T) {
	fake := &fakeMachine{meta: map[uint32]string{uint32(meta.UserReserved1): "reset"}}
	n := dialFake(t, fake)
	if err := n.ClearResetRecord(context.Background()); err != nil || len(fake.meta) != 0 {
		t.Fatalf("META %v, %v; want the key deleted", fake.meta, err)
	}
	if err := n.ClearResetRecord(context.Background()); err != nil {
		t.Fatalf("clearing a missing key: %v", err)
	}
	fake.metaErr = status.Error(codes.Unavailable, "machined unavailable")
	if err := n.ClearResetRecord(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestWasReady(t *testing.T) {
	n := dialFake(t, &fakeMachine{})
	n.st = newState(t)
	if got, err := n.WasReady(context.Background()); err != nil || got {
		t.Fatalf("got %v, %v; want no marker", got, err)
	}

	mk := runtime.NewMetaKey(runtime.NamespaceName, runtime.MetaKeyTagToID(meta.UserReserved2))
	mk.TypedSpec().Value = "ready"
	if err := n.st.Create(context.Background(), mk); err != nil {
		t.Fatal(err)
	}
	if got, err := n.WasReady(context.Background()); err != nil || !got {
		t.Fatalf("got %v, %v; want the marker", got, err)
	}
}

func TestWasReadyError(t *testing.T) {
	n := dialFake(t, &fakeMachine{})
	n.st = failingState{newState(t)}
	if _, err := n.WasReady(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestMarkReady(t *testing.T) {
	fake := &fakeMachine{}
	if err := dialFake(t, fake).MarkReady(context.Background()); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	if _, ok := fake.meta[uint32(meta.UserReserved2)]; !ok || len(fake.meta) != 1 {
		t.Fatalf("META %v, want the marker under UserReserved2", fake.meta)
	}

	fake.metaErr = status.Error(codes.ResourceExhausted, "meta write failed")
	if err := dialFake(t, fake).MarkReady(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestResetEphemeral(t *testing.T) {
	fake := &fakeMachine{}
	if err := dialFake(t, fake).ResetEphemeral(context.Background()); err != nil {
		t.Fatalf("ResetEphemeral: %v", err)
	}
	if len(fake.resets) != 1 {
		t.Fatalf("%d reset calls, want one", len(fake.resets))
	}
	req := fake.resets[0]
	if req.GetGraceful() || !req.GetReboot() || req.GetMode() != machineapi.ResetRequest_SYSTEM_DISK || len(req.GetUserDisksToWipe()) != 0 {
		t.Fatalf("request %v, want a forced reset of the system disk followed by a reboot", req)
	}
	parts := req.GetSystemPartitionsToWipe()
	if len(parts) != 1 || parts[0].GetLabel() != "EPHEMERAL" || !parts[0].GetWipe() {
		t.Fatalf("partitions %v, want EPHEMERAL only", parts)
	}
}

func TestResetEphemeralError(t *testing.T) {
	fake := &fakeMachine{resetErr: status.Error(codes.Internal, "boom")}
	if err := dialFake(t, fake).ResetEphemeral(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

// shortCallTimeout makes every call to the node give up after a short time for the duration of the test.
func shortCallTimeout(t *testing.T) {
	t.Helper()
	old := callTimeout
	callTimeout = 100 * time.Millisecond
	t.Cleanup(func() { callTimeout = old })
}

func TestCallsGiveUpWhenTheNodeDoesNotAnswer(t *testing.T) {
	shortCallTimeout(t)
	fake := &fakeMachine{membersHang: true}
	n := dialFake(t, fake)

	done := make(chan error, 1)
	go func() {
		_, err := n.IsBootstrapped(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if status.Code(errors.Unwrap(err)) != codes.DeadlineExceeded {
			t.Fatalf("IsBootstrapped: %v, want a deadline error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("IsBootstrapped did not give up")
	}
	if err := n.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap after the failed check: %v", err)
	}
}

func TestCallsKeepALongerCallerDeadline(t *testing.T) {
	fake := &fakeMachine{}
	n := dialFake(t, fake)

	want := time.Now().Add(callTimeout + time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), want)
	defer cancel()
	if err := n.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if got := fake.deadline; got.Before(want.Add(-time.Second)) {
		t.Fatalf("call deadline %v, want the caller's %v", got, want)
	}
}

func TestStreamsGiveUpWhenTheNodeDoesNotAnswer(t *testing.T) {
	shortCallTimeout(t)
	n := dialFake(t, &fakeMachine{kubeHang: true})

	done := make(chan error, 1)
	go func() {
		_, err := n.Kubeconfig(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Kubeconfig did not give up")
	}
}
