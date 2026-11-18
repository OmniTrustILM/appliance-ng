package setup

// HandoverVersion is the version of the hand-over file format.
const HandoverVersion = 1

// HandoverFile is what wizard part 1 hands over to the Appliance Manager through the hand-over file. It holds the
// administrator's password hash and TOTP seed, so it is never logged.
type HandoverFile struct {
	Version int         `json:"version"`
	Node    NodeAnswers `json:"node"`
	Admin   struct {
		User         string `json:"user"`
		PasswordHash string `json:"passwordHash"`
		TOTPSeed     string `json:"totpSeed"`
	} `json:"admin"`
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
