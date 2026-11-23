package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ApplianceName is the only name the Appliance resource may have.
const ApplianceName = "appliance"

// Condition types reported in ApplianceStatus.Conditions.
const (
	ConditionNodeConfigured   = "NodeConfigured"
	ConditionAddonsReady      = "AddonsReady"
	ConditionPlatformReady    = "PlatformReady"
	ConditionCertificateReady = "CertificateReady"
	ConditionHandoverComplete = "HandoverComplete"
	ConditionConnectorsReady  = "ConnectorsReady"
	ConditionBackupHealthy    = "BackupHealthy"
)

// Setup phases reported in ApplianceStatus.Setup.
const (
	SetupPart1    = "part1"
	SetupPart2    = "part2"
	SetupComplete = "complete"
)

// TLS sources of TLSSpec.Source.
const (
	TLSSourceBuiltin = "builtin"
	TLSSourceSecret  = "secret"
)

// Mail transport security modes of MailSpec.Security.
const (
	MailSecurityNone     = "none"
	MailSecuritySTARTTLS = "starttls"
	MailSecurityTLS      = "tls"
)

// Backup target types of BackupTarget.Type.
const (
	BackupTypeS3   = "s3"
	BackupTypeSFTP = "sftp"
	BackupTypeNFS  = "nfs"
)

// Database states of BackupRecord.Database.
const (
	BackupDatabaseIncluded    = "included"
	BackupDatabaseNotIncluded = "notIncluded"
)

// ApplianceSpec is the desired configuration of the appliance.
type ApplianceSpec struct {
	Node NodeSpec `json:"node"`
	// +optional
	DNS DNSSpec `json:"dns,omitempty"`
	// +optional
	NTP NTPSpec `json:"ntp,omitempty"`
	// +optional
	Proxy *ProxySpec `json:"proxy,omitempty"`
	// +optional
	TLS TLSSpec `json:"tls,omitempty"`
	// Trust references the Secret key holding the PEM bundle of additional trusted CAs.
	// +optional
	Trust *SecretKeyRef `json:"trust,omitempty"`
	// +optional
	Registries []RegistrySpec `json:"registries,omitempty"`
	// +optional
	Mail *MailSpec `json:"mail,omitempty"`
	// +optional
	Platform *PlatformSpec `json:"platform,omitempty"`
	// +optional
	Backup *BackupSpec `json:"backup,omitempty"`
}

// NodeSpec is the network identity of the node.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.address) || size(oldSelf.address) == 0 || (has(self.address) && self.address == oldSelf.address)",message="node.address cannot be changed once set"
type NodeSpec struct {
	// HostName is the fully qualified host name of the appliance. It is the Kubernetes node name, so it is fixed
	// once set.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)+$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="node.hostName cannot be changed once set"
	HostName string `json:"hostName"`
	// Address is the node address in CIDR notation; empty means DHCP.
	// +optional
	// +kubebuilder:validation:MaxLength=49
	// +kubebuilder:validation:XValidation:rule="self == '' || (isCIDR(self) && cidr(self).ip().family() == 4)",message="node.address must be empty or an IPv4 CIDR"
	Address string `json:"address,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=45
	// +kubebuilder:validation:XValidation:rule="self == '' || (isIP(self) && ip(self).family() == 4)",message="node.gateway must be empty or an IPv4 address"
	Gateway string `json:"gateway,omitempty"`
}

// DNSSpec lists the DNS servers.
type DNSSpec struct {
	// +optional
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=45
	// +kubebuilder:validation:items:XValidation:rule="isIP(self)",message="dns.servers entries must be IP addresses"
	Servers []string `json:"servers,omitempty"`
}

// NTPSpec lists the NTP servers.
type NTPSpec struct {
	// +optional
	Servers []string `json:"servers,omitempty"`
}

// ProxySpec is the outbound HTTP proxy configuration.
type ProxySpec struct {
	// +optional
	HTTP string `json:"http,omitempty"`
	// +optional
	HTTPS string `json:"https,omitempty"`
	// +optional
	NoProxy string `json:"noProxy,omitempty"`
}

// TLSSpec selects the certificate served by the platform.
// +kubebuilder:validation:XValidation:rule="self.source != 'secret' || (has(self.secretRef) && size(self.secretRef) > 0)",message="tls.secretRef is required when tls.source is secret"
type TLSSpec struct {
	// +kubebuilder:validation:Enum=builtin;secret
	// +kubebuilder:default=builtin
	// +optional
	Source string `json:"source,omitempty"`
	// SecretRef names the Secret holding the certificate when Source is secret.
	// +optional
	SecretRef string `json:"secretRef,omitempty"`
}

// SecretKeyRef references a key of a Secret.
type SecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// Keys of the credentials Secrets that RegistrySpec.CredentialsRef and MailSpec.CredentialsRef name.
const (
	CredentialsUsernameKey = "username"
	CredentialsPasswordKey = "password"
)

// RegistrySpec is a container registry with optional credentials.
type RegistrySpec struct {
	Host string `json:"host"`
	// CredentialsRef names the Secret holding the registry credentials.
	// +optional
	CredentialsRef string `json:"credentialsRef,omitempty"`
}

// MailSpec is the outbound SMTP configuration.
type MailSpec struct {
	Host string `json:"host"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// +kubebuilder:validation:Enum=none;starttls;tls
	Security string `json:"security"`
	From     string `json:"from"`
	// CredentialsRef names the Secret holding the SMTP credentials.
	// +optional
	CredentialsRef string `json:"credentialsRef,omitempty"`
}

// PlatformSpec selects the ILM platform to install.
type PlatformSpec struct {
	// +optional
	ILMVersion string `json:"ilmVersion,omitempty"`
	// +optional
	HostName string `json:"hostName,omitempty"`
	// +optional
	// +listType=set
	Components []string `json:"components,omitempty"`
}

// BackupSpec configures the targets that receive copies of the backups.
type BackupSpec struct {
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	Targets []BackupTarget `json:"targets,omitempty"`
}

// BackupTarget is a place that receives scheduled backups. Exactly the block matching Type is set.
// +kubebuilder:validation:XValidation:rule="self.type == 's3' ? has(self.s3) : !has(self.s3)",message="backup target s3 must be set exactly when type is s3"
// +kubebuilder:validation:XValidation:rule="self.type == 'sftp' ? has(self.sftp) : !has(self.sftp)",message="backup target sftp must be set exactly when type is sftp"
// +kubebuilder:validation:XValidation:rule="self.type == 'nfs' ? has(self.nfs) : !has(self.nfs)",message="backup target nfs must be set exactly when type is nfs"
type BackupTarget struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:Enum=s3;sftp;nfs
	Type string `json:"type"`
	// Schedule is a five-field cron expression; empty selects the default schedule.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^(\s*\S+(\s+\S+){4}\s*)?$`
	Schedule string `json:"schedule,omitempty"`
	// TimeZone is the IANA time zone of Schedule; empty selects the default zone.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	TimeZone string `json:"timeZone,omitempty"`
	// Retention is the number of backups kept on the target.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=365
	Retention int32 `json:"retention"`
	// +optional
	S3 *S3Target `json:"s3,omitempty"`
	// +optional
	SFTP *SFTPTarget `json:"sftp,omitempty"`
	// +optional
	NFS *NFSTarget `json:"nfs,omitempty"`
	// CredentialsRef names the Secret in the appliance namespace holding the target credentials: accessKey and
	// secretKey for s3, password or privateKey for sftp.
	// +optional
	CredentialsRef string `json:"credentialsRef,omitempty"`
}

// S3Target is an S3 compatible bucket.
type S3Target struct {
	Endpoint string `json:"endpoint"`
	Bucket   string `json:"bucket"`
	// +optional
	Prefix string `json:"prefix,omitempty"`
	// +optional
	Region string `json:"region,omitempty"`
	// CA references the Secret key holding the PEM CA bundle that signs the endpoint certificate.
	// +optional
	CA *SecretKeyRef `json:"ca,omitempty"`
}

// SFTPTarget is a directory on an SFTP server.
type SFTPTarget struct {
	Host string `json:"host"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32  `json:"port"`
	Path string `json:"path"`
	User string `json:"user"`
	// HostKey is the SHA256 fingerprint of the server's host key, as printed by ssh-keygen -l.
	// +kubebuilder:validation:Pattern=`^SHA256:.+$`
	HostKey string `json:"hostKey"`
}

// NFSTarget is a directory on an NFS export.
type NFSTarget struct {
	Server string `json:"server"`
	Export string `json:"export"`
	// +optional
	Path string `json:"path,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum="3";"4.1";"4.2"
	Version string `json:"version,omitempty"`
}

// Versions are the component versions running on the appliance.
type Versions struct {
	// +optional
	Appliance string `json:"appliance,omitempty"`
	// +optional
	Talos string `json:"talos,omitempty"`
	// +optional
	Kubernetes string `json:"kubernetes,omitempty"`
	// +optional
	Operator string `json:"operator,omitempty"`
	// +optional
	ILM string `json:"ilm,omitempty"`
}

// ApplianceStatus is the observed state of the appliance.
type ApplianceStatus struct {
	// +kubebuilder:validation:Enum=part1;part2;complete
	// +optional
	Setup string `json:"setup,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	Versions Versions `json:"versions,omitempty"`
	// +optional
	NodeAddress string `json:"nodeAddress,omitempty"`
	// +optional
	RebootRequired bool `json:"rebootRequired,omitempty"`
	// DataVolume is the size and free space of the data volume; unset while the manager cannot read it.
	// +optional
	DataVolume *VolumeStatus `json:"dataVolume,omitempty"`
	// RecoveryKit describes the latest recovery kit; unset until one is issued.
	// +optional
	RecoveryKit *RecoveryKitStatus `json:"recoveryKit,omitempty"`
	// Backup reports the local backups and the state of every target.
	// +optional
	Backup *BackupStatus `json:"backup,omitempty"`
	// LastRestoreTest is when a restore test last succeeded.
	// +optional
	LastRestoreTest *metav1.Time `json:"lastRestoreTest,omitempty"`
}

// BackupStatus is the observed state of the backups.
type BackupStatus struct {
	// +optional
	Running bool `json:"running,omitempty"`
	// Last is the most recent backup taken.
	// +optional
	Last *BackupRecord `json:"last,omitempty"`
	// Local lists the backup copies kept on the data volume.
	// +optional
	Local []BackupRecord `json:"local,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=name
	Targets []TargetStatus `json:"targets,omitempty"`
}

// BackupRecord is one backup file.
type BackupRecord struct {
	Name      string      `json:"name"`
	Time      metav1.Time `json:"time"`
	SizeBytes int64       `json:"sizeBytes"`
	// +kubebuilder:validation:Enum=included;notIncluded
	Database string `json:"database"`
}

// TargetStatus is the state of a backup target.
type TargetStatus struct {
	Name string `json:"name"`
	// +optional
	LastSuccess *metav1.Time `json:"lastSuccess,omitempty"`
	// +optional
	LastAttempt *metav1.Time `json:"lastAttempt,omitempty"`
	// FirstAttempt is the first copy attempted to the target, from which a target without a success is overdue.
	// +optional
	FirstAttempt *metav1.Time `json:"firstAttempt,omitempty"`
	// LastError is a fixed reason, never an error text of the target.
	// +optional
	// +kubebuilder:validation:Enum=unreachable;authFailed;hostKeyMismatch;notFound;noSpace;timeout;failed
	LastError string `json:"lastError,omitempty"`
}

// RecoveryKitStatus describes the latest recovery kit, never its key or passphrase.
type RecoveryKitStatus struct {
	// +optional
	IssuedAt *metav1.Time `json:"issuedAt,omitempty"`
	// Confirmed is set once the administrator confirms that the latest kit is stored safely.
	// +optional
	Confirmed bool `json:"confirmed,omitempty"`
	// TalosconfigNotAfter is when the kit's talosconfig expires.
	// +optional
	TalosconfigNotAfter *metav1.Time `json:"talosconfigNotAfter,omitempty"`
	// Recipient is the public key the backups are encrypted to.
	// +optional
	Recipient string `json:"recipient,omitempty"`
	// Issuance identifies the kit among the kits issued with the recipient.
	// +optional
	Issuance string `json:"issuance,omitempty"`
}

// VolumeStatus is the size and free space of a volume, in bytes. FreeBytes is rounded down to coarse steps.
type VolumeStatus struct {
	SizeBytes int64 `json:"sizeBytes"`
	FreeBytes int64 `json:"freeBytes"`
	// LowSpace is set while less than a fifth of the volume is free, judged on the exact free space.
	// +optional
	LowSpace bool `json:"lowSpace,omitempty"`
}

// Appliance is the single cluster-scoped resource that describes the whole appliance.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'appliance'",message="the Appliance must be named appliance"
type Appliance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ApplianceSpec   `json:"spec"`
	Status ApplianceStatus `json:"status,omitempty"`
}

// ApplianceList is a list of Appliance resources.
// +kubebuilder:object:root=true
type ApplianceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Appliance `json:"items"`
}
