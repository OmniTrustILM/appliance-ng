package api

import (
	"crypto/x509"
	"maps"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/backup/target"
)

// targetSecrets are the secrets of a target update by Secret key: set holds the values given, drop the stored keys
// to remove. A secret left out or empty keeps the stored one.
type targetSecrets struct {
	set  map[string]string
	drop []string
}

// secretsOf collects the non-empty values of given, by Secret key.
func secretsOf(given map[string]*string) targetSecrets {
	out := targetSecrets{set: map[string]string{}}
	for key, value := range given {
		if value != nil && *value != "" {
			out.set[key] = *value
		}
	}
	return out
}

// parseBackupTarget validates a target update and returns the target, with the defaults filled in, and its secrets.
func parseBackupTarget(name string, body *BackupTargetUpdate) (v1alpha1.BackupTarget, targetSecrets, *fieldError) {
	if len(validation.IsDNS1123Label(name)) > 0 {
		return v1alpha1.BackupTarget{}, targetSecrets{}, &fieldError{"name", "Enter a name of lowercase letters, digits and hyphens, at most 63 characters."}
	}
	if body == nil || !body.Type.Valid() {
		return v1alpha1.BackupTarget{}, targetSecrets{}, &fieldError{"type", "Choose S3, SFTP or NFS."}
	}
	spec := v1alpha1.BackupTarget{
		Name: name, Type: string(body.Type), Schedule: strings.Join(strings.Fields(deref(body.Schedule)), " "),
		TimeZone: body.TimeZone, Retention: backup.DefaultRetention,
	}
	if spec.Schedule == "" {
		spec.Schedule = backup.DefaultSchedule
	}
	if ferr := checkSchedule(spec.Schedule, spec.TimeZone); ferr != nil {
		return spec, targetSecrets{}, ferr
	}
	if body.Retention != nil {
		if *body.Retention < 1 || *body.Retention > 365 {
			return spec, targetSecrets{}, &fieldError{"retention", "Keep between 1 and 365 backups on the target."}
		}
		spec.Retention = int32(*body.Retention) //nolint:gosec // checked to be at most 365 above
	}
	var given targetSecrets
	var ferr *fieldError
	switch spec.Type {
	case v1alpha1.BackupTypeS3:
		given, ferr = parseS3(&spec, body)
	case v1alpha1.BackupTypeSFTP:
		given, ferr = parseSFTP(&spec, body)
	case v1alpha1.BackupTypeNFS:
		ferr = parseNFS(&spec, body)
	}
	return spec, given, ferr
}

// checkSchedule refuses a schedule the scheduler would not parse and a zone that is not an IANA zone name.
func checkSchedule(schedule, zone string) *fieldError {
	if _, err := time.LoadLocation(zone); err != nil || zone == "" || zone == "Local" {
		return &fieldError{fieldTimeZone, "Choose an IANA time zone, such as Europe/Prague."}
	}
	if _, err := backup.ParseSchedule(schedule, zone); err != nil || len(schedule) > maxScheduleLength {
		return &fieldError{"schedule", "Enter five cron fields: minute, hour, day of month, month and day of week, such as 0 2 * * *."}
	}
	return nil
}

func parseS3(spec *v1alpha1.BackupTarget, body *BackupTargetUpdate) (targetSecrets, *fieldError) {
	b := body.S3
	if b == nil {
		return targetSecrets{}, &fieldError{"s3.endpoint", "Enter the S3 endpoint."}
	}
	if !validEndpoint(b.Endpoint) {
		return targetSecrets{}, &fieldError{"s3.endpoint", "Enter the endpoint as https://host[:port], without a path."}
	}
	if strings.TrimSpace(b.Bucket) == "" {
		return targetSecrets{}, &fieldError{"s3.bucket", "Enter the bucket."}
	}
	if b.Ca != nil && *b.Ca != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(*b.Ca)) {
		return targetSecrets{}, &fieldError{"s3.ca", "The CA bundle holds no PEM certificate."}
	}
	if ferr := secretLengths("s3", map[string]*string{"accessKey": b.AccessKey, "secretKey": b.SecretKey, "ca": b.Ca}); ferr != nil {
		return targetSecrets{}, ferr
	}
	spec.S3 = &v1alpha1.S3Target{Endpoint: strings.TrimSpace(b.Endpoint), Bucket: strings.TrimSpace(b.Bucket),
		Prefix: strings.Trim(deref(b.Prefix), " /"), Region: strings.TrimSpace(deref(b.Region))}
	secrets := secretsOf(map[string]*string{target.AccessKeyKey: b.AccessKey, target.SecretKeyKey: b.SecretKey, target.CAKey: b.Ca})
	if b.RemoveCa != nil && *b.RemoveCa {
		if secrets.set[target.CAKey] != "" {
			return targetSecrets{}, &fieldError{"s3.ca", "Upload a CA bundle or remove the stored one, not both."}
		}
		secrets.drop = []string{target.CAKey}
	}
	return secrets, nil
}

// validEndpoint accepts what the S3 driver accepts: a host with an optional port and an http or https scheme.
func validEndpoint(endpoint string) bool {
	endpoint = strings.TrimSpace(endpoint)
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	return err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http") && strings.Trim(u.Path, "/") == "" &&
		u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func parseSFTP(spec *v1alpha1.BackupTarget, body *BackupTargetUpdate) (targetSecrets, *fieldError) {
	b := body.Sftp
	if b == nil || !validHost(b.Host) {
		return targetSecrets{}, &fieldError{"sftp.host", "Enter the server's host name or IP address."}
	}
	port := defaultSSHPort
	if b.Port != nil {
		port = *b.Port
	}
	switch {
	case port < 1 || port > 65535:
		return targetSecrets{}, &fieldError{"sftp.port", "Enter a port between 1 and 65535."}
	case strings.TrimSpace(b.Path) == "":
		return targetSecrets{}, &fieldError{"sftp.path", "Enter the directory on the server."}
	case b.User == "" || !printable(b.User):
		return targetSecrets{}, &fieldError{"sftp.user", "Enter a user name of printable characters."}
	case !strings.HasPrefix(b.HostKey, "SHA256:") || len(b.HostKey) == len("SHA256:"):
		return targetSecrets{}, &fieldError{"sftp.hostKey", "Read the server's host key and accept it."}
	case deref(b.Password) != "" && deref(b.PrivateKey) != "":
		return targetSecrets{}, &fieldError{"sftp.privateKey", "Enter a password or a private key, not both."}
	}
	if key := deref(b.PrivateKey); key != "" {
		if _, err := ssh.ParsePrivateKey([]byte(key)); err != nil {
			return targetSecrets{}, &fieldError{"sftp.privateKey", "Enter an OpenSSH or PEM private key without a passphrase."}
		}
	}
	if ferr := secretLengths("sftp", map[string]*string{"password": b.Password, "privateKey": b.PrivateKey}); ferr != nil {
		return targetSecrets{}, ferr
	}
	spec.SFTP = &v1alpha1.SFTPTarget{Host: strings.TrimSpace(b.Host), Port: int32(port), Path: strings.TrimSpace(b.Path), //nolint:gosec // a port
		User: b.User, HostKey: b.HostKey}
	// One SSH credential is used, so a new one replaces the other.
	secrets := secretsOf(map[string]*string{target.PasswordKey: b.Password, target.PrivateKeyKey: b.PrivateKey})
	switch {
	case secrets.set[target.PasswordKey] != "":
		secrets.drop = []string{target.PrivateKeyKey}
	case secrets.set[target.PrivateKeyKey] != "":
		secrets.drop = []string{target.PasswordKey}
	}
	return secrets, nil
}

func parseNFS(spec *v1alpha1.BackupTarget, body *BackupTargetUpdate) *fieldError {
	b := body.Nfs
	if b == nil || !validHost(b.Server) {
		return &fieldError{"nfs.server", "Enter the NFS server's host name or IP address."}
	}
	if !strings.HasPrefix(b.Export, "/") {
		return &fieldError{"nfs.export", "Enter the export path, starting with /."}
	}
	version := defaultNFSVersion
	if b.Version != nil {
		version = string(*b.Version)
	}
	if !NFSSettingsVersion(version).Valid() {
		return &fieldError{"nfs.version", "Choose NFS version 3, 4.1 or 4.2."}
	}
	spec.NFS = &v1alpha1.NFSTarget{Server: strings.TrimSpace(b.Server), Export: b.Export, Path: strings.Trim(deref(b.Path), " /"), Version: version}
	return nil
}

// secretLengths refuses a secret longer than maxSecretLength; the field is block.key.
func secretLengths(block string, secrets map[string]*string) *fieldError {
	for _, key := range slices.Sorted(maps.Keys(secrets)) {
		if v := secrets[key]; v != nil && len(*v) > maxSecretLength {
			return &fieldError{block + "." + key, "The value is too long."}
		}
	}
	return nil
}

// validHost accepts a host name or an IP address without a port.
func validHost(host string) bool {
	host = strings.TrimSpace(host)
	if net.ParseIP(host) != nil {
		return true
	}
	return host != "" && len(validation.IsDNS1123Subdomain(strings.ToLower(host))) == 0
}
