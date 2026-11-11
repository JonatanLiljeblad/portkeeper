# Checkout not ready

Applies to the `checkout` Deployment in the demo sandbox.

## Symptom

`checkout` reports zero ready replicas although its pods are running.

## Known cause

A configuration fix was applied, but the running pods started before it and
still hold the old configuration. Pods read configuration only at startup.

## Procedure

1. Confirm the symptom with `get_deployment_status` for `checkout`.
2. Restart the Deployment with `restart_deployment` so new pods read the
   corrected configuration.
3. Call `get_deployment_status` again with `waitSeconds` set to 60 and
   confirm that every desired replica is ready.
4. Scale `checkout` to 3 replicas with `scale_deployment` to work through
   the backlog that built up during the outage.
5. Report what you did and the final state.

If a step fails or is refused, do not look for another way to achieve it.
Stop, and report which step did not complete so that a human can decide.
