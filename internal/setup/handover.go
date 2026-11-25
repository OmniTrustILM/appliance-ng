package setup

import "github.com/OmniTrustILM/appliance-ng/internal/certs"

// HandoverVersion is the version of the hand-over file format.
const HandoverVersion = 1

// Modes of the hand-over: a new appliance runs wizard part 2, a restore waits for a backup. A file without a mode,
// as an older first-boot wrote it, is a new appliance.
const (
	ModeNew     = "new"
	ModeRestore = "restore"
)

// HandoverFile is what wizard part 1 hands over to the Appliance Manager through the hand-over file. It holds the
// administrator's password hash and TOTP seed, so it is never logged.
type HandoverFile struct {
	Version int         `json:"version"`
	Mode    string      `json:"mode,omitempty"`
	Node    NodeAnswers `json:"node"`
	Admin   struct {
		User         string `json:"user"`
		PasswordHash string `json:"passwordHash"`
		TOTPSeed     string `json:"totpSeed"`
	} `json:"admin"`
	// TLS is the certificate and key the setup server served, so that the console serves the same until setup is
	// complete. A file of an older first-boot has none.
	TLS *HandoverTLS `json:"tls,omitempty"`
}

// HandoverTLS is the setup certificate and its key, PEM encoded.
type HandoverTLS struct {
	CertPEM string `json:"certPEM"`
	KeyPEM  string `json:"keyPEM"`
}

// NodeAnswers are the node settings of wizard part 1, validated. An empty Address keeps the address DHCP delivers.
type NodeAnswers struct {
	HostName string   `json:"hostName"`
	Address  string   `json:"address,omitempty"`
	Gateway  string   `json:"gateway,omitempty"`
	DNS      []string `json:"dns"`
	NTP      []string `json:"ntp,omitempty"`
	Proxy    *Proxy   `json:"proxy,omitempty"`
}

// Proxy is the outbound HTTP proxy; an empty field is not set.
type Proxy struct {
	HTTP    string `json:"http,omitempty"`
	HTTPS   string `json:"https,omitempty"`
	NoProxy string `json:"noProxy,omitempty"`
}

// First-boot hands the hand-over file to the Appliance Manager as the Secret HandoverSecretName in
// HandoverSecretNamespace, with the file under the key HandoverSecretKey.
const (
	HandoverSecretNamespace = "appliance-system"
	HandoverSecretName      = "appliance-handover"
	HandoverSecretKey       = "handover.json"
)

// SetupTLSSecretName is the TLS Secret in HandoverSecretNamespace that holds the setup certificate until setup is
// complete.
const SetupTLSSecretName = certs.SetupSecretName
