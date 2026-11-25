package manager

import (
	"context"
	"slices"
	"time"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/ilmclient"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

// Statuses ILM lists for a connector: one it reaches and has approved, and one it reaches that waits for approval.
const (
	ilmConnected          = "connected"
	ilmWaitingForApproval = "waitingForApproval"
)

// ilmPassTimeout bounds the ILM part of a registration pass, so that a slow ILM cannot hold up the platform step.
var ilmPassTimeout = 15 * time.Second

// registrationOutcome is what a registration pass found out in ILM, by connector name.
type registrationOutcome struct {
	// Awaiting are the running v1 connectors that ILM does not list as connected yet: all of them while the platform
	// is not ready or ILM's settings are not applied, which shows that the manager's identity is registered and ILM
	// answers.
	Awaiting []string
	// Refused are the v1 connectors that ILM refused to register.
	Refused []string
	// Reachable are the connectors asked about that ILM lists as connected or waiting for approval.
	Reachable []string
}

// reconcileV1Registrations registers the running connectors that speak interface v1 only in ILM, which the operator
// does not do for them, and removes the registration of those no longer chosen. A connector is found in ILM by its
// URL, so that nothing but ILM keeps its registration. The same list of ILM's connectors tells which of the chosen
// connectors named in offline, that the operator recorded as offline once and never looks at again, ILM reaches
// now. A failure is logged with its fixed reason and retried by the next pass, never returned.
func (r *ApplianceReconciler) reconcileV1Registrations(ctx context.Context, a *v1alpha1.Appliance, running []otilm.Connector, offline []string, platformReady bool) registrationOutcome {
	var wanted []release.Connector
	for _, c := range running {
		if i := slices.IndexFunc(r.Release.Connectors, func(rc release.Connector) bool { return rc.Name == c.Name }); i >= 0 && r.Release.Connectors[i].V1Only() {
			wanted = append(wanted, r.Release.Connectors[i])
		}
	}
	waiting := func() registrationOutcome {
		out := registrationOutcome{}
		for _, c := range wanted {
			out.Awaiting = append(out.Awaiting, c.Name)
		}
		return out
	}
	if !platformReady || !meta.IsStatusConditionTrue(a.Status.Conditions, v1alpha1.ConditionILMSettingsApplied) {
		return waiting()
	}
	ctx, cancel := context.WithTimeout(ctx, ilmPassTimeout)
	defer cancel()
	c, err := ilmClientOf(ctx, r.Client, r.APIReader, r.ILM, a)
	if err != nil {
		logV1Failure(ctx, err, "could not reach ILM to register the v1 connectors")
		return waiting()
	}
	registered, err := c.ListConnectors(ctx)
	if err != nil {
		logV1Failure(ctx, err, "could not list the connectors in ILM")
		return waiting()
	}
	r.removeV1Registrations(ctx, a, c, registered)
	out := registrationOutcome{Reachable: r.reachable(offline, registered)}
	for _, rc := range wanted {
		switch r.registerV1(ctx, c, rc, registered) {
		case v1Connected:
		case v1Refused:
			out.Refused = append(out.Refused, rc.Name)
		default:
			out.Awaiting = append(out.Awaiting, rc.Name)
		}
	}
	return out
}

// reachable returns those of the named catalogue connectors that ILM lists, under their URL, as connected or
// waiting for approval.
func (r *ApplianceReconciler) reachable(names []string, registered []ilmclient.Connector) []string {
	var out []string
	for _, rc := range r.Release.Connectors {
		if !slices.Contains(names, rc.Name) {
			continue
		}
		url := platform.ConnectorURL(rc)
		if slices.ContainsFunc(registered, func(l ilmclient.Connector) bool {
			return l.URL == url && (l.Status == ilmConnected || l.Status == ilmWaitingForApproval)
		}) {
			out = append(out, rc.Name)
		}
	}
	return out
}

// v1Result is how far ILM has taken a v1 connector.
type v1Result int

const (
	v1Pending v1Result = iota
	v1Connected
	v1Refused
)

// registerV1 creates the connector in ILM, or asks ILM to connect to it again when it is listed but not connected,
// and reports whether ILM already lists it as connected, or definitely refused to create it.
func (r *ApplianceReconciler) registerV1(ctx context.Context, c *ilmclient.Client, rc release.Connector, registered []ilmclient.Connector) v1Result {
	url := platform.ConnectorURL(rc)
	i := slices.IndexFunc(registered, func(l ilmclient.Connector) bool { return l.URL == url })
	switch {
	case i < 0:
		if _, err := c.CreateConnector(ctx, rc.DisplayName, url); err != nil {
			logV1Failure(ctx, err, "could not register a v1 connector in ILM", "connector", rc.Name)
			if ilmclient.IsRefusal(err) {
				return v1Refused
			}
			return v1Pending
		}
		log.FromContext(ctx).Info("registered a v1 connector in ILM", "connector", rc.Name)
	case registered[i].Status != ilmConnected:
		if err := c.ReconnectConnector(ctx, registered[i].UUID); err != nil {
			logV1Failure(ctx, err, "could not reconnect a v1 connector in ILM", "connector", rc.Name)
		}
	default:
		return v1Connected
	}
	return v1Pending
}

// removeV1Registrations deletes from ILM the connectors listed under the URL of a v1 connector of the catalogue that
// is not chosen any more. Other connectors, those of an administrator and the v2 ones, stay.
func (r *ApplianceReconciler) removeV1Registrations(ctx context.Context, a *v1alpha1.Appliance, c *ilmclient.Client, registered []ilmclient.Connector) {
	for _, rc := range r.Release.Connectors {
		if !rc.V1Only() || slices.Contains(a.Spec.Platform.Components, rc.Name) {
			continue
		}
		for _, l := range registered {
			if l.URL != platform.ConnectorURL(rc) {
				continue
			}
			if err := c.DeleteConnector(ctx, l.UUID); err != nil {
				logV1Failure(ctx, err, "could not remove a v1 connector from ILM", "connector", rc.Name)
				continue
			}
			log.FromContext(ctx).Info("removed a v1 connector that is no longer among the components from ILM", "connector", rc.Name)
		}
	}
}

// logV1Failure logs a failed ILM call by its fixed reason; ILM's answer is never logged.
func logV1Failure(ctx context.Context, err error, message string, keysAndValues ...any) {
	log.FromContext(ctx).Info(message, append(keysAndValues, "reason", ilmclient.Reason(err))...)
}
