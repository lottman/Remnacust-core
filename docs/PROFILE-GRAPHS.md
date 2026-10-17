# Independent profile graphs (unreleased)

`core.NewProfile(ctx, *core.Config)` creates an unstarted, owned feature graph
inside the existing process. Unlike `core.New`, it does not reload process
environment defaults or replace the default DNS/dialer providers and logger.
The caller must start and close the instance and coordinate listener conflicts,
admission and draining. This API alone is not a node port-plan manager.

Each graph owns its DNS, policy, routing, outbound, statistics, observatory and
other configured features. Tags are local to the graph. Pass the owning node
instance's `RuntimeContext()` when creating a managed profile: its mandatory
user-quota requirement is inherited, including requirements committed after
profile creation. Configuration snapshots, updates and restart journals preserve
that requirement. The caller's input configuration is copied, not modified.
Independent graphs do not borrow the parent's admission lock or shutdown signal.
Their manager remains responsible for closing them during process shutdown.

`core.NewManagedProfile(parent, config)` adds ownership for node-managed graphs.
It borrows the parent's enabled statistics manager while retaining its own
controller, admission lock and shutdown signal. Configuration construction does
not restart or close shared accounting. Parents retain live children and close
them before their own statistics; completed children leave the registry, so
repeated replacements do not retain closed graphs. Construction is pinned until
it registers the child or finishes cleanup. A borrowed feature generation cannot
act as a lifecycle parent. A managed profile starts only after its owning
instance has completed initialization; closed instances cannot restart.

Staged profiles carry an explicit preparation fence in addition to their staged
TCP listener ownership. They are not management-ready and cannot dispatch traffic
through either dispatcher entry point before activation. Feature generations
inherit the fence. Whole-plan startup also carries the fence on the owner and
its children until all graphs start. Topology publication clears it only after
the durable socket decision and complete in-memory roster/configuration update;
the low-level TCP commit holds new graph admission barriers through activation.

`AccountingOwner()` keeps outside-grant usage registered against the actual node
source, including calls through runtime generations. Closing one profile does
not unregister that source. TCP, UDP and Unix native workers are registered with
their instance: shutdown stops admission, interrupts connections and transports,
and waits for workers before closing statistics. UDP cleanup startup is gated
against worker shutdown. This does not itself implement cross-profile plan
transactions or select a node delivery capability.

Connection-context access/error records and Lua logging use the graph's logger
and severity. Detached Xray contexts retain the owning graph. An isolated context
without a logger does not fall back to a neighbouring graph. Diagnostics emitted
with `context.Background()` still use process logging; this is not a claim that
every third-party diagnostic has been isolated. Legacy root initialization keeps
its process-default behavior.

## Verification and boundaries

### TCP listener ownership (unreleased)

Native TCP listeners now use an accounting-root socket pool when their context
carries an owning Core instance. Private control listeners created with a
background context and Unix sockets keep their existing ownership. Feature
generations reuse their profile's logical listener identity; unrelated profiles
cannot silently share an active TCP endpoint through `SO_REUSEPORT`.

`NewStagedManagedProfile` prepares a replacement graph without taking TCP
acceptance from the current graph. `CommitProfileTCPListeners` validates the
entire requested TCP switch, executes the supplied checkpoint callback, and
changes acceptance only if that callback succeeds. Kernel socket-option changes
on an occupied endpoint are rejected. The caller must hold the owning management
lock and must not enter the socket pool from the persistence callback.
If persistence can fail after a rename, that callback must restore the previous
whole manifest before returning an ordinary error. An uncertain restore must
fence root admission and require recovery; the generic socket pool cannot infer
the outcome of a storage operation supplied by its caller.

Accepted connections retain their original profile, including handshakes in
progress. `TCPProfileDrained` becomes ready after retirement and closure of all
owned TCP connections; the graph manager must then close the retired instance.
Closing the accounting root also closes connections whose logical listener has
already been removed. Queues and overlapping listener generations are bounded.
Raw socket inspection and splice detection unwrap the ownership layer without
changing the original connection's close responsibility.

This is a TCP ownership API, not complete graph-delivery support. UDP/QUIC
demultiplexing, production Panel/Node delivery, installed credential-revocation
coverage and the full financial recovery lifecycle
remain separate requirements. The public delivery capability remains disabled.
Until packet-flow ownership exists, staged profiles reject UDP/QUIC binds before
opening a socket, including final-mask paths. Normal active UDP listeners retain
their existing behavior. Hysteria, MASQUE, XHTTP/3 and WireGuard socket creation preserves
the owning context instead of replacing it with `context.Background()`; MASQUE
keeps its own cancellation while retaining the parent context values.

### REALITY listener shutdown

The pinned REALITY dependency's asynchronous `NewListener` can race a handshake
result send against closure of its result channel. This was reproduced with the
race detector in XERA-HTTP's real REALITY stream-one scenario. XERA-HTTP, XHTTP
and gRPC now use the local `reality.NewListener(ctx, inner, config)` lifecycle
wrapper, retaining the official `reality.Server` handshake and detection logic.
No dependency revision, credentials or wire protocol changed.

The wrapper closes a cancellation signal instead of the producer queue, limits
pending handshakes and queued results, closes pending raw sockets on shutdown,
and leaves already-transferred connections with their caller. Its context
retains the owning profile's values. Regression tests force successful late
handshake returns after shutdown and verify that closing the listener does not
interrupt an already-accepted TCP connection. Full REALITY, XERA-HTTP, XHTTP and
gRPC package tests passed with `-race` after the fix.

The existing native live-apply API now replaces DNS, per-level policy and
context logging with the routing/outbound/observer generation. Existing streams
keep their admitted generation; new streams use the committed one. Generations
borrow the same statistics manager and root quota/shutdown owner, so changing
DNS or policy does not reset counters or unregister the quota receipt source.
Protocol handlers select per-level policy from the connection context instead
of the handler's original constructor. MASQUE IP tunnels also retain their
admitted dispatcher/context for internal flows and peer accounting.

System policy counters remain static because native inbound byte counters are
installed when listeners are constructed. API configuration, native port/wire
settings, process settings and other static features still follow their existing
maintenance restrictions. This change does not claim hot replacement of every
feature, same-port transport or authentication configuration.

The DNS/policy regression sends real VLESS traffic to the same domain and port
on two loopback addresses. After commit, the old connection reaches the old
address and a new connection reaches the new address. It checks retained user
counter identity/value, generation API views, durable checkpoint and process
logger ownership. Invalid DNS, duplicate feature records and changes to native
system counters are rejected without replacing the running revision.

DNS retirement now cancels pending cached and system-resolver queries, stops
cache/subscription/UDP request-cleanup tasks, closes owned HTTP/2 sockets and
QUIC connections, and interrupts blocked TCP requests. Cache updates, retries,
subscriptions and connection-pool admission cannot restart a retired instance.
The default localhost fallback uses the same four-second query timeout as an
ordinary configured nameserver; it no longer relies on ignored cancellation.

Local tests exercise active HTTP/2 requests, an established QUIC connection,
cancellation during QUIC handshaking, blocked TCP, UDP retry/task races, late
cache responses and system lookup cancellation. The VLESS generation test also
checks that a draining generation retains its DNS until its final stream ends,
then retires it while the current generation and original stream remain usable.
These checks do not establish the availability of external resolver services.
The upstream AdGuard QUIC tests require a reachable public UDP/853 endpoint: the
local complete attempt failed there, and a separate direct nine-second-budget
QUIC probe also timed out after eight seconds. Local green regression runs
exclude those three tests explicitly; the upstream tests remain unchanged.

The native VLESS integration test creates two graphs with identical tags and
different DNS, policies, API listeners, routes, logs and checkpoint files. It
changes A's route, verifies that existing A/B streams survive, that new A traffic
uses its new route, and that B's route and checkpoint remain unchanged. Closing A
leaves B functional. Separate tests cover quota inheritance before/after profile
creation and autonomous restart from the saved profile checkpoint.

The managed variant uses actual shared counters with VLESS traffic and confirms
that route changes and closing A preserve B's accounting. Lifecycle tests cover
failed construction, concurrent construction/shutdown, nested profiles, final
worker accounting, source registration and startup readiness. Hysteria's UDP
session close flag is atomic and its manager cleanup reads closure under the
manager lock; a concurrent write/close regression and native Hysteria traffic
exercise retirement without the previously observed data race.

These tests do not establish integration with the panel's port-plan compiler or
the node's delivery protocol. Those currently use the existing combined graph.
Do not advertise an independent-profile capability until that protocol, process
restart, quota/statistics aggregation and installed end-to-end checks are wired.
Same-port transport/auth replacement, remaining static-feature changes,
QUIC/mux draining and compatibility across client applications need their own
verification. JSON configuration building can itself apply process
environment settings before this constructor; callers must not treat arbitrary
JSON decoding as an isolated operation.

Native TCP, UDP and Unix workers retain the owning context when constructing a
listener, without retaining the cancellation/deadline of a management request.
This keeps transport setup and its logs inside the profile; a cancelled request
does not own the lifetime of an already admitted persistent listener.

The panel now has a separate `compileNodeProfileGraphs` representation, with one
complete graph and content hash per binding. It preserves original tags and full
profile settings, derives physical TCP/UDP occupancy from the transport and
checks conflicts across graphs. Nonempty process `env` is rejected because JSON
building currently mutates the process environment. The legacy combined-graph
compiler and delivery protocol are still active; the new representation alone
does not enable graph delivery or relax the legacy checks.

### Atomic updates of already running graphs

`command.ApplyRuntimeProfileBatch` prepares all supplied managed graphs before
changing their active routing or credentials. It checks each expected revision,
locks their independent admission barriers together, installs the new user maps,
then invokes one persistence decision for the complete supplied set. A failed
prepare or persistence decision rolls back user maps and disposes staged
listeners/generations. Successful persistence is followed only by in-memory
activation; cancellation after the durable decision does not partially cancel
activation. Existing sessions keep their pinned generations. Unchanged retries
do not persist again. Overlapping batches serialize controller acquisition.

The persistence callback is an internal integration contract, not a completed
fleet journal by itself. The enrolled path below supplies the native checkpoint.
The callback must provide an unambiguous atomic durable decision; a failed
decision must leave the old durable manifest intact. Stores with uncertain
rename/fsync or transaction outcomes need recovery before using this primitive.
The fleet owner must include all its graphs in that manifest and serialize
subsequent API mutations through it. This primitive alone does not add/remove
managed graphs, enroll them into a manifest, restore a process, expose RPCs, or
replace the node's combined delivery protocol.

Native VLESS tests cover two same-tag graphs, stale second revisions, a controlled
persistence failure, a failed second native bind with release of the first staged
listener, credential rollback, activation despite caller cancellation
after the commit decision, and unchanged retry. These use a controlled callback,
so they are not disk-crash or installed-service persistence evidence.

Credential retirement now identifies the owning profile as well as its local
inbound tag. Retiring an old generation or removing a user from A cannot close
B's same-tag/same-user connection. The native HandlerService removal defers stream
revocation until after its checkpoint succeeds; a failed checkpoint can restore
the user without having already destroyed its connections. Explicit global
device revocation remains node-wide. Both the configuration reconciliation path
and HandlerService removal are checked with native same-tag VLESS traffic.

### Native whole-plan checkpoint and restart

`core.WithProfileBindings` records complete compiled graph configurations and
binding/profile/inbound UUID provenance in server-only `ProfilePlanType` metadata.
The enclosing native checkpoint retains its existing checksum, size limit,
private-path checks and atomic file replacement. Nested plans, duplicate binding
identities, invalid UUIDs, malformed handler/feature entries and oversized graphs
are rejected before graph construction. This is binary server metadata; clients
receive their ordinary transport configurations.

Starting a root carrying this metadata requires a durable runtime checkpoint.
It constructs and starts managed graphs while admission remains closed across
the plan, then opens admission after every graph starts. A failed later bind
closes already created graphs and leaves admission fenced. Shutdown owns all
managed instances and retains shared accounting until their workers finish.
Child initialization validates the already flushed snapshot, including inherited
quota requirements, instead of rewriting the whole manifest once per graph.
The existing native checkpoint startup path loads the same metadata, so it can
reconstruct graphs without contacting the panel.

Enrolled graph updates write the complete root manifest, retaining unchanged
graphs. Their controllers share `ManagementAccess()` on the accounting owner;
each still owns its own routing generation, admission barrier and controller
state. Controller callbacks must not recursively enter management operations.
`ApplyEnrolledProfileBatch` provides the actual manifest persistence callback and
creates internal controllers for graphs without an API listener. Subsequent
ordinary controller updates and HandlerService credential mutations use that
same checkpoint, rather than a stale per-graph file. Controller publication and
its shutdown callback are installed together.

If a checkpoint replacement fails, persistence attempts to restore the complete
previous manifest before returning an ordinary failure. If that restoration also
fails, the owning node irreversibly fences new transport leases and new streams
over cached transports until administrator recovery/restart. Existing admitted
streams retain their generation. This prevents a later request from silently
reopening admission against an uncertain disk decision.

Native tests now verify a real file-backed two-profile batch, a subsequent
single-controller route update, complete-manifest readback, shutdown, and a
separate OS process restoring both profiles and serving their saved VLESS routes.
They also verify a failed second startup bind releases the first listener and
that the uncertainty fence blocks both new leases and cached-stream admission.
These are Core process tests, not an installed Panel/Node deployment test.

The panel compiler and node delivery still use the legacy combined graph.
Production node/panel delivery, UDP/QUIC membership changes and graceful removal,
per-binding statistics namespacing, the complete financial receipt lifecycle,
same-port wire replacement and installed end-to-end compatibility remain to be
integrated and verified before advertising a profile-plan delivery capability.

### Private whole-plan control and delivery replay

The existing HandlerService control transport now also registers
`remnacust-profile-control-v1/Inspect` and `/Apply`. Inspection returns the owning
checkpoint revision, process identity, binding provenance, graph revisions and
saved delivery identity; it does not return graph credentials. Application takes
a compiled root `core.Config` with server plan metadata and requires the owning
manifest hash in `if-match`. It validates that hash under the same management
lock as preparation and commit. Existing-roster updates retain their atomic batch
path. TCP roster/provenance changes use the complete-roster transaction below;
root-control changes are rejected. Full membership and production node delivery
are not yet available: inspection still reports `dynamicMembership: false` and
`nodeDelivery: false`.

### TCP roster transactions

`CommitManagedProfileTopology` publishes a prepared full roster through the same
private ProfileControl API. The coordinator holds the owner management lock from
CAS/clock validation through preparation and acknowledgement. It retains exact
unchanged siblings, prepares new independent graphs, and holds their admission
barriers and those of retiring graphs through the durable socket decision.
Binding/profile/inbound identities and actual prepared configurations must match.
One prepared instance cannot be assigned to two binding IDs. Root control
settings stay unchanged, and mandatory root/graph quota metadata is preserved.

The complete checkpoint and delivery clock are written before switching TCP
acceptance. Failure restores the previous whole checkpoint; an uncertain restore
fences root admission for recovery. Retired graphs cannot overwrite the current
checkpoint or receive new physical TCP connections. Their existing admitted flows continue
until TCP ownership drains, then the graph closes. Pending retired graphs are
bounded. Packet-using profiles are rejected by this transaction until packet-flow
ownership is implemented; active UDP/QUIC operation remains unchanged.

Credential revocation resolves the current graph and still-draining graphs of
the same logical binding. It also covers a retired inbound whose local tag was
renamed in its replacement, while preserving another binding's same-user,
same-tag traffic. Binding identity reads and publication share the accounting
owner's lifecycle lock.

Retirement does not block new streams inside an already-owned HTTP/2 transport:
otherwise an intact pooled connection would stop serving requests after a graph
switch. Management readiness and physical acceptance remain closed on retired
graphs. When a user is removed or their credentials change, retired graphs also
keep a profile-local denied-identity record, preventing later streams from
re-authenticating against their old native credential stores. The record remains
through shutdown and is removed only after the retired instance fully closes.
It does not deny the same email on unrelated graphs.

A native TLS/HTTP/2 XERA-HTTP + VLESS test retains one physical client pool across
a same-port graph replacement: a later stream still follows the old graph,
while a fresh client pool follows the new one. A subsequent ordinary enrolled
user-removal reconciliation prevents new streams from using the cached old
identity. This does not by itself implement QUIC packet routing or UDP draining.

A real private gRPC/VLESS test checks failed occupied-port staging, unchanged
disk hash and traffic after failure, removal/addition on the same TCP port,
retained sibling traffic, whole-roster and clock readback, exact replay, retirement
after native drain, same-binding provenance replacement, and revocation of both
old/new flows across an inbound rename. Full Core, dispatcher and command package
tests passed with the race detector after these changes. The same scenario now
closes the owner and starts a separate OS process from the saved checkpoint,
checks the new roster and delivery epoch, and confirms removed bindings do not
return. It removes the user's native credentials through the regular enrolled
configuration reconciliation path: old/current flows close, the sibling keeps
traffic, and the deleted credentials remain rejected after process restart.
This is Core configuration/transport evidence; installed Panel/Node device
deletion and the full quota receipt/recovery lifecycle remain separate gates.

Server-only `ProfileDeliveryType` stores positive signed-63-bit decimal strings
for delivery epoch and logical revision, the UUID owner, enabled state, input
hash and compiled source-plan hash. The source hash binds root control settings,
binding identities and every compiled graph. Mandatory server quota markers and
the delivery record itself are excluded from this source digest. The ordinary
checkpoint hash continues to describe the current actual state.

Older delivery epochs/revisions, changing an immutable lease's owner or content,
removing a saved clock, malformed identities and disabled deliveries exposing
managed graphs are rejected. A new lease with unchanged graph settings still
persists its clock before acknowledgement. Exact delivery replay is a no-op:
it preserves later imperative user/route changes instead of restoring the
original source snapshot. The response reports the actual current state. If
another delivery supersedes the request before acknowledgement, the caller must
inspect again rather than accept an acknowledgement for a different owner.

Native RPC tests extend the file-backed VLESS/process restart scenario: persist
a clock-only delivery, update A through its ordinary controller, replay the old
desired source without undoing A, advance the epoch, reject stale manifest CAS,
old epoch, conflicting owner/source and forged source digest, then restore the
clock and saved routes in another OS process. Unit tests cover 63-bit precision,
revision/mode rollback, metadata duplication and the distinction between policy
markers, source identity and root-control content.

The old `cache-runtime` maintenance command refuses to overwrite a checkpoint
containing an independent plan. It cannot silently discard its graphs or clock;
the node's main configuration handler must use the versioned profile path before
node delivery is enabled. This clock also does not replace node device-revision reconciliation,
financial source fencing, lease ownership or private transport authentication.

### Versioned local CLI input

The native CLI now provides `api cache-profiles`, `api profile-inspect` and
`api profile-apply`. Cache preparation takes `--output` and one local JSON file;
live application takes `--server` and one local JSON file. Inspection returns
bounded binding metadata. Apply/cache acknowledgements contain only the current
hash, delivery, graph count, unchanged flag and, for live apply, process identity.
Configurations and credentials are never command-line arguments or output.

The input envelope has `version: remnacust-profile-input-v1`, `root` (ordinary
Xray JSON), `delivery` (epoch, owner, revision, enabled, inputsHash) and `graphs`
(bindingUuid, configProfileUuid, configProfileInboundUuid, config). Input is
limited to 16 MiB, each root/graph JSON to 4 MiB, graph count to 512 and nesting
to 64. Duplicate JSON members and unknown envelope fields are rejected. Every
root/graph process environment is checked, case-insensitively, before any JSON
builder runs. UUIDs and delivery counters are validated before compilation.
The CLI computes the compiled source hash itself; callers do not supply PlanHash.

Cache preparation preserves existing root and retained-binding mandatory quota
requirements, rejects corrupt old checkpoints and old/conflicting deliveries,
and leaves an exact delivery replay untouched to preserve subsequent API edits.
The live command inspects the owning manifest and sends a single CAS-protected
request. Native process tests execute the real `base.Execute` dispatcher in
child processes, prepare a checkpoint, boot actual profiles, live-apply a VLESS
route while its old stream survives, and exercise legacy overwrite protection.

This also fixes CLI routing for `reconcile`, `cache-runtime`,
`inspect-checkpoint`, `require-user-quota` and `usage-receipt`: flag syntax in
UsageLine is now bracketed so the dispatcher extracts the actual command name.
Command-selection tests alone are supplemented with real CLI execution.

Node has an internal JS bridge: `makeProfileInput` verifies the panel's canonical
graph/plan hashes and provenance before translating its versioned producer plan;
`LiveCoreService.applyProfiles` and `prepareProfileRestart` write private flushed
inputs, use these commands and validate exact delivery acknowledgements. A real
panel-compiler output was accepted by that bridge, including Unicode-key order
and small numeric values. The bridge methods are not yet called by the main
node configuration handler, and the panel's delivery query still emits the
legacy combined plan. No new production capability is advertised by this work.
