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
- `pfGuid` is the function's port GUID, eight colon-separated octets, lower
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
to `mlx5_core` and back under the node's CNI lock to read its port GUID
(port 1's GID 0; the node GUID only when the port exposes no GID table),
unless the daemon already knows it or the Pod's own `Ready` carries it. The
member is merged under resourceVersion fencing, retried on conflict, siblings
preserved. The record is written first, as intent, and promoted to a
confirmed publication once the write is acknowledged, so a process that dies
at the write claims nothing; the configuration ADD saves when it completes is
that promoted record. A merge the API server rejected undoes the record,
putting back the one a previous publication of this sandbox left or removing
it. A write whose outcome is unknown is confirmed from the authoritative copy;
when that read fails too, the record stays intent: this sandbox's DEL settles
it against the authoritative copy before it withdraws anything, withdrawing
only a member it published, the one it set out to publish or the one its
previous confirmed publication left behind on a retry; once the retry is
confirmed that previous member is superseded and forgotten. Ownership is decided
from every later record of the function: a confirmed later publication owns the
member; intent alone does not stop the sandbox that did publish from
withdrawing the member it published, and a member a later write replaced it
with is left for that sandbox's DEL. Then the plugin blocks until `Ready` appears for the key on the
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

The manifests under `deployment/readiness/` are reference manifests: they
carry the pod template each shape needs and place it on `amd64` and `arm64`
nodes, the architectures the image is built for. Adapt the image reference
and add your cluster's InfiniBand node selection before applying them. The
handshake was validated with an equivalent rendering of the daemon shape on
arm64 nodes, with the same container, arguments and mounts.

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
other configuration runs in-process whether or not a daemon is installed,
unless its definition names a `daemonSocketPath`, which forwards every command
of that network; the daemon refuses configurations whose setup or teardown would exec a
delegate such as IPAM. A discovered socket that nothing listens on, as a daemon
killed without cleaning up leaves behind, is treated like an absent one: the
command runs in-process. A `daemonSocketPath` named in the definition never
falls back; its daemon being unreachable is reported. Deploy `deployment/readiness/ib-sriov-cni-daemon.yaml`.
The privileges it needs and why are under [Security considerations](#security-considerations);
its timings and metrics under [Observability](#observability).

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
| any | any | `publishPfGuid` unset | legacy behavior, unchanged, in-process unless the definition names a `daemonSocketPath` |
| with handshake | legacy gate only | `publishPfGuid: true` | every ADD fails at the timeout |
| with handshake | writes `Ready` | `publishPfGuid: true` | handshake; a retry re-reads the device unless the daemon's GUID cache still holds it |
| knows `pfGuid` in status | echoes `pfGuid` | `publishPfGuid: true` | handshake; retry never rebinds |
| older than the `pfGuid` field | echoes `pfGuid` | `publishPfGuid: true` | every ADD fails: unknown field |

Roll the plugin before a controller that writes a new status field.

## Security considerations

What runs with which privileges, who can influence it, and what a compromise
of each part would allow.

### Components and their privileges

**The plugin process.** Execed by the container runtime as root on the host
for every ADD, DEL and CHECK, like any CNI plugin. In legacy configurations
it touches sysfs, netlink and the Pod's network namespace. With the readiness
handshake it also reads and patches one Pod object through the API server,
using the node-local kubeconfig under `/etc/cni/net.d/ib-sriov-cni.d/`.

**The installer (`thin_entrypoint`).** A privileged DaemonSet container that
copies the plugin binary onto the host and, with `--gen-kubeconfig`, writes the
node-local kubeconfig from its own service-account token and refreshes it.

**The node daemon (`ib-sriov --daemon`).** A privileged DaemonSet container
with the host network namespace and host mounts for the CNI binary and
configuration directories, the CNI lock and cache, the network namespace
directories, the socket directory and the log directory. It executes the same
CNI code the plugin would, on behalf of the plugin process, so it needs
everything the plugin needs. It does not share the host PID namespace: it
opens the netns path each request names through the mounted `/run/netns` and
`/var/run/netns` directories, so the runtime has to pass paths under them, as
containerd and CRI-O do, rather than `/proc/<pid>/ns/net`.

### Who can influence what

**Network-definition authors.** A namespaced NetworkAttachmentDefinition is
tenant-controlled input to a root process, so each field it can set is
bounded:

- `logFile` names a file under `/var/log/ib-sriov-cni/plugin/` only. The
  directory part of the value is dropped, `.` and `..` are refused, and an
  existing entry in that directory that is not a regular file is refused rather
  than followed. The directory is the plugin's alone; the daemon's own log lives
  one level up and the daemon refuses to place it in here, so an author cannot
  name the daemon's file and rotate it away from the daemon's open descriptor.
  The check trusts the directory's ancestry: it is created root-owned on a
  root-owned path, and a symlink planted in its place would need host access
  that a definition author does not have. The leaf check is a plain `lstat`
  before open, not an atomic guard against a hostile parent.
- `logFileMaxSize`, `logFileMaxBackups` and `logFileMaxAge` are reduced to
  1024 megabytes, 30 files and 90 days when set above them, so a definition
  cannot defeat the disk-usage bound of the host log.
- `daemonSocketPath` can route an attachment to a socket of the author's
  choosing. Before dialing a configured path the plugin requires an absolute
  path, reached without a symbolic link, to a Unix socket owned by the user
  the plugin runs as (root), so a definition cannot hand the request to a
  socket another user created. The plugin sends only the CNI invocation
  (arguments, netns path, configuration), never its environment or
  credentials, and the daemon it reaches refuses configurations that would
  exec a delegate.
- `publishPfGuid` makes the plugin write to the Pod being set up, and only to
  its own annotation key, fenced by the Pod's UID and resourceVersion. It
  cannot make the plugin write to any other Pod.
- `kubeconfig` names the kubeconfig the standalone plugin uses; the default is
  the node-local file the installer writes. An author who could place a file
  on the node would already be node root.

**Pod authors.** A Pod's Multus selection decides which definition an interface
resolves to; an ambiguous selection fails the ADD rather than guessing.

### API access

Both readiness shapes grant `get` and `patch` on Pods; the daemon shape adds
`list` and `watch` for its informer. The grant is cluster-wide in the shipped
manifests because RBAC cannot scope Pods to a node: the informer asks the API
server for the Pods of one node with a field selector on `spec.nodeName`, the
server applies it, but it is a filter the client requested, not an
authorization boundary.

Scoping differs by shape. The standalone plugin issues namespaced `get` and
`patch` requests, so binding the same ClusterRole with per-namespace
RoleBindings instead of the ClusterRoleBinding confines it to those
namespaces. The daemon's informer lists and watches Pods across all
namespaces, a request per-namespace grants do not authorize; with only
RoleBindings the cache never synchronizes and the daemon refuses to serve.
The daemon therefore keeps the cluster-wide `list` and `watch`; its `patch`
could be narrowed separately, but the shipped manifest does not do so. The
informer is filtered to the daemon's node; a cache miss falls back to a live
read of the Pod the runtime named, with no node check. The boundary is root-only
invocation and trusted runtime input, not per-node API authorization.

Every write is a patch of a single annotation on a Pod the plugin has just
read, conditioned on that read's resourceVersion, and it preserves every other
member of the annotation. A rejected precondition is retried from a fresh
read, never forced.

### The socket

The daemon owns `/run/ib-sriov-cni`, created and kept at mode `0700`, so only
root processes on the node can connect. A request carries the invocation as
data and a deadline; the daemon clamps the deadline to its own ceiling,
refuses callers beyond its in-flight limit at once, and cancels a request whose
caller disconnects. A failed daemon call is never retried in-process by the
plugin, so a device operation runs at most once per invocation.

### Data on the node

The CNI cache under `/var/lib/cni/ib-sriov` holds each attachment's
configuration, including the UID of the Pod it was published for, the member
it published, a previous member a retry may have left behind, and whether the
publication was confirmed. The log
files under `/var/log/ib-sriov-cni` (the daemon's) and
`/var/log/ib-sriov-cni/plugin` (the plugin's) are mode `0600` and record Pod namespace
and name, PCI addresses and GUIDs. Both directories are root-owned; read access
to them is node-root access. The kubeconfig under `/etc/cni/net.d/ib-sriov-cni.d`
carries the DaemonSet's service-account token and is refreshed ahead of
rotation. The image is distroless and has no shell.

### Blast radius

A compromised plugin process is root on the node already; the handshake adds
API access limited by the role above. A compromised daemon has the same node
access plus the ability to patch annotations on any Pod the role reaches,
which with the cluster-wide binding is every Pod: it could publish false
connection details or remove another attachment's member, and a controller
that trusts them could bind or unbind the wrong function. Namespace-scoped
RoleBindings bound the standalone plugin to the tenants that use the feature;
the daemon's informer lists and watches Pods across namespaces, so its
ClusterRoleBinding stays, and confining its patch access means a separate
namespaced patch grant beside a cluster-wide list and watch grant. Nothing here
grants access to Secrets, Nodes or other resources.

## Observability

The plugin logs one `CNI request completed` summary for each ADD, DEL, and
CHECK callback. The node daemon logs a separate summary for each accepted
request. Existing `cni command served` messages describe handler completion;
the new daemon summary describes the final response attempt.

### Reading the timings

`mode=shim` measures the host callback, including route selection, forwarding,
and printing the ADD result. `mode=standalone` measures the local callback.
`mode=daemon` measures the accepted request after validation and deadline checks,
through handler execution, cancellation handling, and the response-write attempt.
A successful write does not prove the client received the result.

A readiness deadline is classified precisely by the daemon. Its CNI error
envelope carries no telemetry fields, so the shim reports that reply as an
`error`; a deadline in the shim's own transport is a `timeout`.

Summaries include the command, result, elapsed time, error stage, Pod identity,
interface, device ID, phase timings, readiness outcome, live Pod GET count,
PATCH attempts/rejected writes, and GUID discovery attempts. Identities are
log fields only. CNI stdout still contains only the protocol result.

| Phase | Work measured |
| --- | --- |
| `config_identity` | Configuration and Pod/network identity resolution |
| `device_lock` | First node lock acquisition, including contention |
| `ownership_lock` | Node lock that makes record-then-publish, and decide-then-withdraw, one step each against the other |
| `device_inspection` | PCI index lookup and GUID read, including vfio-pci restoration |
| `guid_cache` | PCI index lookup only: the GUID came from memory, the daemon's cache or the Pod's retained Ready, so no driver was touched |
| `annotation_merge` | Publication read/merge/PATCH loop, including conflict backoff |
| `readiness_wait` | Observed Ready convergence: controller work, cache delivery, and poll delay |
| `netns_setup` | Post-readiness device information and network namespace setup |
| `final_lock` | Lock acquisition before final device setup or VF teardown |
| `device_finalize` | ADD device setup and local CNI configuration save |
| `withdrawal` | DEL's feature check and connection-details withdrawal |
| `cleanup` | Remaining DEL work, excluding its separately timed lock wait |
| `transport` | Shim forwarding; log-only, not a daemon phase |

Top-level phases do not overlap. Configuration/identity and cleanup can have
multiple segments; each segment contributes one phase-histogram observation.
Sum their durations for a per-request phase total. These phases do not cover
every instruction, so their sum can be smaller than the total request duration.

Daemon `--log-level=4` adds `CNI phase detail` records for index scanning,
driver unbind/bind operations, GUID discovery, and deferred vfio restoration.
These are inclusive child timings; **do not add them to the parent phase**.
The restore duration includes its own unbind/bind children. Discovery attempts
are counted without logging each poll. Inspection fails if restoration fails,
even when the GUID read succeeded.

Logging uses stderr and the optional node-local files. The plugin's file is
shared by every plugin process on the node: each appends to it, and a process
that starts and finds it over its size limit renames it aside under a lock, to
a name no backup has, so no process rotates or overwrites a file another still
writes to; backups stay uncompressed
and are pruned once old enough to be no process's open file. The daemon's file
is its own and rotates in-process. Keep the two files separate. Summaries do not dump network configuration, annotations,
credentials, or error bodies.

### Enabling daemon metrics

Metrics are disabled by default. Add a daemon argument, not a NAD field:

```text
--metrics-bind-address=127.0.0.1:9808
```

Scrape `GET /metrics` on that address. The listener is separate from the private
CNI Unix socket and cannot execute CNI commands. It has bounded HTTP read,
write, idle, and shutdown timeouts. Scrapes collect in-memory values only;
they do not query Kubernetes, UFM, or sysfs.

The endpoint has no built-in authentication or TLS. Loopback is suitable for
local inspection. To scrape remotely, explicitly configure a reachable address
and restrict access using the deployment's firewall or network controls. No
Service, scrape configuration, or public listener is installed automatically.
An explicitly configured address that cannot bind prevents daemon startup;
a later metrics-listener failure is logged without interrupting CNI requests.

Each daemon owns a private registry. Counters reset on daemon restart and series
appear when first observed. There is no standalone metrics server or shim
metrics-reporting protocol.

| Metric suffix (prefix `ib_sriov_cni_`) | Type | Labels |
| --- | --- | --- |
| `request_duration_seconds` | Histogram | `command`, `result` |
| `phase_duration_seconds` | Histogram | `command`, `phase`, `result` |
| `requests_inflight` | Gauge | `command` |
| `readiness_total` | Counter | `outcome` |
| `live_pod_gets_total` | Counter | `reason`, `result` |
| `request_rejections_total` | Counter | `reason` |

Commands are `ADD`, `DEL`, and `CHECK`; results are `success`, `error`,
`timeout`, and `canceled`. Unknown labels map to `other`, never arbitrary input.
No Pod, namespace, NAD, GUID, PCI address, sandbox, or raw error labels exist.
Use scrape-target labels for node identity.

Readiness outcomes are `first_read_ready`, `waited_ready`, `timeout`,
`canceled`, and `error`. First-read Ready still follows publication: it does
not skip the details update or prove a new UFM request was made.

Live GET reasons are `cache_miss`, `periodic_recheck`, `stale_write`,
`current_confirmation`, and `standalone`. A GET observation counts an actual
Pod client operation, including errors; it excludes cache polls and remembered
negative answers. Client-go internal HTTP retries are not separate observations.
The informer's LIST/WATCH traffic is not included.

Rejected requests (`unsupported`, `stopping`, `capacity`, `malformed`,
`expired`) do not enter the request histogram or in-flight gauge; `stopping`
is the shutdown drain refusing new requests. Admission is immediate; there
is no queue-wait measurement. HTTP 200 with a CNI error is counted as an error,
and cancellation after handler success cannot produce a success sample.

Histogram buckets, in seconds, are:
`.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 240, 600, +Inf`.
The request histogram's `_count` is the completed-request count; do not add
shim callback counts to it. For example, per-node ADD p95 is:

```promql
histogram_quantile(0.95,
  sum by (instance, le) (
    rate(ib_sriov_cni_request_duration_seconds_bucket{command="ADD"}[5m])
  )
)
```

Compare latency with results, in-flight requests, readiness outcomes, rejected
requests, and fallback GETs. The Ready wait measures observed convergence,
not UFM latency alone. Phase measurements support performance investigation;
they do not change publication, readiness, retry, or driver behavior.
