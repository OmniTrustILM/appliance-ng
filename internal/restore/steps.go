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
	"github.com/OmniTrustILM/appliance-ng/internal/cbom"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/ilmclient"
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
	kept, err := e.keptCredentials(ctx, j)
	if err != nil {
		return err
	}
	secrets = slices.DeleteFunc(secrets, func(s corev1.Secret) bool {
		return s.Namespace == kube.SystemNamespace && slices.Contains(kept, s.Name)
	})
	if err := e.deleteUnknownIdentity(ctx, secrets); err != nil {
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

// deleteUnknownIdentity deletes the manager's ILM identity when the backup holds none: the restored database does
// not know its certificate, so the manager registers anew once the restore is over.
func (e *Engine) deleteUnknownIdentity(ctx context.Context, secrets []corev1.Secret) error {
	if slices.ContainsFunc(secrets, func(s corev1.Secret) bool {
		return s.Namespace == kube.SystemNamespace && s.Name == ilmclient.IdentitySecret
	}) {
		return nil
	}
	identity := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: kube.SystemNamespace, Name: ilmclient.IdentitySecret}}
	if err := e.deps.Client.Delete(ctx, identity); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete secret %s/%s: %w", kube.SystemNamespace, ilmclient.IdentitySecret, err)
	}
	return nil
}

// keptCredentials names the credentials Secrets of the dependencies whose choice the restore does not take from
// the backup: on a restore in place those of the current choice; on a fresh one those entered for the dependencies
// it chooses anew and the backup's of the dependencies these replace. The backup's Secrets of these names are not
// written.
func (e *Engine) keptCredentials(ctx context.Context, j *job) ([]string, error) {
	d := j.status.Dependencies
	if j.status.InPlace {
		a, err := e.appliance(ctx)
		if err != nil {
			return nil, err
		}
		return credentialsOf(a.Spec.Platform, dependencyKinds), nil
	}
	if d == nil || len(d.Replace) == 0 {
		return nil, nil
	}
	members, err := readMembers(ctx, j, func(name string) bool { return name == applianceMember })
	if err != nil {
		return nil, err
	}
	backedUp, err := applianceSpec(members)
	if err != nil {
		return nil, err
	}
	entered := &v1alpha1.PlatformSpec{Database: d.Database, Messaging: d.Messaging, Identity: d.Identity}
	return append(credentialsOf(entered, d.Replace), credentialsOf(backedUp.Platform, d.Replace)...), nil
}

// dependencyKinds are the dependencies a restore chooses.
var dependencyKinds = []string{v1alpha1.DependencyDatabase, v1alpha1.DependencyMessaging, v1alpha1.DependencyIdentity}

// credentialsOf names the credentials Secrets of the external dependencies of p among kinds.
func credentialsOf(p *v1alpha1.PlatformSpec, kinds []string) []string {
	if p == nil {
		return nil
	}
	var names []string
	if p.Database != nil && slices.Contains(kinds, v1alpha1.DependencyDatabase) {
		names = append(names, p.Database.CredentialsRef)
	}
	if p.Messaging != nil && slices.Contains(kinds, v1alpha1.DependencyMessaging) {
		names = append(names, p.Messaging.CredentialsRef)
	}
	if p.Identity != nil && slices.Contains(kinds, v1alpha1.DependencyIdentity) {
		names = append(names, p.Identity.CredentialsRef)
	}
	return names
}

// keptInPlace tells whether a restore in place keeps the current Secret instead of the backup's.
func keptInPlace(s corev1.Secret) bool {
	return s.Name == recovery.RecipientSecret || s.Namespace == certs.ClusterResourceNamespace
}

// mergeAccounts replaces the backup's accounts Secret with the merge of the current accounts into it, for a restore
// in place only the account that started it. The merged accounts are sealed under the current key, which stays: the
// accounts Secret is then the only one written, so a restart at any point leaves accounts that open under the key
// the appliance holds. A backup without accounts leaves the current accounts and their key as they are.
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
	if currentKey != nil {
		secrets = slices.Delete(secrets, key, key+1)
	}
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
// and with the ILM host name chosen for the restore. A restore in place keeps the current dependencies; a fresh one
// takes those it chooses anew.
func (e *Engine) restoreAppliance(ctx context.Context, j *job) error {
	members, err := readMembers(ctx, j, func(name string) bool { return name == applianceMember })
	if err != nil {
		return err
	}
	restored, err := applianceSpec(members)
	if err != nil {
		return err
	}
	if !j.status.InPlace {
		if err := e.usePlatformBundle(ctx, j.status.PlatformBundle); err != nil {
			return err
		}
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a, err := e.appliance(ctx)
		if err != nil {
			return err
		}
		spec := *restored.DeepCopy()
		spec.Node = a.Spec.Node
		if spec.Platform == nil {
			spec.Platform = &v1alpha1.PlatformSpec{}
		}
		if host := j.status.ILMHostName; host != "" {
			spec.Platform.HostName = host
		}
		chooseDependencies(spec.Platform, a.Spec.Platform, j.status)
		a.Spec = spec
		return e.deps.Client.Update(ctx, a, client.FieldOwner(kube.ManagerFieldManager))
	})
}

// usePlatformBundle makes the images of the loaded platform bundle version available in the appliance's registry,
// before the platform is installed from them, and records it as the platform bundle that runs, from which the add-on
// pass takes the ILM operator. An empty version records the release's own platform, replacing a bundle that an
// earlier fresh restore recorded.
func (e *Engine) usePlatformBundle(ctx context.Context, version string) error {
	if version == "" {
		return e.updateStatus(ctx, func(st *v1alpha1.ApplianceStatus) { st.Versions.PlatformBundle = "" })
	}
	if e.deps.Images == nil {
		return errors.New("no registry for the images of a platform bundle")
	}
	if err := e.deps.Images(ctx, version); err != nil {
		return fmt.Errorf("tag the images of platform bundle %s: %w", version, err)
	}
	return e.updateStatus(ctx, func(st *v1alpha1.ApplianceStatus) { st.Versions.PlatformBundle = version })
}

// chooseDependencies sets the dependencies of the restored platform p: the current ones of a restore in place,
// the ones a fresh restore chooses anew.
func chooseDependencies(p, current *v1alpha1.PlatformSpec, st v1alpha1.RestoreStatus) {
	if st.InPlace {
		if current == nil {
			current = &v1alpha1.PlatformSpec{}
		}
		p.Database, p.Messaging, p.Identity = current.Database, current.Messaging, current.Identity
		return
	}
	d := st.Dependencies
	if d.Replaces(v1alpha1.DependencyDatabase) {
		p.Database = d.Database
	}
	if d.Replaces(v1alpha1.DependencyMessaging) {
		p.Messaging = d.Messaging
	}
	if d.Replaces(v1alpha1.DependencyIdentity) {
		p.Identity = d.Identity
	}
}

// restoreILMSecrets writes the backup's Secrets of the platform namespace while the operator and ILM are stopped,
// after deleting the CertificateRequests there, so that cert-manager keeps the restored CA and certificate. It stops
// them again first: a resumed restore or an add-on pass may have started the operator since the stop step.
//
// A rollback, which skips the secrets step, also writes the backup's ILM identity of the manager, or deletes the
// current one when the backup holds none: the restored database knows only the backup's.
func (e *Engine) restoreILMSecrets(ctx context.Context, j *job) error {
	rollback := j.status.Rollback != nil
	secrets, err := readSecrets(ctx, j, func(ns string) bool {
		return ns == platform.Namespace || rollback && ns == kube.SystemNamespace
	})
	if err != nil {
		return err
	}
	secrets = slices.DeleteFunc(secrets, func(s corev1.Secret) bool {
		return s.Namespace == kube.SystemNamespace && s.Name != ilmclient.IdentitySecret
	})
	if err := e.stop(ctx, j); err != nil {
		return err
	}
	if err := e.deleteRequests(ctx, platform.Namespace, ""); err != nil {
		return err
	}
	if rollback {
		if err := e.deleteUnknownIdentity(ctx, secrets); err != nil {
			return err
		}
	}
	return e.writeSecrets(ctx, secrets)
}

// restoreDatabase streams the backup's dump into the database the Appliance chooses, which a managed database
// first recreates empty; an external one takes the dump, made to drop what it replaces, as it is. A backup without a
// dump leaves the database as it is. Like restoreILMSecrets, it stops the operator and ILM again first.
func (e *Engine) restoreDatabase(ctx context.Context, j *job) error {
	if err := e.stop(ctx, j); err != nil {
		return err
	}
	a, err := e.appliance(ctx)
	if err != nil {
		return err
	}
	dumper := e.deps.Dumper
	if a.Spec.Platform != nil && a.Spec.Platform.Database != nil {
		if e.deps.External == nil {
			return errors.New("no dumper for an external database")
		}
		dumper = e.deps.External(*a.Spec.Platform.Database)
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
		if rc, ok := dumper.(recreator); ok {
			if err := rc.Recreate(ctx); err != nil {
				return err
			}
		}
		return dumper.Load(ctx, r)
	})
	return refusal(err)
}

// errCBOMMember ends the search for a member of the CBOM store.
var errCBOMMember = errors.New("the backup holds CBOM objects")

// restoreCBOM makes the store on the appliance hold exactly the backup's CBOM objects: it writes them back, with
// their content type and metadata, and deletes the objects the backup does not hold. A backup with CBOM objects
// brings the store up by creating its volume claim, whether the restored repository is on or off; the appliance
// controller runs the store while a restore pauses the platform. Without CBOM objects and without a running store
// there is nothing to do.
func (e *Engine) restoreCBOM(ctx context.Context, j *job) error {
	a, err := e.appliance(ctx)
	if err != nil {
		return err
	}
	held, err := e.holdsCBOM(ctx, j)
	if err != nil {
		return err
	}
	runs, err := cbom.StoreRuns(ctx, e.deps.Client, a.Spec.Platform)
	if err != nil || !held && !runs {
		return err
	}
	if held {
		err := e.deps.Client.Create(ctx, cbom.Claim(), client.FieldOwner(kube.ManagerFieldManager))
		if client.IgnoreAlreadyExists(err) != nil {
			return fmt.Errorf("create claim %s/%s: %w", cbom.Namespace, cbom.StoreClaim, err)
		}
	}
	store, err := e.cbomStore(ctx)
	if err != nil {
		return err
	}
	f, err := open(ctx, j.status.File)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	keep := map[string]bool{}
	err = backup.Extract(f, j.id, func(name string, r io.Reader) error {
		key, ok, err := store.Restore(ctx, name, r)
		if ok && err == nil {
			keep[key] = true
		}
		return err
	})
	if err != nil {
		return refusal(err)
	}
	pruned, err := store.Prune(ctx, keep)
	if err != nil {
		return err
	}
	e.deps.Log.Info("restored the CBOM objects", "objects", len(keep), "deleted", pruned)
	return nil
}

func (e *Engine) holdsCBOM(ctx context.Context, j *job) (bool, error) {
	f, err := open(ctx, j.status.File)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	err = backup.Extract(f, j.id, func(name string, _ io.Reader) error {
		if cbom.IsMember(name) {
			return errCBOMMember
		}
		return nil
	})
	if errors.Is(err, errCBOMMember) {
		return true, nil
	}
	return false, refusal(err)
}

// cbomStore waits for the store on the appliance to answer with its bucket.
func (e *Engine) cbomStore(ctx context.Context) (*cbom.Store, error) {
	var store *cbom.Store
	err := e.waitFor(ctx, "the CBOM store", func(ctx context.Context) (bool, error) {
		s, err := cbom.OpenStore(ctx, e.deps.Client, e.deps.CBOMStore)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if s.Ready(ctx) != nil {
			return false, nil
		}
		store = s
		return true, nil
	})
	return store, err
}

// restoreCertificate has the built-in certificate, for this appliance's host names, and zot's issued again by the
// restored root CA, which the node's mirrors and the manager trust zot by. An uploaded certificate stays; a warning
// names a host name it does not cover.
func (e *Engine) restoreCertificate(ctx context.Context, _ *job) error {
	a, err := e.appliance(ctx)
	if err != nil {
		return err
	}
	hosts := certs.Hosts(a)
	if a.Spec.TLS.Source == v1alpha1.TLSSourceSecret {
		if err := e.warnUncovered(ctx, a.Spec.TLS.SecretRef, hosts); err != nil {
			return err
		}
	} else if err := e.ensureBuiltin(ctx, a, hosts); err != nil {
		return err
	}
	for _, name := range []string{certs.SecretName, certs.RegistrySecretName} {
		s := &corev1.Secret{}
		s.Namespace, s.Name = certs.Namespace, name
		if err := e.deps.Client.Delete(ctx, s); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete secret %s/%s: %w", certs.Namespace, name, err)
		}
	}
	return nil
}

// ensureBuiltin applies the built-in chain with a leaf for hosts and the node's address.
func (e *Engine) ensureBuiltin(ctx context.Context, a *v1alpha1.Appliance, hosts []string) error {
	var ips []netip.Addr
	ip, found, err := certs.NodeAddress(ctx, e.deps.Client, a)
	if err != nil {
		return err
	}
	if found {
		ips = append(ips, ip)
	}
	_, err = certs.EnsureBuiltin(ctx, e.deps.Client, hosts, ips)
	return err
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
