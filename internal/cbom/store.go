package cbom

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

const (
	dialTimeout = 10 * time.Second
	maxRetries  = 3
	// partSize bounds what a write of an object buffers.
	partSize = 16 << 20
)

// StoreRuns tells whether the store on the appliance runs for p: its storage is the appliance and the repository is
// on, or it is off while the store's volume, which keeps the CBOMs stored before, exists. A backup then holds the
// store's objects.
func StoreRuns(ctx context.Context, c client.Reader, p *v1alpha1.PlatformSpec) (bool, error) {
	if p == nil || p.CBOM == nil || p.CBOM.Storage == v1alpha1.CBOMStorageS3 {
		return false, nil
	}
	if p.CBOM.Enabled {
		return true, nil
	}
	claim := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}}
	err := c.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: StoreClaim}, claim)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read claim %s/%s: %w", Namespace, StoreClaim, err)
	}
	return true, nil
}

// Object is an object of the store as a backup keeps it: its key, size, content type and user metadata.
type Object struct {
	Key         string            `json:"key"`
	Size        int64             `json:"size"`
	ContentType string            `json:"contentType,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// Store is the bucket of the on-appliance S3 store, reached with its root keys.
type Store struct {
	client *minio.Client
}

// OpenStore reaches the store at address, host:port, with the root keys of StoreSecret. An empty address is the
// cluster IP of the store's Service: the manager runs in the host network, where the cluster's DNS does not answer.
func OpenStore(ctx context.Context, c client.Reader, address string) (*Store, error) {
	keys := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: StoreSecret}, keys); err != nil {
		return nil, fmt.Errorf("read secret %s/%s: %w", Namespace, StoreSecret, err)
	}
	access, secret := keys.Data[AccessKeyKey], keys.Data[SecretKeyKey]
	if len(access) == 0 || len(secret) == 0 {
		return nil, fmt.Errorf("secret %s/%s holds no keys", Namespace, StoreSecret)
	}
	if address == "" {
		svc := &corev1.Service{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: StoreName}, svc); err != nil {
			return nil, fmt.Errorf("read service %s/%s: %w", Namespace, StoreName, err)
		}
		ip := svc.Spec.ClusterIP
		if ip == "" || ip == corev1.ClusterIPNone {
			return nil, fmt.Errorf("service %s/%s has no cluster IP", Namespace, StoreName)
		}
		address = net.JoinHostPort(ip, strconv.Itoa(StorePort))
	}
	return NewStore(address, string(access), string(secret))
}

// NewStore reaches the store at address, host:port, over plain HTTP inside the cluster and never through a proxy.
func NewStore(address, accessKey, secretKey string) (*Store, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: dialTimeout}).DialContext
	c, err := minio.New(address, &minio.Options{
		Creds:        credentials.NewStaticV4(accessKey, secretKey, ""),
		Region:       StoreRegion,
		BucketLookup: minio.BucketLookupPath,
		Transport:    transport,
		MaxRetries:   maxRetries,
	})
	if err != nil {
		return nil, fmt.Errorf("create the CBOM store client: %w", err)
	}
	return &Store{client: c}, nil
}

// Ready checks that the store answers and has its bucket.
func (s *Store) Ready(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, Bucket)
	if err != nil {
		return fmt.Errorf("reach the CBOM store: %w", err)
	}
	if !ok {
		return fmt.Errorf("the CBOM store has no bucket %s", Bucket)
	}
	return nil
}

// Backup adds every object of the store to a backup through add, which takes a member's name, size and content, one
// member per object, and returns how many it added.
func (s *Store) Backup(ctx context.Context, add func(name string, size int64, r io.Reader) error) (int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	n := 0
	for listed := range s.client.ListObjects(ctx, Bucket, minio.ListObjectsOptions{Recursive: true}) {
		if listed.Err != nil {
			return n, fmt.Errorf("list the CBOM store: %w", listed.Err)
		}
		if err := s.backupObject(ctx, listed.Key, add); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *Store) backupObject(ctx context.Context, key string, add func(string, int64, io.Reader) error) error {
	obj, err := s.client.GetObject(ctx, Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("read a CBOM object: %w", err)
	}
	defer func() { _ = obj.Close() }()
	info, err := obj.Stat()
	if err != nil {
		return fmt.Errorf("read a CBOM object: %w", err)
	}
	o := Object{Key: key, Size: info.Size, ContentType: info.ContentType, Metadata: info.UserMetadata}
	name := MemberPrefix + base64.RawURLEncoding.EncodeToString([]byte(key))
	if why := objectProblem(o); why != "" {
		return fmt.Errorf("%w: the object %s %s, so a restore would refuse it", ErrInvalidMember, name, why)
	}
	header, err := json.Marshal(o)
	if err != nil {
		return err
	}
	header = append(header, '\n')
	return add(name, int64(len(header))+info.Size, io.MultiReader(bytes.NewReader(header), obj))
}

// Restore writes the object a backup's member name holds back into the store, with its content type and user
// metadata, replacing the object of that key. It returns the key, or ok false when name is not a CBOM member. The
// member is checked as CheckMember checks it; content after the object's size fails the write after the object was
// written, which a restore that verified the backup first never meets.
func (s *Store) Restore(ctx context.Context, name string, member io.Reader) (key string, ok bool, err error) {
	if !IsMember(name) {
		return "", false, nil
	}
	r := bufio.NewReaderSize(member, maxHeader)
	o, err := readHeader(name, r)
	if err != nil {
		return "", true, err
	}
	opts := minio.PutObjectOptions{ContentType: o.ContentType, UserMetadata: o.Metadata, PartSize: partSize}
	if _, err := s.client.PutObject(ctx, Bucket, o.Key, io.LimitReader(r, o.Size), o.Size, opts); err != nil {
		return "", true, fmt.Errorf("write a CBOM object: %w", err)
	}
	if err := atEnd(name, r); err != nil {
		return "", true, err
	}
	return o.Key, true, nil
}

// Prune deletes the objects of the store whose keys keep does not hold and returns how many it deleted.
func (s *Store) Prune(ctx context.Context, keep map[string]bool) (int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var extra []string
	for listed := range s.client.ListObjects(ctx, Bucket, minio.ListObjectsOptions{Recursive: true}) {
		if listed.Err != nil {
			return 0, fmt.Errorf("list the CBOM store: %w", listed.Err)
		}
		if !keep[listed.Key] {
			extra = append(extra, listed.Key)
		}
	}
	for _, key := range extra {
		if err := s.client.RemoveObject(ctx, Bucket, key, minio.RemoveObjectOptions{}); err != nil {
			return 0, fmt.Errorf("delete a CBOM object: %w", err)
		}
	}
	return len(extra), nil
}
