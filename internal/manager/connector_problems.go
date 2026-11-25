package manager

import (
	"context"
	"fmt"
	"slices"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

// States of a chosen Connector in the Appliance status.
const (
	connectorStateReady   = "ready"
	connectorStateWaiting = "waiting"
	connectorStateFailed  = "failed"
)

// Problems of a Connector. They are fixed texts: the operator's and the pods' messages stay out of the Appliance.
const (
	problemImagePull      = "the image cannot be pulled; check the registry credentials and the proxy"
	problemCrashLoop      = "the connector keeps restarting; see its log"
	problemOutOfMemory    = "the connector ran out of memory"
	problemNotScheduled   = "the connector cannot be scheduled"
	problemMissingConfig  = "a Secret or setting the connector needs is missing"
	problemRegistration   = "ILM refused the registration"
	problemUnreachable    = "ILM cannot reach the connector"
	problemOperatorFailed = "the operator reports the connector as failed"
)

// connectorStates returns the state of every Connector, with the problem of a failed one. pods holds the pods of
// each Connector that is not ready, by Connector name.
func connectorStates(connectors []otilm.Connector, pods map[string][]corev1.Pod) []v1alpha1.ConnectorState {
	states := make([]v1alpha1.ConnectorState, len(connectors))
	for i := range connectors {
		c := &connectors[i]
		states[i] = v1alpha1.ConnectorState{Name: c.Name, State: connectorStateWaiting}
		if connectorReady(c) {
			states[i].State = connectorStateReady
		} else if problem := connectorProblem(c, pods[c.Name]); problem != "" {
			states[i].State = connectorStateFailed
			states[i].Problem = problem
		}
	}
	return states
}

// runningConnectors returns the connectors whose state is ready.
func runningConnectors(connectors []otilm.Connector, states []v1alpha1.ConnectorState) []otilm.Connector {
	var running []otilm.Connector
	for i := range connectors {
		if states[i].State == connectorStateReady {
			running = append(running, connectors[i])
		}
	}
	return running
}

// offlineConnectors returns the names of the connectors that failed only because the operator recorded their
// registration as offline.
func offlineConnectors(states []v1alpha1.ConnectorState) []string {
	var names []string
	for _, s := range states {
		if s.State == connectorStateFailed && s.Problem == problemUnreachable {
			names = append(names, s.Name)
		}
	}
	return names
}

// withRegistrations returns states with what the ILM pass found: the connectors ILM reaches ready and those it
// refused failed with the registration problem.
func withRegistrations(states []v1alpha1.ConnectorState, o registrationOutcome) []v1alpha1.ConnectorState {
	out := slices.Clone(states)
	for i := range out {
		switch name := out[i].Name; {
		case slices.Contains(o.Reachable, name):
			out[i] = v1alpha1.ConnectorState{Name: name, State: connectorStateReady}
		case slices.Contains(o.Refused, name):
			out[i] = v1alpha1.ConnectorState{Name: name, State: connectorStateFailed, Problem: problemRegistration}
		}
	}
	return out
}

// waitingStates returns states with those of the named connectors set to waiting.
func waitingStates(states []v1alpha1.ConnectorState, names []string) []v1alpha1.ConnectorState {
	out := slices.Clone(states)
	for i := range out {
		if slices.Contains(names, out[i].Name) {
			out[i].State = connectorStateWaiting
		}
	}
	return out
}

// connectorProblem returns why a Connector that is not ready has failed, or "" while it is ready or still on its
// way. The pods tell first, in the order of the problems below, then the registration, then the operator's phase.
func connectorProblem(c *otilm.Connector, pods []corev1.Pod) string {
	if connectorReady(c) {
		return ""
	}
	if problem := podProblem(pods); problem != "" {
		return problem
	}
	if r := c.Status.Registration; r != nil {
		switch r.Status {
		case otilm.RegistrationStatusFailed:
			return problemRegistration
		case otilm.RegistrationStatusOffline:
			return problemUnreachable
		}
	}
	if c.Status.Phase == otilm.ConnectorPhaseFailed {
		return problemOperatorFailed
	}
	return ""
}

// podProblem returns the first problem of the list that any container of the pods shows, init containers included.
func podProblem(pods []corev1.Pod) string {
	waiting := func(reasons ...string) bool {
		return anyContainer(pods, func(s *corev1.ContainerStatus) bool {
			return s.State.Waiting != nil && slices.Contains(reasons, s.State.Waiting.Reason)
		})
	}
	switch {
	case waiting("ErrImagePull", "ImagePullBackOff"):
		return problemImagePull
	case waiting("CrashLoopBackOff"):
		return problemCrashLoop
	case anyContainer(pods, func(s *corev1.ContainerStatus) bool {
		t := s.LastTerminationState.Terminated
		return t != nil && t.Reason == "OOMKilled"
	}):
		return problemOutOfMemory
	case anyPod(pods, func(p *corev1.Pod) bool {
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
				return true
			}
		}
		return false
	}):
		return problemNotScheduled
	case waiting("CreateContainerConfigError"):
		return problemMissingConfig
	}
	return ""
}

func anyPod(pods []corev1.Pod, match func(*corev1.Pod) bool) bool {
	for i := range pods {
		if match(&pods[i]) {
			return true
		}
	}
	return false
}

func anyContainer(pods []corev1.Pod, match func(*corev1.ContainerStatus) bool) bool {
	return anyPod(pods, func(p *corev1.Pod) bool {
		for _, statuses := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
			for i := range statuses {
				if match(&statuses[i]) {
					return true
				}
			}
		}
		return false
	})
}

// connectorPods returns the pods of the Deployment of every Connector that is not ready, by Connector name. A
// Connector whose Deployment does not exist yet has no pods, and neither has one whose pods cannot be read: the
// pods only explain a failure, so the platform does not fail with them.
func (r *ApplianceReconciler) connectorPods(ctx context.Context, connectors []otilm.Connector) map[string][]corev1.Pod {
	pods := map[string][]corev1.Pod{}
	for i := range connectors {
		c := &connectors[i]
		if connectorReady(c) {
			continue
		}
		list, err := r.readPods(ctx, c.Name)
		if err != nil {
			log.FromContext(ctx).Error(err, "read the pods of a connector; its state is reported without them", "connector", c.Name)
			continue
		}
		pods[c.Name] = list
	}
	return pods
}

// readPods returns the pods the Deployment named name selects, none while the Deployment does not exist.
func (r *ApplianceReconciler) readPods(ctx context.Context, name string) ([]corev1.Pod, error) {
	deploy := &appsv1.Deployment{}
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: name}, deploy)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the deployment: %w", err)
	}
	selector, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("read the pod selector: %w", err)
	}
	list := &corev1.PodList{}
	if err := r.APIReader.List(ctx, list, client.InNamespace(platform.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("list the pods: %w", err)
	}
	return list.Items, nil
}
