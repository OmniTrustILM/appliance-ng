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

func testNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func nodeName(n *corev1.Node) string {
	if n == nil {
		return ""
	}
	return n.Name
}

func TestOwnNodeReadsTheNamedNode(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithObjects(testNode("aaa-stale"), testNode("ilm")).Build()
	t.Setenv(NodeNameEnv, "ilm")
	if n, err := OwnNode(ctx, c); err != nil || nodeName(n) != "ilm" {
		t.Errorf("OwnNode = %q, %v; want ilm", nodeName(n), err)
	}
	t.Setenv(NodeNameEnv, "missing")
	if n, err := OwnNode(ctx, c); err != nil || n != nil {
		t.Errorf("OwnNode of a missing node = %q, %v; want none", nodeName(n), err)
	}
	failing := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("boom")
		},
	}).Build()
	if _, err := OwnNode(ctx, failing); err == nil {
		t.Error("a get error was not reported")
	}
}

func TestOwnNodeWithoutANameUsesTheOnlyNode(t *testing.T) {
	ctx := context.Background()
	t.Setenv(NodeNameEnv, "")
	if n, err := OwnNode(ctx, fake.NewClientBuilder().Build()); err != nil || n != nil {
		t.Errorf("OwnNode without nodes = %q, %v; want none", nodeName(n), err)
	}
	if n, err := OwnNode(ctx, fake.NewClientBuilder().WithObjects(testNode("ilm")).Build()); err != nil || nodeName(n) != "ilm" {
		t.Errorf("OwnNode = %q, %v; want ilm", nodeName(n), err)
	}
	if _, err := OwnNode(ctx, fake.NewClientBuilder().WithObjects(testNode("a"), testNode("b")).Build()); err == nil {
		t.Error("two nodes without a name were not reported")
	}
	failing := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("boom")
		},
	}).Build()
	if _, err := OwnNode(ctx, failing); err == nil {
		t.Error("a list error was not reported")
	}
}
