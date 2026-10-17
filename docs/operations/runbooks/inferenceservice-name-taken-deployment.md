# InferenceService stuck in `Failed` with `DeploymentNameTaken`

An `InferenceService` sits with `status.schedulingStatus: DeploymentNameTaken`
and a phase of `Failed` while it should be serving, or `Suspended`/`Stopped`
when it is deliberately off (the refusal respects the user's intent and rides
the marker instead). The reconcile creates or updates nothing past the refused
Deployment step, and it removes the operator-owned HPA so the HPA cannot keep
scaling the foreign object; everything else an earlier pass provisioned is
left in place. The operator log repeats a refusal line on a slow two-minute
cadence instead of erroring. The controller is not broken: a Deployment
already holds the name the service generates, and the controller refuses to
touch an object it did not create (#2011).

## Trigger

One or more of:

- `kubectl get inferenceservice <name>` shows `PHASE: Failed` that never
  clears, and `kubectl describe` shows a Warning event:
  `DeploymentNameTaken Deployment default/<name> already exists and is not
  controlled by this InferenceService; ...`
- Operator log signal, repeated about every 2 minutes (the bounded recheck,
  not an error backoff):

  ```
  INFO Refusing to overwrite or recreate a Deployment this InferenceService
    does not control  {"error": "Deployment default/<name> already exists and
    is not controlled by this InferenceService; refusing to overwrite or
    recreate it (LLMKube did not create it); delete or rename the object, or
    the service"}
  ```

- The message variant `is already controlled by another InferenceService
  (uid <uid>)` instead: the object at that name carries a controller
  reference to a different `InferenceService` UID.
- The message variant `is still controlled by a previous InferenceService
  named <name> (uid <uid>) that was deleted`: the InferenceService was deleted
  and recreated under the same name (for example a GitOps replace) before
  garbage collection removed the old Deployment. No action is needed; the
  refusal clears on its own once the old Deployment is gone, because its
  deletion triggers a reconcile of the new service.

## Diagnose

1. **Read the service status.**

   ```bash
   kubectl get inferenceservice <name> -o jsonpath='{.status.phase}{"\n"}{.status.schedulingStatus}{"\n"}{.status.schedulingMessage}{"\n"}'
   kubectl get inferenceservice <name> -o jsonpath='{.status.conditions[?(@.type=="Degraded")]}' | jq
   ```

   Expect `Failed` and `DeploymentNameTaken` for a service that should be
   serving; a suspended or zero-replica service reports `Suspended` or
   `Stopped` with the same marker, and the conflict bites when it is
   unsuspended or scaled back up. The message names the blocking Deployment
   and the remedy. The `Degraded` condition carries the generic
   `Failed` reason; the specific `DeploymentNameTaken` reason appears only in
   `status.schedulingStatus` and the Warning event.

2. **Inspect whatever holds the name.** This is the decision point.

   ```bash
   kubectl get deployment <name> -n <namespace> -o jsonpath='{.metadata.ownerReferences}' | jq
   kubectl get deployment <name> -n <namespace> -o jsonpath='{.spec.selector.matchLabels}{"\n"}{.spec.template.spec.containers[*].image}{"\n"}'
   ```

   Four cases:

   - **No owner references at all**: something other than LLMKube created a
     Deployment with this name (a hand-rolled workload, another operator, a
     leftover). The container image tells you whose it is. This includes a
     legacy object from before the controller stamped owner references: such
     an object is refused, not migrated, even when its selector predates the
     current one.
   - **A controller reference to a different `InferenceService` UID**: a
     same-name service was deleted and recreated before garbage collection
     removed its old Deployment (or the old Deployment was orphaned with
     `kubectl delete --cascade=orphan`), and the new service now refuses to
     adopt the stale one. Check whether the referenced service still exists:
     `kubectl get inferenceservice <name> -o jsonpath='{.metadata.uid}'`.
   - **A controller reference to this service's own UID** means the operator
     manages the Deployment and no refusal fires for it; if you see the
     refusal anyway, the UID in the message differs from `metadata.uid`
     here, which is the different-owner case above.
   - **A reference to this service's UID without `controller: true`** (for
     example a third-party applier stamping plain owner references): refused
     all the same, with the generic "is not controlled by this
     InferenceService" message, because the gate requires the controller
     role. The reference alone does not hand the object to the operator.

3. **Check what still serves through the name.**

   ```bash
   kubectl get endpointslices -l kubernetes.io/service-name=<name> -n <namespace>
   ```

   A foreign Deployment may still be running pods under the same name. Do not
   assume the name is free just because the InferenceService is new.

   Also check for a Service the operator provisioned on an earlier pass: the
   refusal leaves it running, and its selector still matches any pod carrying
   the service's labels, including a foreign Deployment's pods when the
   foreign object copied the same selector. The cluster name can therefore
   route to pods the operator does not own while the phase reads `Failed`,
   and `status.endpoint` is emptied by the refusal even though that Service
   may still route.

   ```bash
   kubectl get service <name> -n <namespace> -o jsonpath='{.spec.selector}{"\n"}{.metadata.ownerReferences}' | jq
   ```

   The same applies to the operator-owned HPA when `spec.autoscaling` is set:
   an HPA left scaling the generated name would scale the foreign
   Deployment's replicas, so the refusal removes the operator-owned HPA (and
   its `AutoscalingReady` condition) while the name is not the service's. A
   foreign HPA is left alone.

## Mitigate (immediate)

Pick based on the Diagnose decision:

1. **If the operator's own Service is still up** (the service was serving
   before the name changed hands): decide whether it should keep routing. It
   now selects whatever pods match its labels, which may be the foreign
   Deployment's. Delete the Service, or fix the foreign workload's labels, or
   rename a claimant; the refusal neither drains nor deletes the Service, so
   this decision is yours. The operator-owned HPA needs no such decision:
   the refusal removes it while the name is not the service's, so it cannot
   keep scaling the foreign object.

2. **The foreign Deployment is not needed** (nobody's workload, or a stale
   leftover from a deleted service): delete it. The controller provisions the
   owned Deployment on its next pass; the refusal is rechecked about every
   2 minutes, so recovery needs no manual poke.

   ```bash
   kubectl delete deployment <name> -n <namespace>
   ```

3. **The foreign Deployment is someone's real workload**: rename one of the
   two claimants. Either rename the workload (and whatever selects it), or
   rename the `InferenceService` (`metadata.name` is immutable, so this means
   create a new one and delete the old). The refusal never resolves on its
   own; it waits for a human.

Do **not** try to hand the object to the operator by editing owner references
by hand unless you can vouch for the workload's entire spec: the moment the
reference matches, the operator replaces the spec with what the
`InferenceService` asks for on the next reconcile.

## Resolve (structural)

If the collision recurs (for example another controller in the namespace
mints the same names), rename the `InferenceService` rather than deleting
objects on every pass. Every object the operator itself creates carries the
service's controller reference from birth, so a service's own Deployment
never trips this refusal; only a genuinely foreign object at the generated
name can.

## Verify

1. **The refusal is gone from status.**

   ```bash
   kubectl get inferenceservice <name> -o jsonpath='{.status.phase}{"\n"}{.status.schedulingStatus}{"\n"}'
   ```

   Expect the phase to leave `Failed` (`Creating` then `Progressing` then
   `Ready` as pods come up) and `schedulingStatus` to be empty.

2. **The Deployment exists and is owned.**

   ```bash
   kubectl get deployment <name> -n <namespace> -o jsonpath='{.metadata.ownerReferences}' | jq
   ```

   Exactly one controller reference naming this `InferenceService`.

3. **No new Warning events.**

   ```bash
   kubectl get events -n <namespace> --field-selector reason=DeploymentNameTaken --sort-by=.lastTimestamp | tail -3
   ```

   The last event should predate your fix; a fresh one means the name is
   occupied again. One caveat: the event is emitted before the status write
   persists the marker, so a status write that conflicts re-fires the Warning
   on the next pass for the same episode. A fresh event alone is not proof
   the name was re-taken; check `schedulingStatus` too.

## Related

- Issue: [#2011](https://github.com/defilantech/LLMKube/issues/2011) (the
  finding and the fix design)
- Sibling refusals, same pattern: router-proxy objects
  ([#2007](https://github.com/defilantech/LLMKube/issues/2007), shipped in
  [#2008](https://github.com/defilantech/LLMKube/pull/2008)) and the managed
  HPA ([#2009](https://github.com/defilantech/LLMKube/issues/2009), shipped in
  [#2010](https://github.com/defilantech/LLMKube/pull/2010)); for the HPA case
  see "An HPA with the service's name already exists" in the
  [autoscaling guide](../../site/guides/metrics-driven-autoscaling.md)
- The refusal is one of the operator's ownership guards: objects LLMKube did
  not create are never adopted, overwritten, or deleted, and a refused
  sequence creates or updates nothing past the refused step
- Same defect class, still unguarded: the Service upsert in
  `reconcileService` (#2086)
