package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
)

// Members of a backup.
const (
	applianceMember = "appliance.yaml"
	databaseMember  = "database.sql"
	secretsPrefix   = "secrets/"
)

// maxMember caps a member of a backup read into memory: a Secret near the 1 MiB that Kubernetes takes is larger as
// YAML.
const maxMember = 2 << 20

// errUnexpected marks a backup that holds what a backup of its appliance does not.
var errUnexpected = errors.New("the backup holds an unexpected member")

// certificateAnnotation names the Certificate of a cert-manager CertificateRequest.
const certificateAnnotation = "cert-manager.io/certificate-name"

var certificateRequestList = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "CertificateRequestList"}

// recreator is a Dumper that replaces the database with an empty one before a load.
type recreator interface {
	Recreate(ctx context.Context) error
}

// readMembers decrypts the backup and returns the content of the members want selects. Only small members are read
// this way, each at most maxMember bytes, and only into memory.
func readMembers(ctx context.Context, j *job, want func(name string) bool) (map[string][]byte, error) {
	f, err := open(ctx, j.status.File)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	out := map[string][]byte{}
	err = backup.Extract(f, j.id, func(name string, r io.Reader) error {
		if !want(name) {
			return nil
		}
		data, err := readCapped(r)
		out[name] = data
		return err
	})
	if err != nil {
		return nil, refusal(err)
	}
	return out, nil
}

// readCapped reads a member of at most maxMember bytes.
func readCapped(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxMember+1))
	if err == nil && len(data) > maxMember {
		return nil, fmt.Errorf("%w: a member larger than %d bytes", errUnexpected, maxMember)
	}
	return data, err
}

// readSecrets returns the backup's Secrets in the namespaces want selects, sorted by namespace and name. Every Secret
// in the backup must be the one its member path names and one that a backup of the backup's appliance collects.
func readSecrets(ctx context.Context, j *job, want func(namespace string) bool) ([]corev1.Secret, error) {
	members, err := readMembers(ctx, j, func(name string) bool {
		return name == applianceMember || strings.HasPrefix(name, secretsPrefix)
	})
	if err != nil {
		return nil, err
	}
	spec, err := applianceSpec(members)
	if err != nil {
		return nil, err
	}
	backedUp := &v1alpha1.Appliance{Spec: spec}
	delete(members, applianceMember)
	var out []corev1.Secret
	for _, name := range slices.Sorted(maps.Keys(members)) {
		s, err := memberSecret(backedUp, name, members[name])
		if err != nil {
			return nil, err
		}
		if want(s.Namespace) {
			out = append(out, s)
		}
	}
	return out, nil
}

// memberSecret reads the Secret of the member name of a backup of a.
func memberSecret(a *v1alpha1.Appliance, name string, data []byte) (corev1.Secret, error) {
	var s corev1.Secret
	if err := yaml.Unmarshal(data, &s); err != nil {
		return corev1.Secret{}, fmt.Errorf("read the backup's secret %s: %w", name, err)
	}
	if name != secretsPrefix+s.Namespace+"/"+s.Name+".yaml" || !backup.Restorable(a, s.Namespace, s.Name) {
		return corev1.Secret{}, fmt.Errorf("%w: %s", errUnexpected, name)
	}
	return s, nil
}

// applianceSpec reads the spec of the backup's appliance member, which every backup holds.
func applianceSpec(members map[string][]byte) (v1alpha1.ApplianceSpec, error) {
	data, ok := members[applianceMember]
	if !ok {
		return v1alpha1.ApplianceSpec{}, fmt.Errorf("%w: no %s", errUnexpected, applianceMember)
	}
	var restored struct {
		Spec v1alpha1.ApplianceSpec `json:"spec"`
	}
	if err := yaml.Unmarshal(data, &restored); err != nil {
		return v1alpha1.ApplianceSpec{}, fmt.Errorf("read the backup's appliance: %w", err)
	}
	return restored.Spec, nil
}

// restoreSystemSecrets writes the backup's Secrets outside the platform namespace, with the console accounts merged:
// a fresh restore keeps the current accounts, a restore in place only the account that started it. A fresh restore
// also writes the backup's recipient and the built-in root CA and leaves the recovery kit
// unconfirmed: the kit's cluster access belongs to the old node, so the administrator issues a new one. A restore in
// place keeps the recipient of the current kit, which may be newer than the backup's, and the root CA.
func (e *Engine) restoreSystemSecrets(ctx context.Context, j *job) error {
	secrets, err := readSecrets(ctx, j, func(ns string) bool { return ns != platform.Namespace })
	if err != nil {
		return err
	}
	if secrets, err = e.mergeAccounts(ctx, j, secrets); err != nil {
		return err
	}
	if j.status.InPlace {
		return e.writeSecrets(ctx, slices.DeleteFunc(secrets, keptInPlace))
	}
	if err := e.deleteRequests(ctx, certs.ClusterResourceNamespace, certs.RootCAName); err != nil {
		return err
	}
	if err := e.writeSecrets(ctx, secrets); err != nil {
		return err
	}
	return e.updateStatus(ctx, func(st *v1alpha1.ApplianceStatus) {
		if st.RecoveryKit != nil {
			st.RecoveryKit.Confirmed = false
		}
	})
}

// keptInPlace tells whether a restore in place keeps the current Secret instead of the backup's.
func keptInPlace(s corev1.Secret) bool {
	return s.Name == recovery.RecipientSecret || s.Namespace == certs.ClusterResourceNamespace
}

// mergeAccounts replaces the backup's accounts Secret with the merge of the current accounts into it, for a restore
// in place only the account that started it. A backup without accounts leaves the current accounts and their key as
// they are.
func (e *Engine) mergeAccounts(ctx context.Context, j *job, secrets []corev1.Secret) ([]corev1.Secret, error) {
	find := func(name string) int {
		for i, s := range secrets {
			if s.Namespace == kube.SystemNamespace && s.Name == name {
				return i
			}
		}
		return -1
	}
	users, key := find(auth.UsersSecret), find(auth.UsersKeySecret)
	switch {
	case users < 0 && key < 0:
		return secrets, nil
	case users < 0 || key < 0:
		return nil, fmt.Errorf("the backup holds only one of %s and %s", auth.UsersSecret, auth.UsersKeySecret)
	}
	current, err := e.liveSecret(ctx, kube.SystemNamespace, auth.UsersSecret)
	if err != nil {
		return nil, err
	}
	if current != nil && j.status.InPlace {
		if current, err = auth.OnlyUsers(current, j.status.StartedBy); err != nil {
			return nil, fmt.Errorf("read the console accounts: %w", err)
		}
	}
	currentKey, err := e.liveSecret(ctx, kube.SystemNamespace, auth.UsersKeySecret)
	if err != nil {
		return nil, err
	}
	merged, err := auth.MergeUsers(current, currentKey, &secrets[users], &secrets[key])
	if err != nil {
		return nil, fmt.Errorf("merge the console accounts: %w", err)
	}
	secrets[users] = *merged
	return secrets, nil
}

// liveSecret returns the Secret in the cluster, or nil when it does not exist.
func (e *Engine) liveSecret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	err := e.deps.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, s)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read secret %s/%s: %w", namespace, name, err)
	}
	return s, nil
}

func (e *Engine) writeSecrets(ctx context.Context, secrets []corev1.Secret) error {
	for i := range secrets {
		if err := e.writeSecret(ctx, &secrets[i]); err != nil {
			return err
		}
	}
	return nil
}

// writeSecret writes the backup's Secret into the live object: its data and type replace the live ones, its labels
// and annotations are merged in, and the live identity and owner references stay, which the operators that own
// such Secrets rely on. A Secret that does not exist is created.
func (e *Engine) writeSecret(ctx context.Context, s *corev1.Secret) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live, err := e.liveSecret(ctx, s.Namespace, s.Name)
		if err != nil {
			return err
		}
		if live == nil {
			return e.createSecret(ctx, s)
		}
		live.Data, live.StringData = s.Data, nil
		if s.Type != "" {
			live.Type = s.Type
		}
		live.Labels = merged(live.Labels, s.Labels)
		live.Annotations = merged(live.Annotations, s.Annotations)
		if err := e.deps.Client.Update(ctx, live, client.FieldOwner(kube.ManagerFieldManager)); err != nil {
			return fmt.Errorf("update secret %s/%s: %w", s.Namespace, s.Name, err)
		}
		return nil
	})
}

func (e *Engine) createSecret(ctx context.Context, s *corev1.Secret) error {
	created := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: s.Name, Labels: s.Labels, Annotations: s.Annotations},
		Immutable:  s.Immutable,
		Type:       s.Type,
		Data:       s.Data,
	}
	if err := e.deps.Client.Create(ctx, created, client.FieldOwner(kube.ManagerFieldManager)); err != nil {
		return fmt.Errorf("create secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	return nil
}

// merged returns live with the entries of restored added or replaced.
func merged(live, restored map[string]string) map[string]string {
	if len(restored) == 0 {
		return live
	}
	out := maps.Clone(live)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, restored)
	return out
}

// deleteRequests deletes the cert-manager CertificateRequests in namespace, only those of certificate when it is set.
// Without them cert-manager no longer finds a request that the restored key pair does not match, and keeps it.
func (e *Engine) deleteRequests(ctx context.Context, namespace, certificate string) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(certificateRequestList)
	if err := e.deps.Client.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list the certificate requests in %s: %w", namespace, err)
	}
	for i := range list.Items {
		r := &list.Items[i]
		if certificate != "" && r.GetAnnotations()[certificateAnnotation] != certificate {
			continue
		}
		if err := e.deps.Client.Delete(ctx, r); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete certificate request %s/%s: %w", namespace, r.GetName(), err)
		}
	}
	return nil
}

// restoreAppliance replaces the spec with the backup's, except the node section, which belongs to this appliance,
// and with the ILM host name chosen for the restore.
func (e *Engine) restoreAppliance(ctx context.Context, j *job) error {
	members, err := readMembers(ctx, j, func(name string) bool { return name == applianceMember })
	if err != nil {
		return err
	}
	restored, err := applianceSpec(members)
	if err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a, err := e.appliance(ctx)
		if err != nil {
			return err
		}
		spec := *restored.DeepCopy()
		spec.Node = a.Spec.Node
		if host := j.status.ILMHostName; host != "" {
			if spec.Platform == nil {
				spec.Platform = &v1alpha1.PlatformSpec{}
			}
			spec.Platform.HostName = host
		}
		a.Spec = spec
		return e.deps.Client.Update(ctx, a, client.FieldOwner(kube.ManagerFieldManager))
	})
}

// restoreILMSecrets writes the backup's Secrets of the platform namespace while the operator and ILM are stopped,
// after deleting the CertificateRequests there, so that cert-manager keeps the restored CA and certificate. It stops
// them again first: a resumed restore or an add-on pass may have started the operator since the stop step.
func (e *Engine) restoreILMSecrets(ctx context.Context, j *job) error {
	secrets, err := readSecrets(ctx, j, func(ns string) bool { return ns == platform.Namespace })
	if err != nil {
		return err
	}
	if err := e.stop(ctx, j); err != nil {
		return err
	}
	if err := e.deleteRequests(ctx, platform.Namespace, ""); err != nil {
		return err
	}
	return e.writeSecrets(ctx, secrets)
}

// restoreDatabase streams the backup's dump into the database, which a managed database first recreates empty. A
// backup without a dump leaves the database as it is. Like restoreILMSecrets, it stops the operator and ILM again
// first.
func (e *Engine) restoreDatabase(ctx context.Context, j *job) error {
	if err := e.stop(ctx, j); err != nil {
		return err
	}
	f, err := open(ctx, j.status.File)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	err = backup.Extract(f, j.id, func(name string, r io.Reader) error {
		if name != databaseMember {
			return nil
		}
		if rc, ok := e.deps.Dumper.(recreator); ok {
			if err := rc.Recreate(ctx); err != nil {
				return err
			}
		}
		return e.deps.Dumper.Load(ctx, r)
	})
	return refusal(err)
}

// restoreCertificate has the built-in certificate issued again for this appliance's host names by the restored root
// CA. An uploaded certificate stays; a warning names a host name it does not cover.
func (e *Engine) restoreCertificate(ctx context.Context, _ *job) error {
	a, err := e.appliance(ctx)
	if err != nil {
		return err
	}
	hosts := certs.Hosts(a)
	if a.Spec.TLS.Source == v1alpha1.TLSSourceSecret {
		return e.warnUncovered(ctx, a.Spec.TLS.SecretRef, hosts)
	}
	var ips []netip.Addr
	ip, found, err := certs.NodeAddress(ctx, e.deps.Client, a)
	if err != nil {
		return err
	}
	if found {
		ips = append(ips, ip)
	}
	if _, err := certs.EnsureBuiltin(ctx, e.deps.Client, hosts, ips); err != nil {
		return err
	}
	leaf := &corev1.Secret{}
	leaf.Namespace, leaf.Name = certs.Namespace, certs.SecretName
	if err := e.deps.Client.Delete(ctx, leaf); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete secret %s/%s: %w", certs.Namespace, certs.SecretName, err)
	}
	return nil
}

func (e *Engine) warnUncovered(ctx context.Context, secretName string, hosts []string) error {
	s, err := e.liveSecret(ctx, kube.SystemNamespace, secretName)
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("secret %s/%s not found", kube.SystemNamespace, secretName)
	}
	host := certs.UncoveredHost(s.Data[corev1.TLSCertKey], hosts)
	if host == "" {
		return nil
	}
	return e.updateStatus(ctx, func(st *v1alpha1.ApplianceStatus) {
		st.Restore.Warning = "the uploaded certificate does not cover " + host
	})
}
