package v1alpha1

import (
	"time"

	corev1 "k8s.io/api/core/v1"
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
	// ConditionILMSettingsApplied reports whether the settings the manager keeps in ILM, such as the external OIDC
	// provider, are written.
	ConditionILMSettingsApplied = "ILMSettingsApplied"
	// ConditionCBOMRepositoryApplied reports whether the CBOM repository and its store are applied; it is absent
	// while the repository was never chosen.
	ConditionCBOMRepositoryApplied = "CBOMRepositoryApplied"
	// ConditionAdminCertificateRegistered reports whether ILM's administrator holds the certificate the console hands
	// out; it is absent while there is none or ILM has no administrator to give it to.
	ConditionAdminCertificateRegistered = "AdminCertificateRegistered"
	// ConditionClusterHealthy reports whether every node of the cluster is up and in step.
	ConditionClusterHealthy = "ClusterHealthy"
)

// Reasons of ClusterHealthy while it is false.
const (
	ReasonNodeDown       = "nodeDown"
	ReasonTwoVotingNodes = "twoVotingNodes"
	ReasonEtcdUnhealthy  = "etcdUnhealthy"
	ReasonCopyBehind     = "copyBehind"
	ReasonConfigPending  = "configPending"
)

// ReasonUpdateHeld is the reason of AddonsReady while a system update holds the add-on pass: the add-ons are
// checked, not applied.
const ReasonUpdateHeld = "UpdateHeld"

// Setup phases reported in ApplianceStatus.Setup.
const (
	SetupPart1    = "part1"
	SetupPart2    = "part2"
	SetupComplete = "complete"
	// SetupRestore is a new appliance that restores a backup instead of running wizard part 2.
	SetupRestore = "restore"
)

// TLS sources of TLSSpec.Source.
const (
	TLSSourceBuiltin = "builtin"
	TLSSourceSecret  = "secret"
	TLSSourceACME    = "acme"
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
	Cluster ClusterSpec `json:"cluster"`
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

// FirstNode returns the first node of the cluster, the appliance that was set up first, or nil when there is none.
func (s *ApplianceSpec) FirstNode() *ClusterNode {
	if len(s.Cluster.Nodes) == 0 {
		return nil
	}
	return &s.Cluster.Nodes[0]
}

// NodeNamed returns the node of the cluster with the host name name, or nil when there is none.
func (s *ApplianceSpec) NodeNamed(name string) *ClusterNode {
	for i := range s.Cluster.Nodes {
		if s.Cluster.Nodes[i].HostName == name {
			return &s.Cluster.Nodes[i]
		}
	}
	return nil
}

// HostName returns the host name of the first node, or "" when there is none.
func (s *ApplianceSpec) HostName() string {
	if n := s.FirstNode(); n != nil {
		return n.HostName
	}
	return ""
}

// ClusterSpec describes the nodes of the appliance and the address the cluster is reached at.
type ClusterSpec struct {
	// +optional
	Address ClusterAddress `json:"address,omitempty"`
	// Nodes are the nodes of the cluster, the first node first.
	// +listType=map
	// +listMapKey=hostName
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=7
	// +kubebuilder:validation:XValidation:rule="self.all(a, self.exists_one(b, b.hostName.split('.')[0] == a.hostName.split('.')[0]))",message="node host names must differ in their first label, which is the Kubernetes node name"
	Nodes []ClusterNode `json:"nodes"`
}

// ClusterAddress is the address the cluster is reached at: a virtual IPv4 address the nodes share, or a DNS name
// that resolves to the nodes. Neither set is the address of the first node.
// +kubebuilder:validation:XValidation:rule="!has(self.virtual) || size(self.virtual) == 0 || !has(self.external) || size(self.external) == 0",message="cluster.address takes virtual or external, not both"
type ClusterAddress struct {
	// +optional
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:XValidation:rule="self == '' || (isIP(self) && ip(self).family() == 4)",message="cluster.address.virtual must be an IPv4 address"
	Virtual string `json:"virtual,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)+)?$`
	External string `json:"external,omitempty"`
}

// NodeRole is the part a node plays in the cluster.
// +kubebuilder:validation:Enum=voting;worker
type NodeRole string

// Roles of ClusterNode.Role.
const (
	// NodeRoleVoting is a control-plane node with an etcd member that also runs workloads.
	NodeRoleVoting NodeRole = "voting"
	// NodeRoleWorker runs workloads only.
	NodeRoleWorker NodeRole = "worker"
)

// ClusterNode is the network identity, role and placement tags of one node. The host name names the entry; the
// address is fixed once set.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.address) || size(oldSelf.address) == 0 || (has(self.address) && self.address == oldSelf.address)",message="a node's address cannot be changed once set"
type ClusterNode struct {
	// HostName is the fully qualified host name of the node. Talos registers the node in Kubernetes under the
	// first label of this name.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)+$`
	HostName string `json:"hostName"`
	// Address is the node address in CIDR notation; empty means DHCP.
	// +optional
	// +kubebuilder:validation:MaxLength=49
	// +kubebuilder:validation:XValidation:rule="self == '' || (isCIDR(self) && cidr(self).ip().family() == 4)",message="node address must be empty or an IPv4 CIDR"
	Address string `json:"address,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=45
	// +kubebuilder:validation:XValidation:rule="self == '' || (isIP(self) && ip(self).family() == 4)",message="node gateway must be empty or an IPv4 address"
	Gateway string `json:"gateway,omitempty"`
	// Role is set by the manager when the node joins.
	// +optional
	Role NodeRole `json:"role,omitempty"`
	// Tags are short names that place connectors on the node.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9][a-z0-9-]{0,30}$`
	Tags []string `json:"tags,omitempty"`
	// Maintenance marks the node as taken out of service by the administrator.
	// +optional
	Maintenance bool `json:"maintenance,omitempty"`
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
	// +kubebuilder:validation:Enum=builtin;secret;acme
	// +kubebuilder:default=builtin
	// +optional
	Source string `json:"source,omitempty"`
	// SecretRef names the Secret holding the certificate when Source is secret.
	// +optional
	SecretRef string `json:"secretRef,omitempty"`
	// ACME configures the automatic certificate when Source is acme.
	// +optional
	ACME *ACMESpec `json:"acme,omitempty"`
}

// Solvers of ACMESolver.Type.
const (
	ACMESolverHTTP01 = "http01"
	ACMESolverDNS01  = "dns01"
)

// ACMESpec is the ACME server the platform certificate is requested from: the one at DirectoryURL, or ILM through
// ILMProfile. Exactly one of them is set.
type ACMESpec struct {
	// DirectoryURL is the https directory of a custom ACME server.
	// +optional
	DirectoryURL string `json:"directoryUrl,omitempty"`
	// Email is the contact of the ACME account.
	// +optional
	Email string `json:"email,omitempty"`
	// EABKeyID is the key ID of the external account binding.
	// +optional
	EABKeyID string `json:"eabKeyId,omitempty"`
	// EABSecretRef names the Secret that holds the binding's HMAC key under the key hmac.
	// +optional
	EABSecretRef *corev1.LocalObjectReference `json:"eabSecretRef,omitempty"`
	// CABundle is the PEM bundle that signs the ACME server's certificate.
	// +optional
	CABundle []byte `json:"caBundle,omitempty"`
	// Solver proves control of the host names; ILMProfile ignores it.
	// +optional
	Solver ACMESolver `json:"solver,omitempty"`
	// ILMProfile requests the certificate from the ACME profile of this appliance's ILM.
	// +optional
	ILMProfile *ILMACMEProfile `json:"ilmProfile,omitempty"`
}

// ACMESolver is the challenge that proves control of a host name.
type ACMESolver struct {
	// +kubebuilder:validation:Enum=http01;dns01
	// +kubebuilder:default=http01
	// +optional
	Type string `json:"type,omitempty"`
	// DNS01 configures the DNS provider when Type is dns01.
	// +optional
	DNS01 *DNS01Solver `json:"dns01,omitempty"`
}

// DNS01Solver is a DNS provider that answers the DNS-01 challenge.
type DNS01Solver struct {
	// Provider is one of cert-manager's DNS-01 providers: rfc2136, cloudflare, route53, azuredns, clouddns,
	// digitalocean, acmedns or akamai.
	Provider string `json:"provider"`
	// Config holds the provider's settings that are not secret.
	// +optional
	Config map[string]string `json:"config,omitempty"`
	// CredentialsSecretRef names the Secret that holds the provider's secret settings.
	// +optional
	CredentialsSecretRef *corev1.LocalObjectReference `json:"credentialsSecretRef,omitempty"`
}

// ILMACMEProfile is the ACME profile of ILM that issues the certificate.
type ILMACMEProfile struct {
	RAProfileUUID    string `json:"raProfileUuid"`
	VaultUUID        string `json:"vaultUuid"`
	VaultProfileUUID string `json:"vaultProfileUuid"`
	// ProfileName is the name of the ACME profile in ILM.
	// +kubebuilder:default=appliance
	// +optional
	ProfileName string `json:"profileName,omitempty"`
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
	// +listType=map
	// +listMapKey=name
	Components []ComponentSpec `json:"components,omitempty"`
	// Database is the external database ILM uses; unset is the managed one.
	// +optional
	Database *ExternalDatabase `json:"database,omitempty"`
	// Messaging is the external message broker ILM uses; unset is the managed one.
	// +optional
	Messaging *ExternalMessaging `json:"messaging,omitempty"`
	// Identity is the external OIDC provider ILM signs users in with; unset is the managed Keycloak.
	// +optional
	Identity *ExternalIdentity `json:"identity,omitempty"`
	// Utils installs the utils service.
	// +optional
	Utils bool `json:"utils,omitempty"`
	// CBOM installs the CBOM repository ILM keeps CBOMs in.
	// +optional
	CBOM *CBOMSpec `json:"cbom,omitempty"`
}

// ComponentSpec is a platform component the administrator chose.
type ComponentSpec struct {
	Name string `json:"name"`
	// NodeTag places a connector on the nodes with this tag; empty runs it on any node.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]{0,30}$`
	NodeTag string `json:"nodeTag,omitempty"`
}

// ComponentNames returns the names of the chosen components, in order.
func (p *PlatformSpec) ComponentNames() []string {
	names := make([]string, len(p.Components))
	for i, c := range p.Components {
		names[i] = c.Name
	}
	return names
}

// Stores of CBOMSpec.Storage.
const (
	CBOMStorageAppliance = "appliance"
	CBOMStorageS3        = "s3"
)

// Keys of the credentials Secret that CBOMSpec.CredentialsRef names.
const (
	CredentialsAccessKey = "accessKey"
	CredentialsSecretKey = "secretKey"
)

// CBOMSpec is the CBOM repository. Turned off, the repository goes; the store on the appliance keeps running with the
// CBOMs stored before, which backups still hold.
// +kubebuilder:validation:XValidation:rule="self.storage == 's3' ? has(self.s3) && has(self.credentialsRef) && size(self.credentialsRef) > 0 : !has(self.s3) && (!has(self.credentialsRef) || size(self.credentialsRef) == 0)",message="cbom s3 and credentialsRef must be set exactly when storage is s3"
type CBOMSpec struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Storage is the S3 store of the CBOMs: one on the appliance's data volume, or an external one.
	// +kubebuilder:validation:Enum=appliance;s3
	Storage string `json:"storage"`
	// S3 is the external bucket; its Prefix is not used.
	// +optional
	S3 *S3Target `json:"s3,omitempty"`
	// CredentialsRef names the Secret in the appliance namespace holding accessKey and secretKey of the external
	// bucket.
	// +optional
	CredentialsRef string `json:"credentialsRef,omitempty"`
}

// CredentialsClientSecretKey is the key of the Secret that ExternalIdentity.CredentialsRef names; the Secrets of
// ExternalDatabase and ExternalMessaging use CredentialsUsernameKey and CredentialsPasswordKey.
const CredentialsClientSecretKey = "clientSecret"

// Keys of the Secret that ExternalIdentity.CredentialsRef names that hold the issuer and the client ID its client
// secret was given for; the manager sends the client secret to ILM only with that issuer and client ID.
const (
	CredentialsIssuerKey   = "issuer"
	CredentialsClientIDKey = "clientID"
)

// TLS modes of ExternalDatabase.TLSMode, as libpq and pgjdbc name them.
const (
	DatabaseTLSDisable    = "disable"
	DatabaseTLSRequire    = "require"
	DatabaseTLSVerifyFull = "verify-full"
)

// CAKey is the key of the PEM CA bundle in the Secret that ExternalDatabase.CA and ExternalMessaging.CA name.
const CAKey = "ca.crt"

// ExternalDatabase is a PostgreSQL database outside the appliance.
// +kubebuilder:validation:XValidation:rule="!has(self.tlsMode) || self.tlsMode != 'verify-full' || has(self.ca)",message="database.ca is required when tlsMode is verify-full"
type ExternalDatabase struct {
	// Host is a host name or an IPv4 address.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._-]{1,253}$`
	Host string `json:"host"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// Name is the database; its characters are safe in a JDBC URL.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_$.-]{1,63}$`
	Name string `json:"name"`
	// CredentialsRef names the Secret in the appliance namespace holding the user name and the password.
	// +kubebuilder:validation:MinLength=1
	CredentialsRef string `json:"credentialsRef"`
	// TLSMode is the sslmode of the connection; empty leaves the drivers' default, which uses TLS when the server
	// offers it without verifying the server.
	// +optional
	// +kubebuilder:validation:Enum=disable;require;verify-full
	TLSMode string `json:"tlsMode,omitempty"`
	// CA references the PEM CA bundle that signs the server's certificate; verify-full needs it.
	// +optional
	CA *SecretKeyRef `json:"ca,omitempty"`
}

// ExternalMessaging is a RabbitMQ broker outside the appliance.
// +kubebuilder:validation:XValidation:rule="(has(self.tls) && self.tls) == has(self.ca)",message="messaging.ca is required exactly when tls is set"
type ExternalMessaging struct {
	// Host is a host name or an IPv4 address.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._-]{1,253}$`
	Host string `json:"host"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// +optional
	// +kubebuilder:validation:MaxLength=255
	VirtualHost string `json:"virtualHost,omitempty"`
	// CredentialsRef names the Secret in the appliance namespace holding the user name and the password.
	// +kubebuilder:validation:MinLength=1
	CredentialsRef string `json:"credentialsRef"`
	// TLS connects with AMQPS to Port, verifying the server against CA.
	// +optional
	TLS bool `json:"tls,omitempty"`
	// CA references the PEM CA bundle that signs the broker's certificate; TLS needs it.
	// +optional
	CA *SecretKeyRef `json:"ca,omitempty"`
}

// ExternalIdentity is an OIDC provider outside the appliance.
type ExternalIdentity struct {
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://`
	Issuer string `json:"issuer"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	ClientID string `json:"clientId"`
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=255
	Scopes []string `json:"scopes,omitempty"`
	// Audiences are the audiences ILM accepts in the provider's tokens; none accepts any token of the issuer.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=255
	Audiences []string `json:"audiences,omitempty"`
	// CredentialsRef names the Secret in the appliance namespace holding the client secret.
	// +kubebuilder:validation:MinLength=1
	CredentialsRef string `json:"credentialsRef"`
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
	// PlatformBundle is the version of the platform bundle that is running.
	// +optional
	PlatformBundle string `json:"platformBundle,omitempty"`
	// RequiredManager is the version of the release a system update installed once it passed its point of no return.
	// The manager of any other version holds the add-on pass: its add-ons are not for the upgraded Kubernetes. It is
	// kept across later updates and restores in place.
	// +optional
	RequiredManager string `json:"requiredManager,omitempty"`
}

// ApplianceStatus is the observed state of the appliance.
type ApplianceStatus struct {
	// +kubebuilder:validation:Enum=part1;part2;complete;restore
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
	// Restore is the latest restore of a backup; unset until one starts.
	// +optional
	Restore *RestoreStatus `json:"restore,omitempty"`
	// Connectors is the state of every chosen connector, with the problem of a failed one.
	// +listType=map
	// +listMapKey=name
	// +optional
	Connectors []ConnectorState `json:"connectors,omitempty"`
	// Updates lists the bundles that can be loaded and the ones loaded; unset until the first check or load.
	// +optional
	Updates *UpdatesStatus `json:"updates,omitempty"`
	// Update is the latest update or rollback; unset until one starts.
	// +optional
	Update *UpdateStatus `json:"update,omitempty"`
	// Cluster is the observed state of the nodes; unset until the manager first reads it.
	// +optional
	Cluster *ClusterStatus `json:"cluster,omitempty"`
}

// ClusterStatus is the observed state of the cluster's nodes and of the nodes that hold its shared roles, each by host
// name.
type ClusterStatus struct {
	// +listType=map
	// +listMapKey=hostName
	// +optional
	Nodes []NodeStatus `json:"nodes,omitempty"`
	// AddressHolder is the node that holds the virtual address.
	// +optional
	AddressHolder string `json:"addressHolder,omitempty"`
	// Leader is the node that runs the leading manager.
	// +optional
	Leader string `json:"leader,omitempty"`
	// DatabasePrimary is the node that runs the PostgreSQL primary.
	// +optional
	DatabasePrimary string `json:"databasePrimary,omitempty"`
}

// NodeStatus is the observed state of one node.
type NodeStatus struct {
	HostName string `json:"hostName"`
	// +optional
	Role NodeRole `json:"role,omitempty"`
	// +optional
	Address string `json:"address,omitempty"`
	// Ready is the node's Kubernetes Ready condition.
	// +optional
	Ready bool `json:"ready,omitempty"`
	// EtcdHealthy tells whether the node's etcd member is healthy; a worker has none.
	// +optional
	EtcdHealthy bool `json:"etcdHealthy,omitempty"`
	// +optional
	Talos string `json:"talos,omitempty"`
	// +optional
	Kubernetes string `json:"kubernetes,omitempty"`
	// +optional
	CPUs int32 `json:"cpus,omitempty"`
	// +optional
	MemoryBytes int64 `json:"memoryBytes,omitempty"`
	// DataVolume is the size and free space of the node's data volume; unset while it cannot be read.
	// +optional
	DataVolume *VolumeStatus `json:"dataVolume,omitempty"`
	// +optional
	// +listType=set
	Tags []string `json:"tags,omitempty"`
}

// UpdatesStatus is the state of the update bundles.
type UpdatesStatus struct {
	// LastCheck is when the online source was last checked.
	// +optional
	LastCheck *metav1.Time `json:"lastCheck,omitempty"`
	// LastCheckError is a fixed reason, never an error text of the source; unset when the last check succeeded.
	// +optional
	LastCheckError string `json:"lastCheckError,omitempty"`
	// Available are the bundles found online that are newer than what runs.
	// +optional
	Available []BundleInfo `json:"available,omitempty"`
	// Loaded are the bundles on the data volume.
	// +optional
	Loaded []BundleInfo `json:"loaded,omitempty"`
}

// BundleInfo describes one update bundle.
type BundleInfo struct {
	// +kubebuilder:validation:Enum=system;platform
	Kind    string `json:"kind"`
	Version string `json:"version"`
	Arch    string `json:"arch"`
	// ILM, Operator and MinAppliance are set for a platform bundle.
	// +optional
	ILM string `json:"ilm,omitempty"`
	// +optional
	Operator string `json:"operator,omitempty"`
	// +optional
	MinAppliance string `json:"minAppliance,omitempty"`
	// OperatorRange is set for a system bundle.
	// +optional
	OperatorRange string `json:"operatorRange,omitempty"`
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// +optional
	LoadedAt *metav1.Time `json:"loadedAt,omitempty"`
	// Blocked is a fixed reason the bundle cannot be installed now; unset when it can.
	// +optional
	// +kubebuilder:validation:Enum=olderThanRunning;operatorOutOfRange;needsAppliance;wrongArch;sameVersion;invalidMetadata
	Blocked string `json:"blocked,omitempty"`
	// Needs is the version that must be installed first.
	// +optional
	Needs string `json:"needs,omitempty"`
}

// UpdateStatus is the progress and outcome of an update or rollback.
type UpdateStatus struct {
	// +kubebuilder:validation:Enum=platform;system;rollback
	Kind string `json:"kind"`
	From string `json:"from"`
	To   string `json:"to"`
	// FromBundle is the platform bundle that ran before the update; empty for the release's own platform.
	// +optional
	FromBundle string `json:"fromBundle,omitempty"`
	// FromILM is the ILM version that ran before a platform update, to which a rollback goes back; a rollback keeps
	// it. Unset in the status of an update recorded before it was kept.
	// +optional
	FromILM string `json:"fromILM,omitempty"`
	// Step is the step the update is at.
	// +optional
	Step string `json:"step,omitempty"`
	// Message is a fixed text, never an error text of a component.
	// +optional
	Message string `json:"message,omitempty"`
	// Backup is the name of the backup taken before the update.
	// +optional
	Backup    string      `json:"backup,omitempty"`
	StartedAt metav1.Time `json:"startedAt"`
	// PointOfNoReturn is set once the update has asked for the new ILM version, or the health check of a system update
	// passed; from then on only a rollback, or for a system update a restore onto a fresh appliance, undoes it.
	// +optional
	PointOfNoReturn bool `json:"pointOfNoReturn,omitempty"`
	// BootID is the boot ID of the node before the reboot a system update waits for: the reboot into the new Talos,
	// or back into the old one.
	// +optional
	BootID string `json:"bootID,omitempty"`
	// Deadline is when the wait at the recorded step of a system update gives up: for the manager of the new release
	// to take the update over, for the node to boot the new Talos and come back healthy, or to boot the old Talos
	// again after a rollback.
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`
	// Installed is set once a system update installed the new Talos as the boot entry the next boot boots.
	// +optional
	Installed bool `json:"installed,omitempty"`
	// PreviousManager is the manager image that ran before a system update applied the manager of the new release.
	// +optional
	PreviousManager string `json:"previousManager,omitempty"`
	// PreviousAddonSteps are the add-on steps of the release a system update started from, which its pass applied:
	// while the update holds the pass, they must stay ready.
	// +optional
	PreviousAddonSteps []string `json:"previousAddonSteps,omitempty"`
	// HandBack is the fixed reason of a system update that failed before the node booted the new Talos: the manager
	// of the new release runs the previous manager again, which records the update failed with it.
	// +optional
	HandBack string `json:"handBack,omitempty"`
}

// The system and rollback kinds of UpdateStatus, a rollback's progress being that of its restore; the step of a
// completed update; the step at which a system update releases the add-on pass; and the steps at which a system
// update waits for the manager of the new release to take it over.
const (
	UpdateKindSystem   = "system"
	UpdateKindRollback = "rollback"
	UpdateStepDone     = "done"
	UpdateStepAddons   = "addons"
	UpdateStepManager  = "manager"
	UpdateStepTalos    = "talos"
)

// Unfinished tells whether u is an update that neither completed nor failed; the progress of a rollback is that of its
// restore.
func (u *UpdateStatus) Unfinished() bool {
	return u != nil && u.Kind != UpdateKindRollback && u.Step != UpdateStepDone && u.Message == ""
}

// HoldsAddons tells whether u is a system update that holds the add-on pass: unfinished and not yet at its addons
// step, so that nothing changes the add-ons from its backup on.
func (u *UpdateStatus) HoldsAddons() bool {
	return u.Unfinished() && u.Kind == UpdateKindSystem && u.Step != UpdateStepAddons
}

// HandOverOverdue tells whether u is a system update that, at now, is past the deadline by which the manager of the
// new release had to take it over: unfinished at its manager step, or at its talos step before the boot was recorded,
// not handed back, with the previous manager recorded. The previous manager fails such an update.
func (u *UpdateStatus) HandOverOverdue(now time.Time) bool {
	return u.Unfinished() && u.Kind == UpdateKindSystem && u.PreviousManager != "" && u.HandBack == "" &&
		u.BootID == "" && (u.Step == UpdateStepManager || u.Step == UpdateStepTalos) && u.Deadline != nil &&
		!now.Before(u.Deadline.Time)
}

// HoldsAddonsFor tells whether s holds the add-on pass of the manager of version: while a system update holds it,
// and in a manager of another version than the one a system update made required past its point of no return.
func (s *ApplianceStatus) HoldsAddonsFor(version string) bool {
	return s.Update.HoldsAddons() || s.Versions.RequiredManager != "" && s.Versions.RequiredManager != version
}

// ConnectorState is the state of one chosen connector.
type ConnectorState struct {
	Name string `json:"name"`
	// +kubebuilder:validation:Enum=ready;waiting;failed
	State string `json:"state"`
	// Problem is a fixed text, never a message of the operator; set only for a failed connector.
	// +optional
	Problem string `json:"problem,omitempty"`
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
	// Pinned is the kind of update the backup was taken for; the backup stays on the data volume until the next
	// update of that kind finishes.
	// +optional
	Pinned string `json:"pinned,omitempty"`
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
