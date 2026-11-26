package v1alpha1

import (
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Steps of a restore in RestoreStatus.Step, in their order. A restore in place skips platform, keycloak and
// certificate. A rollback runs stop, uninstall and reinstall, then the steps of a restore in place from stop on.
const (
	RestoreStepSecrets     = "secrets"
	RestoreStepAppliance   = "appliance"
	RestoreStepPlatform    = "platform"
	RestoreStepStop        = "stop"
	RestoreStepILMSecrets  = "ilmSecrets"
	RestoreStepDatabase    = "database"
	RestoreStepCBOM        = "cbom"
	RestoreStepStart       = "start"
	RestoreStepKeycloak    = "keycloak"
	RestoreStepCertificate = "certificate"
	RestoreStepUninstall   = "uninstall"
	RestoreStepReinstall   = "reinstall"
	RestoreStepDone        = "done"
	// RestoreStepWaitingForKit is a restore the manager restarted during: the backup key it held in memory is gone,
	// and the restore resumes at PausedStep once the recovery kit is given again.
	RestoreStepWaitingForKit = "waitingForKit"
	// RestoreStepAbandoned is a failed restore the administrator gave up: the appliance keeps the state it reached.
	RestoreStepAbandoned = "abandoned"
)

// Reasons of RestoreStatus.Message.
const (
	RestoreFailed  = "failed"
	RestoreTimeout = "timeout"
)

// RestoreStatus is the progress of a restore. Step is the step that runs, recorded before it starts. A restore that
// failed keeps the step it failed at and has a Message; a restore waiting for the kit has Step waitingForKit and
// PausedStep set to the step it resumes at.
type RestoreStatus struct {
	// Backup is the file name of the backup.
	Backup string `json:"backup"`
	// File is the path of the backup file in the manager, read again at each step.
	// +optional
	File string `json:"file,omitempty"`
	// +optional
	InPlace bool `json:"inPlace,omitempty"`
	// ILMHostName is the ILM host name the restore sets; empty keeps the backup's.
	// +optional
	ILMHostName string `json:"ilmHostName,omitempty"`
	// +kubebuilder:validation:Enum=secrets;appliance;platform;stop;ilmSecrets;database;cbom;start;keycloak;certificate;uninstall;reinstall;done;waitingForKit;abandoned
	Step string `json:"step"`
	// PausedStep is the step a restore waiting for the kit resumes at.
	// +optional
	// +kubebuilder:validation:Enum=secrets;appliance;platform;stop;ilmSecrets;database;cbom;start;keycloak;certificate;uninstall;reinstall
	PausedStep string `json:"pausedStep,omitempty"`
	// AbandonedStep is the step an abandoned restore had failed at.
	// +optional
	// +kubebuilder:validation:Enum=secrets;appliance;platform;stop;ilmSecrets;database;cbom;start;keycloak;certificate;uninstall;reinstall
	AbandonedStep string `json:"abandonedStep,omitempty"`
	// Message is the fixed reason a restore failed at Step; set only on failure.
	// +optional
	// +kubebuilder:validation:Enum=failed;timeout
	Message string `json:"message,omitempty"`
	// Warning names what a completed restore left for the administrator, such as host names an uploaded certificate
	// does not cover.
	// +optional
	Warning string `json:"warning,omitempty"`
	// Dependencies replaces the backup's choice of the dependencies it names on a fresh restore.
	// +optional
	Dependencies *RestoreDependencies `json:"dependencies,omitempty"`
	// +optional
	StartedAt metav1.Time `json:"startedAt,omitempty"`
	// StartedBy is the console account that started the restore; a restore in place keeps it among the backup's
	// accounts.
	// +optional
	StartedBy string `json:"startedBy,omitempty"`
	// PlatformBundle is the loaded platform bundle a fresh restore installs the platform from; empty for the
	// release's own platform.
	// +optional
	PlatformBundle string `json:"platformBundle,omitempty"`
	// Rollback is set on a restore in place that rolls a platform update back.
	// +optional
	Rollback *RestoreRollback `json:"rollback,omitempty"`
}

// RestoreRollback is a platform update rolled back: the platform is uninstalled, installed again at ILMVersion and
// restored from the backup taken before the update.
type RestoreRollback struct {
	// ILMVersion is the ILM version the platform is installed at again.
	ILMVersion string `json:"ilmVersion"`
	// PlatformBundle is the platform bundle that runs again; empty for the release's own platform.
	// +optional
	PlatformBundle string `json:"platformBundle,omitempty"`
	// Reinstalled is set once the platform runs at ILMVersion again: the steps from stop on restore the backup.
	// +optional
	Reinstalled bool `json:"reinstalled,omitempty"`
}

// Active tells whether a restore runs or waits for the kit: it neither completed, failed nor was abandoned.
func (r *RestoreStatus) Active() bool {
	return r.Unsettled() && r.Message == ""
}

// Unsettled tells whether a restore neither completed nor was abandoned: it runs, waits for the kit or failed, and
// keeps the backups and the ILM settings held until it ends.
func (r *RestoreStatus) Unsettled() bool {
	return r != nil && r.Step != RestoreStepDone && r.Step != RestoreStepAbandoned
}

// CurrentStep is the step the restore is at: PausedStep while it waits for the kit, Step otherwise.
func (r *RestoreStatus) CurrentStep() string {
	switch {
	case r == nil:
		return ""
	case r.Step == RestoreStepWaitingForKit:
		return r.PausedStep
	default:
		return r.Step
	}
}

// HoldsOperator tells whether the ILM operator must stay at zero replicas: from stop until the restore starts it, and
// from stop until a rollback installs the platform again.
func (r *RestoreStatus) HoldsOperator() bool {
	return r.Active() && slices.Contains([]string{
		RestoreStepStop, RestoreStepUninstall, RestoreStepILMSecrets, RestoreStepDatabase, RestoreStepCBOM,
	}, r.CurrentStep())
}

// PausesPlatform tells whether the Platform must not be applied: from stop until the started operator reports it
// ready.
func (r *RestoreStatus) PausesPlatform() bool {
	return r.HoldsOperator() || (r.Active() && r.CurrentStep() == RestoreStepStart)
}

// Dependencies of the platform that a restore can choose anew.
const (
	DependencyDatabase  = "database"
	DependencyMessaging = "messaging"
	DependencyIdentity  = "identity"
)

// RestoreDependencies are the dependencies a fresh restore chooses instead of the backup's: each one Replace names
// is external when its block is set and managed otherwise.
type RestoreDependencies struct {
	// +optional
	// +listType=set
	// +kubebuilder:validation:items:Enum=database;messaging;identity
	Replace []string `json:"replace,omitempty"`
	// +optional
	Database *ExternalDatabase `json:"database,omitempty"`
	// +optional
	Messaging *ExternalMessaging `json:"messaging,omitempty"`
	// +optional
	Identity *ExternalIdentity `json:"identity,omitempty"`
}

// Replaces tells whether d replaces the backup's choice of the dependency kind.
func (d *RestoreDependencies) Replaces(kind string) bool {
	return d != nil && slices.Contains(d.Replace, kind)
}
