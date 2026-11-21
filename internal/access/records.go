package access

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

// The kinds of issued credentials.
const (
	KindKubeconfig  = "kubeconfig"
	KindTalosconfig = "talosconfig"
)

const (
	// TalosconfigRecords is the ConfigMap in Namespace that records the issued talosconfigs until they expire.
	TalosconfigRecords = "appliance-talosconfigs"
	recordsKey         = "records"
)

// now is the clock of the records; tests replace it.
var now = time.Now

// Credential describes an issued credential, never its secret.
type Credential struct {
	Kind string
	// Name revokes a kubeconfig; talosconfigs have none.
	Name     string
	Role     string
	IssuedBy string
	IssuedAt time.Time
	// Expires is nil for a long-lived kubeconfig.
	Expires *time.Time
	// BreakGlass marks the kubeconfig of the recovery kit.
	BreakGlass bool
}

// talosRecord is an issued talosconfig in TalosconfigRecords.
type talosRecord struct {
	Role     string    `json:"role"`
	NotAfter time.Time `json:"notAfter"`
	IssuedBy string    `json:"issuedBy"`
	IssuedAt time.Time `json:"issuedAt"`
}

// ErrCorruptRecords is returned when TalosconfigRecords does not parse.
var ErrCorruptRecords = errors.New("the talosconfig records do not parse")

// List returns the issued kubeconfigs, ordered by name, then the talosconfigs that have not expired, oldest first.
// When the talosconfig records do not parse, it returns the kubeconfigs with an error that wraps ErrCorruptRecords.
func List(ctx context.Context, c client.Reader) ([]Credential, error) {
	creds, err := kubeconfigs(ctx, c)
	if err != nil {
		return nil, err
	}
	cm, err := records(ctx, c)
	if err != nil {
		return nil, err
	}
	talos, err := decodeRecords(cm)
	if err != nil {
		return creds, err
	}
	at := now()
	for _, r := range talos {
		if r.NotAfter.After(at) {
			creds = append(creds, Credential{Kind: KindTalosconfig, Role: r.Role, IssuedBy: r.IssuedBy, IssuedAt: r.IssuedAt, Expires: &r.NotAfter})
		}
	}
	return creds, nil
}

// kubeconfigs returns the issued kubeconfigs, ordered by name.
func kubeconfigs(ctx context.Context, c client.Reader) ([]Credential, error) {
	accounts := &corev1.ServiceAccountList{}
	if err := c.List(ctx, accounts, client.InNamespace(Namespace), client.MatchingLabels{LabelKind: KindKubeconfig}); err != nil {
		return nil, fmt.Errorf("list the ServiceAccounts in %s: %w", Namespace, err)
	}
	creds := make([]Credential, 0, len(accounts.Items))
	for i := range accounts.Items {
		creds = append(creds, describe(&accounts.Items[i]))
	}
	slices.SortFunc(creds, func(a, b Credential) int { return cmp.Compare(a.Name, b.Name) })
	return creds, nil
}

// describe returns the credential of a kubeconfig's ServiceAccount.
func describe(sa *corev1.ServiceAccount) Credential {
	c := Credential{Kind: KindKubeconfig, Name: sa.Name, Role: sa.Annotations[AnnotationRole], IssuedBy: sa.Annotations[AnnotationIssuedBy],
		BreakGlass: sa.Name == BreakGlass}
	if t, err := time.Parse(time.RFC3339, sa.Annotations[AnnotationIssuedAt]); err == nil {
		c.IssuedAt = t
	}
	if t, err := time.Parse(time.RFC3339, sa.Annotations[AnnotationExpires]); err == nil {
		c.Expires = &t
	}
	return c
}

// RecordTalosconfig records a talosconfig issued with role until notAfter, and drops the records that expired.
func RecordTalosconfig(ctx context.Context, c client.Client, role string, notAfter time.Time, issuedBy string) error {
	record := talosRecord{Role: role, NotAfter: notAfter.UTC(), IssuedBy: issuedBy, IssuedAt: now().UTC().Truncate(time.Second)}
	return updateRecords(ctx, c, func(rs []talosRecord) []talosRecord { return append(rs, record) })
}

// RemoveExpired revokes the kubeconfigs that expired by at, never the break-glass one, and drops the talosconfig
// records that did. A failed revoke and corrupt records hold up nothing else.
func RemoveExpired(ctx context.Context, c client.Client, at time.Time) error {
	creds, err := kubeconfigs(ctx, c)
	if err != nil {
		return err
	}
	var errs []error
	for _, cred := range creds {
		if cred.BreakGlass || cred.Expires == nil || cred.Expires.After(at) {
			continue
		}
		if err := Revoke(ctx, c, cred.Name); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, err)
		}
	}
	errs = append(errs, updateRecords(ctx, c, func(rs []talosRecord) []talosRecord {
		return slices.DeleteFunc(rs, func(r talosRecord) bool { return !r.NotAfter.After(at) })
	}))
	return errors.Join(errs...)
}

// updateRecords applies change to the talosconfig records, after dropping those that expired, and writes them only
// when they changed. A write that lost a race with another one is retried on the records that won.
func updateRecords(ctx context.Context, c client.Client, change func([]talosRecord) []talosRecord) error {
	lostRace := func(err error) bool { return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) }
	return retry.OnError(retry.DefaultRetry, lostRace, func() error {
		cm, err := records(ctx, c)
		if err != nil {
			return err
		}
		current, err := decodeRecords(cm)
		if err != nil {
			return err
		}
		at := now()
		next := change(slices.DeleteFunc(slices.Clone(current), func(r talosRecord) bool { return !r.NotAfter.After(at) }))
		if slices.Equal(next, current) {
			return nil
		}
		data, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("encode the talosconfig records: %w", err)
		}
		cm.Data = map[string]string{recordsKey: string(data)}
		if cm.ResourceVersion == "" {
			return c.Create(ctx, cm, client.FieldOwner(kube.ManagerFieldManager))
		}
		return c.Update(ctx, cm, client.FieldOwner(kube.ManagerFieldManager))
	})
}

// records reads TalosconfigRecords; a ConfigMap without a resource version does not exist yet.
func records(ctx context.Context, c client.Reader) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: TalosconfigRecords}, cm)
	if apierrors.IsNotFound(err) {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: TalosconfigRecords}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read ConfigMap %s/%s: %w", Namespace, TalosconfigRecords, err)
	}
	return cm, nil
}

func decodeRecords(cm *corev1.ConfigMap) ([]talosRecord, error) {
	data, ok := cm.Data[recordsKey]
	if !ok {
		return nil, nil
	}
	var rs []talosRecord
	if err := json.Unmarshal([]byte(data), &rs); err != nil {
		// The decoder's error may quote the content; the records name no secret, but the error stays fixed.
		return nil, fmt.Errorf("ConfigMap %s/%s key %s: %w", Namespace, TalosconfigRecords, recordsKey, ErrCorruptRecords)
	}
	return rs, nil
}
