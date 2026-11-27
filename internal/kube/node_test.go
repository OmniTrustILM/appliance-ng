package kube

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func testNode(name string, addresses ...corev1.NodeAddress) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Addresses: addresses}}
}

func nodeName(n *corev1.Node) string {
	if n == nil {
		return ""
	}
	return n.Name
}

func failingGet() client.WithWatch {
	return fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("boom")
		},
	}).Build()
}

func TestNodeByName(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithObjects(testNode("ilm"), testNode("ilm2")).Build()
	if n, err := NodeByName(ctx, c, "ilm2"); err != nil || nodeName(n) != "ilm2" {
		t.Errorf("NodeByName(ilm2) = %q, %v; want ilm2", nodeName(n), err)
	}
	if n, err := NodeByName(ctx, c, "missing"); err != nil || n != nil {
		t.Errorf("NodeByName of a missing node = %q, %v; want none", nodeName(n), err)
	}
	if _, err := NodeByName(ctx, c, ""); err == nil {
		t.Error("an empty name was not reported")
	}
	if _, err := NodeByName(ctx, failingGet(), "ilm"); err == nil {
		t.Error("a get error was not reported")
	}
}

func TestOwnNodeReadsTheNamedNode(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithObjects(testNode("aaa-stale"), testNode("ilm")).Build()
	t.Setenv(NodeNameEnv, "ilm")
	if n, err := OwnNode(ctx, c); err != nil || nodeName(n) != "ilm" {
		t.Errorf("OwnNode = %q, %v; want ilm", nodeName(n), err)
	}
	t.Setenv(NodeNameEnv, "")
	if _, err := OwnNode(ctx, fake.NewClientBuilder().WithObjects(testNode("ilm")).Build()); err == nil {
		t.Errorf("OwnNode without %s was not reported", NodeNameEnv)
	}
}

func TestInternalAddress(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithObjects(
		testNode("ilm2",
			corev1.NodeAddress{Type: corev1.NodeHostName, Address: "ilm2"},
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.1.12"}),
		testNode("bare"),
	).Build()
	if a, err := InternalAddress(ctx, c, "ilm2"); err != nil || a != "10.0.1.12" {
		t.Errorf("InternalAddress(ilm2) = %q, %v; want 10.0.1.12", a, err)
	}
	for _, name := range []string{"bare", "missing"} {
		if a, err := InternalAddress(ctx, c, name); err == nil {
			t.Errorf("InternalAddress(%s) = %q; want an error", name, a)
		}
	}
	if _, err := InternalAddress(ctx, failingGet(), "ilm2"); err == nil {
		t.Error("a get error was not reported")
	}
}
