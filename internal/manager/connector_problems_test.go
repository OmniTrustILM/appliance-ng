package manager

import (
	"testing"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

func connectorIn(phase otilm.ConnectorPhase, registration otilm.RegistrationStatusValue) *otilm.Connector {
	c := &otilm.Connector{}
	c.Spec.Registration = &otilm.RegistrationSpec{}
	c.Status.Phase = phase
	if registration != "" {
		c.Status.Registration = &otilm.RegistrationStatus{Status: registration}
	}
	return c
}

func waitingPod(reason string) corev1.Pod {
	return corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}},
	}}}
}

func TestConnectorProblem(t *testing.T) {
	oom := corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}},
	}}}
	unscheduled := corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionFalse},
	}}}
	initFailing := corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
		{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}}},
	}}}
	const (
		pull     = "the image cannot be pulled; check the registry credentials and the proxy"
		restarts = "the connector keeps restarting; see its log"
	)
	tests := []struct {
		name string
		c    *otilm.Connector
		pods []corev1.Pod
		want string
	}{
		{"running and ready", connectorIn(otilm.ConnectorPhaseRunning, otilm.RegistrationStatusConnected), nil, ""},
		{"ready ignores pods", connectorIn(otilm.ConnectorPhaseRunning, otilm.RegistrationStatusConnected), []corev1.Pod{waitingPod("CrashLoopBackOff")}, ""},
		{"ErrImagePull", connectorIn(otilm.ConnectorPhaseDeploying, ""), []corev1.Pod{waitingPod("ErrImagePull")}, pull},
		{"ImagePullBackOff", connectorIn(otilm.ConnectorPhaseDeploying, ""), []corev1.Pod{waitingPod("ImagePullBackOff")}, pull},
		{"init container", connectorIn(otilm.ConnectorPhaseDeploying, ""), []corev1.Pod{initFailing}, pull},
		{"CrashLoopBackOff", connectorIn(otilm.ConnectorPhaseDeploying, ""), []corev1.Pod{waitingPod("CrashLoopBackOff")}, restarts},
		{"OOMKilled", connectorIn(otilm.ConnectorPhaseDeploying, ""), []corev1.Pod{oom}, "the connector ran out of memory"},
		{"not scheduled", connectorIn(otilm.ConnectorPhaseDeploying, ""), []corev1.Pod{unscheduled}, "the connector cannot be scheduled"},
		{"CreateContainerConfigError", connectorIn(otilm.ConnectorPhaseDeploying, ""),
			[]corev1.Pod{waitingPod("CreateContainerConfigError")}, "a Secret or setting the connector needs is missing"},
		{"registration failed", connectorIn(otilm.ConnectorPhaseRunning, otilm.RegistrationStatusFailed), nil, "ILM refused the registration"},
		{"registration offline", connectorIn(otilm.ConnectorPhaseRunning, otilm.RegistrationStatusOffline), nil, "ILM cannot reach the connector"},
		{"failed phase", connectorIn(otilm.ConnectorPhaseFailed, ""), nil, "the operator reports the connector as failed"},
		{"pod reason before registration", connectorIn(otilm.ConnectorPhaseRunning, otilm.RegistrationStatusFailed),
			[]corev1.Pod{waitingPod("CrashLoopBackOff")}, restarts},
		{"earlier problem in the list wins", connectorIn(otilm.ConnectorPhaseDeploying, ""),
			[]corev1.Pod{waitingPod("CrashLoopBackOff"), waitingPod("ErrImagePull")}, pull},
		{"deploying without a problem", connectorIn(otilm.ConnectorPhaseDeploying, ""), nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := connectorProblem(tt.c, tt.pods); got != tt.want {
				t.Errorf("connectorProblem = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConnectorStates(t *testing.T) {
	ready := connectorIn(otilm.ConnectorPhaseRunning, otilm.RegistrationStatusConnected)
	ready.Name = "ok"
	crash := connectorIn(otilm.ConnectorPhaseDeploying, "")
	crash.Name = "crash"
	slow := connectorIn(otilm.ConnectorPhaseDeploying, "")
	slow.Name = "slow"
	got := connectorStates([]otilm.Connector{*ready, *crash, *slow}, map[string][]corev1.Pod{"crash": {waitingPod("CrashLoopBackOff")}})
	want := []v1alpha1.ConnectorState{
		{Name: "ok", State: "ready"},
		{Name: "crash", State: "failed", Problem: "the connector keeps restarting; see its log"},
		{Name: "slow", State: "waiting"},
	}
	if len(got) != len(want) {
		t.Fatalf("states = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("state %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
