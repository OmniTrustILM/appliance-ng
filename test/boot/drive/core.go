package main

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

const (
	// coreNamespace and coreService name ILM Core's Service.
	coreNamespace = "ilm"
	coreService   = "core"
	// clientCertHeader carries the client certificate to Core, as the ILM edge sets it.
	clientCertHeader = "X-Forwarded-Tls-Client-Cert"
)

// certHeader is the value of clientCertHeader for cert as ILM 2.19.0 Core decodes it: the leaf certificate as
// URL-encoded PEM, a space as %20, so that a decoder of either convention reads it.
func certHeader(cert tls.Certificate) string {
	encoded := url.QueryEscape(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Leaf.Raw})))
	return strings.ReplaceAll(encoded, "+", "%20")
}

// headerTransport sets the client certificate header on every request.
type headerTransport struct {
	value string
	next  http.RoundTripper
}

func (h headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set(clientCertHeader, h.value)
	return h.next.RoundTrip(req)
}

// coreClient is a client of base, ILM Core reached straight, that signs in with cert.
func coreClient(cert tls.Certificate) *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: headerTransport{value: certHeader(cert), next: http.DefaultTransport}}
}

// forwardCore forwards a local port to the port of a running pod of ILM Core's Service with kubeconfig and returns
// Core's base URL on it and a function that stops the forward.
func (f forwarded) forwardCore(ctx context.Context, kubeconfig []byte) (string, func(), error) {
	cfg, err := f.restConfig(kubeconfig)
	if err != nil {
		return "", nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", nil, errors.New("not a usable kubeconfig")
	}
	pod, port, err := corePod(ctx, cs)
	if err != nil {
		return "", nil, err
	}
	rt, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return "", nil, err
	}
	target := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(coreNamespace).Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: rt}, http.MethodPost, target)
	stop, ready := make(chan struct{}), make(chan struct{})
	pf, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{fmt.Sprintf("0:%d", port)}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		return "", nil, err
	}
	failed := make(chan error, 1)
	go func() { failed <- pf.ForwardPorts() }()
	select {
	case <-ready:
	case err := <-failed:
		return "", nil, fmt.Errorf("forward a port to pod %s: %w", pod, err)
	case <-ctx.Done():
		close(stop)
		return "", nil, ctx.Err()
	}
	ports, err := pf.GetPorts()
	if err != nil || len(ports) != 1 {
		close(stop)
		return "", nil, fmt.Errorf("forward a port to pod %s: %v", pod, err)
	}
	fmt.Printf("drive: ILM Core pod %s port %d forwarded to 127.0.0.1:%d\n", pod, port, ports[0].Local)
	return fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local), func() { close(stop) }, nil
}

// corePod is a running pod of ILM Core's Service and the container port the Service targets.
func corePod(ctx context.Context, cs kubernetes.Interface) (string, int32, error) {
	svc, err := cs.CoreV1().Services(coreNamespace).Get(ctx, coreService, metav1.GetOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("read service %s/%s: %w", coreNamespace, coreService, err)
	}
	if len(svc.Spec.Ports) == 0 {
		return "", 0, fmt.Errorf("service %s/%s has no port", coreNamespace, coreService)
	}
	pods, err := cs.CoreV1().Pods(coreNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(svc.Spec.Selector).String(), FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return "", 0, fmt.Errorf("list the pods of service %s/%s: %w", coreNamespace, coreService, err)
	}
	if len(pods.Items) == 0 {
		return "", 0, fmt.Errorf("service %s/%s has no running pod", coreNamespace, coreService)
	}
	pod, target := pods.Items[0], svc.Spec.Ports[0].TargetPort
	if target.IntVal > 0 {
		return pod.Name, target.IntVal, nil
	}
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.Name == target.StrVal || target.StrVal == "" && p.ContainerPort == svc.Spec.Ports[0].Port {
				return pod.Name, p.ContainerPort, nil
			}
		}
	}
	return pod.Name, svc.Spec.Ports[0].Port, nil
}
