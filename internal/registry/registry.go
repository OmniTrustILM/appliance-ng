// Package registry holds what the appliance knows about its in-cluster registry.
package registry

const (
	// Port is the port zot listens on, on the node's loopback address.
	Port = 5443
	// PushUser is the only user that may push to zot.
	PushUser = "appliance-manager"
	// SecretName is the Secret in the system namespace with zot's htpasswd file (key htpasswd) and the push user's
	// password (key password).
	SecretName = "zot-htpasswd"
)
