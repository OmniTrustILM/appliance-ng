package v1alpha1

import (
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Steps of a restore in RestoreStatus.Step, in their order. A restore in place skips platform, keycloak and
// certificate.
const (
	RestoreStepSecrets     = "secrets"
	RestoreStepAppliance   = "appliance"
	RestoreStepPlatform    = "platform"
	RestoreStepStop        = "stop"
	RestoreStepILMSecrets  = "ilmSecrets"
	RestoreStepDatabase    = "database"
	RestoreStepStart       = "start"
	RestoreStepKeycloak    = "keycloak"
	RestoreStepCertificate = "certificate"
	RestoreStepDone        = "done"
	// RestoreStepWaitingForKit is a restore the manager restarted during: the backup key it held in memory is gone,
	// and the restore resumes at PausedStep once the recovery kit is given again.
	RestoreStepWaitingForKit = "waitingForKit"
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
	// +kubebuilder:validation:Enum=secrets;appliance;platform;stop;ilmSecrets;database;start;keycloak;certificate;done;waitingForKit
	Step string `json:"step"`
	// PausedStep is the step a restore waiting for the kit resumes at.
	// +optional
	// +kubebuilder:validation:Enum=secrets;appliance;platform;stop;ilmSecrets;database;start;keycloak;certificate
	PausedStep string `json:"pausedStep,omitempty"`
	// Message is the fixed reason a restore failed at Step; set only on failure.
	// +optional
	// +kubebuilder:validation:Enum=failed;timeout
	Message string `json:"message,omitempty"`
	// Warning names what a completed restore left for the administrator, such as host names an uploaded certificate
	// does not cover.
	// +optional
	Warning string `json:"warning,omitempty"`
	// +optional
	StartedAt metav1.Time `json:"startedAt,omitempty"`
	// StartedBy is the console account that started the restore; a restore in place keeps it among the backup's
	// accounts.
	// +optional
	StartedBy string `json:"startedBy,omitempty"`
}

// Active tells whether a restore runs or waits for the kit: it neither completed nor failed.
func (r *RestoreStatus) Active() bool {
	return r != nil && r.Step != RestoreStepDone && r.Message == ""
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

// HoldsOperator tells whether the ILM operator must stay at zero replicas: from stop until the restore starts it.
func (r *RestoreStatus) HoldsOperator() bool {
	return r.Active() && slices.Contains([]string{RestoreStepStop, RestoreStepILMSecrets, RestoreStepDatabase}, r.CurrentStep())
}

// PausesPlatform tells whether the Platform must not be applied: from stop until the started operator reports it
// ready.
func (r *RestoreStatus) PausesPlatform() bool {
	return r.HoldsOperator() || (r.Active() && r.CurrentStep() == RestoreStepStart)
}
