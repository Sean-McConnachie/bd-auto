package drain

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"bd-auto/internal/runner"
)

// BillingError is a safety-gate refusal. Source lets command renderers retain
// a machine-readable output contract without parsing the human explanation.
type BillingError struct {
	Source runner.BillingSource
	Roles  []string
	Cause  error
}

func (e *BillingError) Error() string {
	roles := strings.Join(e.Roles, "/")
	if e.Source == runner.BillingAPIKey {
		return fmt.Sprintf("drain: the %s Codex runner uses API-key billing; nothing was dispatched. Re-run with --allow-api-billing to authorize API charges for this command", roles)
	}
	reason := "the authentication source could not be established safely"
	if e.Cause != nil {
		reason = e.Cause.Error()
	}
	return fmt.Sprintf("drain: the %s Codex runner is not ready: %s; nothing was dispatched. Run `codex login` and choose ChatGPT authentication, then try again", roles, reason)
}

func (e *BillingError) Unwrap() error { return e.Cause }

// AuthorizeBilling checks each distinct billing-sensitive runner
// configuration once. It is a separate, non-skippable gate from Preflight.
func (e *Engine) AuthorizeBilling(ctx context.Context) error {
	if e.billingChecked {
		return e.billingErr
	}
	if e.Cfg == nil {
		return errors.New("drain: billing check needs Cfg")
	}
	e.billingChecked = true

	for _, group := range e.preflightGroups() {
		role := group.roles[0]
		spec := e.Cfg.Runner(string(role))
		if !spec.BillingSensitive {
			continue
		}
		rn, err := e.runnerFor(role)
		if err != nil {
			e.billingErr = err
			return err
		}
		source, checked, err := runner.BillingSourceOf(ctx, rn, e.RepoRoot)
		if !checked {
			continue
		}
		if err != nil || source == runner.BillingUnknown {
			e.billingErr = &BillingError{Source: runner.BillingUnknown, Roles: group.names(), Cause: err}
			return e.billingErr
		}
		if source == runner.BillingAPIKey && !e.AllowAPIBilling {
			e.billingErr = &BillingError{Source: source, Roles: group.names()}
			return e.billingErr
		}
		if source == runner.BillingAPIKey {
			e.logf("billing warning: Codex for %s uses API-key billing; --allow-api-billing authorizes API charges for this command", strings.Join(group.names(), ", "))
		}
	}
	return nil
}
