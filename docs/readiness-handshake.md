# The readiness handshake

How a PF passthrough attachment tells the InfiniBand controller what it was
given, and waits until the controller has put it on the fabric, before the
container runtime is told the network is up.

## Why

A PF bound to `vfio-pci` has no network interface the plugin could configure;
what makes it usable is the fabric: the controller must place the function's
GUID in the partition (PKey) the network definition names. Historically the
plugin gated on a Pod annotation the controller set once it had done that
(`mellanox.infiniband.app=configured`) and failed ADD until then. With Multus
invoking delegates serially and the runtime tearing the sandbox down on every
failure, a machine with several attachments could spend minutes in
create-fail-retry cycles while the controller was working, and every retry
rebound hardware.

The handshake replaces the gate with a conversation on the Pod object: the
plugin publishes exactly what it allocated, then blocks until the controller
confirms the binding. One ADD per attachment on the happy path, no sandbox
churn, and a retry after a real failure reuses the binding already made.

## Actors

| actor | writes | reads |
|---|---|---|
| ib-sriov-cni, the producer | `v1.ib-sriov-cni.io/connection-details` | `v1.ib-sriov-cni.io/connection-status`, `k8s.v1.cni.cncf.io/networks` |
| the InfiniBand controller (ib-kubernetes), the consumer | `v1.ib-sriov-cni.io/connection-status`, the fabric | `v1.ib-sriov-cni.io/connection-details`, Pod lifecycle |
| Multus and the runtime | `k8s.v1.cni.cncf.io/networks`, `CNI_ARGS`, `CNI_IFNAME` | the CNI result |

Neither side writes the other's annotation, and neither removes the other's
members.

## Wire contract

Both annotations are JSON objects keyed by the attachment key
`<NAD namespace>/<NAD name>/<PCI BDF>`. Including the BDF keeps repeated
attachments of one definition distinct.

The annotation keys live under `v1.ib-sriov-cni.io/`, a versioned prefix the
project owns: Kubernetes reserves unprefixed keys for end users, and the working
group's own keys under `k8s.v1.cni.cncf.io` are the network attachment spec, not
plugin state. A future incompatible schema gets a new prefix, not new fields.

The producer's member:

```json
{
  "tenant-a/ib-pf/0000:03:00.0": {
    "nameSpace": "tenant-a",
    "networkName": "ib-pf",
    "deviceID": "0000:03:00.0",
    "pfGuid": "e0:9d:73:03:00:fe:b0:a4",
    "deviceIndex": 0
  }
}
```

- `deviceID` is a canonical lower-case PCI BDF; it is validated before it
  reaches a sysfs path or an annotation key.
- `pfGuid` is the function's node GUID, eight colon-separated octets, lower
  case. The all-zero and all-ones values are placeholders and are never
  published or reused.
- `deviceIndex` is the node-local rank of the function among InfiniBand PCI
  functions sorted by BDF. It is not a slot number.
- `nameSpace` and `networkName` are the network definition's, resolved from the
  Pod's Multus selection: by `CNI_IFNAME` when the selection names an interface,
  otherwise by the configuration's `name` matched against the unnamed
  selections, under the preconditions the lifecycle section states. A position
  among delegates is never used.

The consumer's member:

```json
{
  "tenant-a/ib-pf/0000:03:00.0": {"Status": "Ready", "pfGuid": "e0:9d:73:03:00:fe:b0:a4"}
}
```

`Status` is required and `Ready` is its only defined value. `pfGuid` is
optional: a controller that echoes the GUID it bound lets a retry republish
without touching the device. Both readers reject unknown fields, so an
additive change to either object must roll out on the reading side first (the
plugin before a controller that writes a new status field). Absence of a
member is not a value: it means "not yet" to the producer and "rolled back" to
the consumer.

## Invariants

1. ADD publishes its member before it can succeed. There is no publish-only
   mode; publishing implies waiting.
2. `Ready` confirms a binding, not one ADD attempt.
3. A rollback DEL withdraws only its own details member. It does not release
   the fabric binding and does not touch `Ready`. A teardown DEL, one for a
   Pod that is being deleted, withdraws nothing: the members stay on the Pod
   object until it is gone.
4. An unchanged retry may reuse `Ready` only for the same Pod (by UID) and the
   same attachment key. The plugin checks those two; that the allocation and
   the policy behind the key are unchanged is the controller's part: it must
   invalidate `Ready` when either changes (invariant 5). The plugin does not
   compare the device's GUID against the retained one.
5. A release or a reassignment must invalidate `Ready` before the binding can
   authorize a different owner.
6. Sibling members are preserved on every write, and producer and consumer
   ownership is never crossed.

## Lifecycle

**ADD.** Identity first: namespace and name from `CNI_ARGS`, the Pod read and,
when `CNI_ARGS` carry `K8S_POD_UID`, checked against it; the network resolved
from the Multus selection, by the interface a selection names or, for one that
names none, by the configuration's `name` Multus passed, matched against the
unnamed selections; a position among delegates is never assumed, because
Multus may insert default-network delegates before the selections. Only when
that leaves more than one definition or none does the ADD fail and ask for
explicit interfaces. The name match rests on two preconditions Multus does not
enforce, because Multus keeps the delegate's own namespace and name to itself:
the definition's configuration `name` equals its object name (Multus fills
the name only when the author left it blank), and no Multus default network
carries the same name as a definition the Pod selects. Where either may not
hold, name the interfaces in the selection, as KubeVirt does; a Multus change
that passes the delegate's namespace and name to the plugin would remove the
preconditions, and the plugin will prefer that identity when it exists. Only
then is hardware touched: the PF is rebound from `vfio-pci`
to `mlx5_core` and back under the node's CNI lock to read its node GUID,
unless the daemon already knows it or the Pod's own `Ready` carries it. The
member is merged under resourceVersion fencing, retried on conflict, siblings
preserved. Then the plugin blocks until `Ready` appears for the key on the
Pod it published to, or `readinessTimeoutSeconds` (default 120) expires, or
the Pod is gone, replaced or being deleted. A Pod carrying a deletion
timestamp will never become ready and kubelet cannot tear its sandbox down
until the ADD returns, so that ends the wait at once. Only success returns the
CNI result; any other outcome fails the ADD and the runtime tears the sandbox
down.

**DEL.** If the Pod is being deleted (it carries a `deletionTimestamp`),
nothing is withdrawn: its details are the controller's record of what to
release, a controller that is slow or restarting must still find them, and
the Pod object takes them along when it goes. Once the object is gone they are
gone with it; a controller that needs them past that point keeps its own
record, for instance behind a finalizer. The check is made on the copy
DEL reads, and a write fenced on an older copy is rejected once the deletion
lands, so a retry reads live and sees it. Otherwise, only the sandbox that
published may withdraw, and only while it is the last to have done so: a DEL
whose sandbox has no record of publishing withdraws nothing, and neither does
one whose Pod has since published the same function from a later sandbox,
because kubelet can tear an earlier sandbox down after a later one is up and
both name the same key. The record is the configuration ADD writes to its
cache, with the Pod UID and an instant, immediately before its publication
write; an ADD that cannot record fails before it publishes. The instant is
the wall clock or, when a record on the node already carries that instant or
a later one, one nanosecond past the latest of them, so two sandboxes of one
Pod order by publication even when the clock stands still or moves back
between them. Record-then-publish and decide-then-withdraw each run as one
step under a node-wide ownership lock, released before any wait, so neither
can interleave with the other. A sandbox whose own record cannot be read
settles nothing, and so does a record of the same attachment whose instant
cannot be read: DEL then withdraws nothing, logs why, and continues device
cleanup while keeping its ownership record for a later DEL. Another record
that cannot be read at all identifies no attachment and is passed over, with
an error naming the file, so one damaged record does not stop every
withdrawal on the node. This does not schedule a retry. Atomic
cache writes use the reserved `.tmp-` filename prefix until rename; ownership
scans ignore these uncommitted files, even when their JSON is complete. Older
suffix-named temporary files are not automatically recognized or removed,
because those names can also identify committed records; an unreadable one
still makes ownership indeterminate. Within those bounds
the member this delegate published is removed, and nothing else, and a
rollback that has no namespace left removes its own record too. The Pod is
identified by UID: from `CNI_ARGS` when present, otherwise from the UID
the ADD recorded with its cached configuration, which it writes immediately
before its publication write, so a first ADD that times out has left the
record its rollback needs. With neither, which takes a runtime that passes no
`K8S_POD_UID` and an ADD that failed before it could publish, the plugin has
no evidence which Pod the attachment belonged to; the same address may since
have been handed to a same-name replacement Pod, so the withdrawal is skipped
and DEL still succeeds. Legacy teardown of VF devices is unchanged.

**Rollback and retry.** When an ADD fails after publishing, the runtime issues
DEL and tries again. The DEL withdraws the member; the controller keeps the
binding and `Ready`, because details absence means rollback, not release. The
retry republishes the same allocation, and its wait is satisfied by the
retained `Ready` at once. If the controller echoed `pfGuid`, the retry does
not rebind the device either.

**Release.** The controller releases a binding when the Pod is deleted or
replaced, or when the same key is republished with a different allocation. On
deletion it can rely on the details members being there until the object is
gone, whatever order kubelet's teardown and its own reconciliation run in.
There is no signal for one attachment removed from a Pod that stays alive;
that attachment stays bound until the Pod goes.

## What the controller must do

Watch Pods; for each details member, bind `pfGuid` into the partition the
network definition names, then write `Ready` under the same key, echoing the
GUID if it can; keep `Ready` across a rollback; release and clear `Ready` on
Pod deletion or replacement, or when the key's allocation changes. A
controller that never writes `Ready` makes every publishing ADD fail at the
timeout. That is a controller gap, not a plugin fault, and it is how the two
sides' failures are told apart:

| observation on the launcher Pod and logs | side |
|---|---|
| ADD fails with `timed out waiting for "<key>" to become ready`; details carry the key, status lacks it or is not `Ready` | controller |
| details never appear; the plugin or daemon log shows the ADD failing before `connection-details published or current` | plugin, or its API access on the node |
| ADD fails with `parse v1.ib-sriov-cni.io/connection-status`: the controller wrote a field this plugin does not know | version skew; roll the plugin first |
| `connection-details ready` is logged for this attachment and this ADD still fails afterwards | plugin |
| `connection-details ready` is logged and the sandbox is torn down anyway, with no later error from this plugin | another delegate or the runtime; read the failing command's own error |
| ADD succeeds and the guest sees the device, but the GUID is not a member of the partition | controller |

Read the failing command and its exact error, not the presence of `Ready`: a
sandbox is torn down when any delegate fails, and this plugin's success is
recorded before later delegates run.

## Two ways to run the plugin

**Standalone.** The runtime execs the plugin; it reads and patches the Pod
through the API server with a node-local kubeconfig, and its readiness wait
polls the API server. Deploy `deployment/readiness/ib-sriov-cni-standalone.yaml`.

**With the node daemon.** `ib-sriov --daemon` installs the plugin binary and
the node kubeconfig, then serves ADD, DEL and CHECK for its node over a Unix
socket from one Pod informer filtered to that node. A wait that ends within a
few seconds is served from the cache alone; a longer wait re-reads the API
server about every five seconds per waiting attachment, because a watch can
stall without saying so, and a cache miss or a rejected write also reads live.
On SIGTERM the daemon stops accepting requests, cancels the waits, drains for
up to five seconds and then waits up to thirty seconds for requests still
finishing, so an ADD caught mid device rebind restores `vfio-pci` before the
process exits, provided the rebind returns within that budget; a bind the
kernel holds longer is named in the exit error and the function may be left
on the wrong driver until its next ADD. The Pod's termination grace period
must exceed the sum of the two waits. The daemon also remembers each function's GUID for its
lifetime, learned only from device reads inside ADD and from a retry's own
`Ready`; a restart starts empty, so a fresh Pod's first ADD reads the device
again while a retry that carries `Ready` does not. The plugin process looks for the socket at
`/run/ib-sriov-cni/ib-sriov.sock` only for publishing attachments; every
other configuration runs in-process whether or not a daemon is installed, and
the daemon refuses configurations whose setup or teardown would exec a
delegate such as IPAM. A discovered socket that nothing listens on, as a daemon
killed without cleaning up leaves behind, is treated like an absent one: the
command runs in-process. A `daemonSocketPath` named in the definition never
falls back; its daemon being unreachable is reported. Deploy `deployment/readiness/ib-sriov-cni-daemon.yaml`.

The stock `deployment/ib-sriov-cni-daemonset.yaml` deploys neither shape and
is unchanged; a cluster that never sets `publishPfGuid` needs nothing here.

## Timeouts

`readinessTimeoutSeconds` bounds one attachment's wait, 120 seconds by default
and at most 3600.
Multus invokes delegates serially, so a Pod's attachments wait one after
another and the sum, plus device work, is what the runtime's own sandbox
deadline sees. Size the value to the controller's real turnaround and to the
number of attachments per Pod, and keep the product under the runtime's
sandbox creation deadline; CNI does not expose that deadline to the plugin.

## Compatibility

| plugin | controller | network definition | result |
|---|---|---|---|
| any | any | `publishPfGuid` unset | legacy behavior, unchanged |
| with handshake | legacy gate only | `publishPfGuid: true` | every ADD fails at the timeout |
| with handshake | writes `Ready` | `publishPfGuid: true` | handshake; a retry re-reads the device unless the daemon's GUID cache still holds it |
| knows `pfGuid` in status | echoes `pfGuid` | `publishPfGuid: true` | handshake; retry never rebinds |
| older than the `pfGuid` field | echoes `pfGuid` | `publishPfGuid: true` | every ADD fails: unknown field |

Roll the plugin before a controller that writes a new status field.
