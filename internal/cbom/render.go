// Package cbom renders the CBOM repository and its on-appliance S3 store, and reads and writes the store's objects
// for a backup and a restore.
package cbom

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"

	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/common/util"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/engine"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

// The kind of the workloads and the label that names an application.
const (
	kindDeployment = "Deployment"
	labelName      = "app.kubernetes.io/name"
)

// The chart and the images the release pins for the repository and its store.
const (
	ChartName       = "cbom-repository"
	StoreImage      = "docker.io/versity/versitygw:v1.11.0"
	RepositoryImage = "hub.omnitrustregistry.com/ilm/cbom-repository:1.2.0"
)

// Namespace holds the repository and its store: they belong to ILM, whose Core calls the repository.
const Namespace = platform.Namespace

// The on-appliance store: a versitygw S3 server on a local-path volume, whose root keys are generated on the device
// and kept in StoreSecret, which the repository reads too.
const (
	StoreName   = "cbom-s3"
	StorePort   = 7070
	StoreClaim  = "cbom-s3-data"
	StoreSecret = "cbom-s3-credentials"
	Bucket      = "cbom-repo"
	StoreRegion = "us-east-1"
	// StoreSize is the size the store's volume claim requests; local-path does not enforce it.
	StoreSize = "5Gi"
	// AccessKeyKey and SecretKeyKey are the keys of the credentials Secret the chart reads.
	AccessKeyKey = "access_key"
	SecretKeyKey = "secret_key"
)

// ExternalSecret is the copy in Namespace of the credentials of an external store, in the keys the chart reads.
const ExternalSecret = "appliance-cbom-s3"

// The repository as the chart names it.
const (
	releaseName       = "cbom-repository"
	repositoryService = "cbom-repository-service"
	repositoryPort    = 8080
	repositoryDeploy  = "cbom-repository-deployment"
	trustedSecret     = "trusted-certificates-cbom-repository"
	storeUser         = 10001
)

// URL is the repository's in-cluster address that ILM's Core calls; the repository is not published at the edge.
func URL() string {
	return "http://" + net.JoinHostPort(repositoryService+"."+Namespace+".svc", strconv.Itoa(repositoryPort))
}

// Inputs are what the rendering needs besides the spec.
type Inputs struct {
	// ILMHost is the ILM host name, whose https origin the repository admits.
	ILMHost string
	// RepositoryImage is the pinned reference, with its digest.
	RepositoryImage string
	// CA is the PEM CA bundle that signs an external store's certificate; empty trusts the system roots.
	CA string
}

// Render returns the objects of the CBOM repository spec describes, in Namespace, from chart, the archive of the
// pinned chart: its Deployment and Service and the Secret of a CA. Nothing is rendered when the repository is off. The
// chart's own credentials Secret and its test Pod are never rendered; the credentials Secret is the caller's to create.
// The store on the appliance is StoreObjects.
func Render(spec *v1alpha1.CBOMSpec, chart []byte, in Inputs) ([]*unstructured.Unstructured, error) {
	if spec == nil || !spec.Enabled {
		return nil, nil
	}
	values, err := chartValues(spec, in)
	if err != nil {
		return nil, err
	}
	return renderChart(chart, values)
}

// Workloads lists every object Render and StoreObjects may return, by kind and name, so that those a spec no longer renders are
// deleted. The store's volume claim and the credentials Secrets are not among them: they keep the stored CBOMs.
func Workloads() []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	add := func(apiVersion, kind, name string) {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(apiVersion)
		u.SetKind(kind)
		u.SetNamespace(Namespace)
		u.SetName(name)
		out = append(out, u)
	}
	add("apps/v1", kindDeployment, StoreName)
	add("v1", "Service", StoreName)
	add("apps/v1", kindDeployment, repositoryDeploy)
	add("v1", "Service", repositoryService)
	add("v1", "Secret", trustedSecret)
	return out
}

// Claim is the store's volume claim on the local-path storage of the data volume.
func Claim() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: StoreClaim, Labels: storeLabels()},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: ptr.To("local-path"),
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(StoreSize)}},
		},
	}
}

// chartValues are the chart's values for spec: the pinned image, a restricted security context, the store and its
// credentials, and the ILM origin.
func chartValues(spec *v1alpha1.CBOMSpec, in Inputs) (map[string]any, error) {
	image, err := imageValues(in.RepositoryImage)
	if err != nil {
		return nil, err
	}
	image["securityContext"] = map[string]any{
		"runAsNonRoot": true, "readOnlyRootFilesystem": true, "allowPrivilegeEscalation": false,
		"capabilities": map[string]any{"drop": []any{"ALL"}},
	}
	image["resources"] = map[string]any{
		"requests": map[string]any{"cpu": "10m", "memory": "64Mi"}, "limits": map[string]any{"memory": "256Mi"},
	}
	repo := map[string]any{
		"endpoint": fmt.Sprintf("http://%s:%d", StoreName, StorePort), "region": StoreRegion, "bucket": Bucket,
		"usePathStyle": true, "existingSecret": StoreSecret, "corsAllowedOrigins": "https://" + in.ILMHost,
	}
	values := map[string]any{
		"image":              image,
		"podSecurityContext": map[string]any{"runAsNonRoot": true, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"cbomRepo":           repo,
	}
	if spec.Storage == v1alpha1.CBOMStorageS3 {
		if spec.S3 == nil {
			return nil, errors.New("the CBOM repository's external store has no bucket")
		}
		repo["endpoint"], repo["bucket"], repo["existingSecret"] = endpointURL(spec.S3.Endpoint), spec.S3.Bucket, ExternalSecret
		if spec.S3.Region != "" {
			repo["region"] = spec.S3.Region
		}
		if in.CA != "" {
			values["trusted"] = map[string]any{"certificates": in.CA}
		}
	}
	return values, nil
}

// endpointURL is the S3 endpoint as a URL: one without a scheme is https, as for the backup targets.
func endpointURL(endpoint string) string {
	if strings.Contains(endpoint, "://") {
		return endpoint
	}
	return "https://" + endpoint
}

// imageValues splits a pinned reference registry/repository/name:tag@digest into the chart's image values.
func imageValues(ref string) (map[string]any, error) {
	name, digest, ok := strings.Cut(ref, "@")
	registry, path, hasRegistry := strings.Cut(name, "/")
	slash, colon := strings.LastIndex(path, "/"), strings.LastIndex(path, ":")
	if !ok || !hasRegistry || slash < 0 || colon < slash {
		return nil, fmt.Errorf("image %q is not registry/repository/name:tag@digest", ref)
	}
	return map[string]any{
		"registry": registry, "repository": path[:slash], "name": path[slash+1 : colon], "tag": path[colon+1:], "digest": digest,
	}, nil
}

// renderChart renders chart with values through the Helm template engine, without a release or a cluster, and returns
// its objects in Namespace sorted by template, without empty documents, notes and tests.
func renderChart(archive []byte, values map[string]any) ([]*unstructured.Unstructured, error) {
	c, err := loader.LoadArchive(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("load the %s chart: %w", ChartName, err)
	}
	options := common.ReleaseOptions{Name: releaseName, Namespace: Namespace, IsInstall: true}
	vals, err := util.ToRenderValues(c, values, options, common.DefaultCapabilities)
	if err != nil {
		return nil, fmt.Errorf("the values of the %s chart: %w", ChartName, err)
	}
	files, err := engine.Render(c, vals)
	if err != nil {
		return nil, fmt.Errorf("render the %s chart: %w", ChartName, err)
	}
	var out []*unstructured.Unstructured
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if strings.Contains(name, "/templates/tests/") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		objs, err := documents(files[name])
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		out = append(out, objs...)
	}
	return out, nil
}

// documents decodes the non-empty YAML documents of text and puts them in Namespace.
func documents(text string) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	reader := k8syaml.NewYAMLReader(bufio.NewReader(strings.NewReader(text)))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		obj := map[string]any{}
		if err := k8syaml.Unmarshal(doc, &obj); err != nil {
			return nil, err
		}
		if len(obj) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: obj}
		u.SetNamespace(Namespace)
		out = append(out, u)
	}
}

func storeLabels() map[string]string {
	return map[string]string{labelName: StoreName, "app.kubernetes.io/part-of": ChartName}
}

// StoreObjects are the store's Deployment and Service from its pinned image: versitygw's POSIX back end on the
// store's claim, one bucket directory made before it starts, non-root and with the root keys of StoreSecret.
func StoreObjects(image string) ([]*unstructured.Unstructured, error) {
	if !strings.Contains(image, "@") {
		return nil, fmt.Errorf("image %q has no digest", image)
	}
	security := &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
		Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	mounts := []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	key := func(env, key string) corev1.EnvVar {
		return corev1.EnvVar{Name: env, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: StoreSecret}, Key: key,
		}}}
	}
	probe := func(period int32) *corev1.Probe {
		return &corev1.Probe{PeriodSeconds: period, ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromString("s3")}}}
	}
	labels := storeLabels()
	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: kindDeployment},
		ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: StoreName, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{labelName: StoreName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(false),
					EnableServiceLinks:           ptr.To(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](storeUser), RunAsGroup: ptr.To[int64](storeUser),
						FSGroup: ptr.To[int64](storeUser), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					InitContainers: []corev1.Container{{
						Name: "bucket", Image: image, Command: []string{"mkdir", "-p", "/data/" + Bucket},
						SecurityContext: security, VolumeMounts: mounts,
					}},
					Containers: []corev1.Container{{
						Name: "versitygw", Image: image,
						Args:           []string{"--port", ":" + strconv.Itoa(StorePort), "--health", "/health", "--region", StoreRegion, "posix", "/data"},
						Env:            []corev1.EnvVar{key("ROOT_ACCESS_KEY", AccessKeyKey), key("ROOT_SECRET_KEY", SecretKeyKey)},
						Ports:          []corev1.ContainerPort{{Name: "s3", ContainerPort: StorePort}},
						ReadinessProbe: probe(10), LivenessProbe: probe(20),
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
						},
						SecurityContext: security, VolumeMounts: mounts,
					}},
					Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: StoreClaim},
					}}},
				},
			},
		},
	}
	service := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: StoreName, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{labelName: StoreName},
			Ports:    []corev1.ServicePort{{Name: "s3", Port: StorePort, TargetPort: intstr.FromString("s3")}},
		},
	}
	var out []*unstructured.Unstructured
	for _, obj := range []runtime.Object{deployment, service} {
		content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return nil, err
		}
		u := &unstructured.Unstructured{Object: content}
		unstructured.RemoveNestedField(u.Object, "status")
		unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
		unstructured.RemoveNestedField(u.Object, "spec", "template", "metadata", "creationTimestamp")
		out = append(out, u)
	}
	return out, nil
}
