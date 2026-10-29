# Gachinetes ♂ — design

Gachinetes is a Kubernetes distribution rebuilt around the gachimuchi meme.
The meme is the vocabulary; every term maps to a real gap in Kubernetes.
It is built on [kuberoot](https://github.com/kuberoot-dev/kuberoot) and ships
its add-ons as [kubepkg](https://github.com/kuberoot-dev/kubepkg) packages.

Three layers, all kept:

1. **Language** — names, CLI, events, logs, UI. The meme itself.
2. **Add-ons** — controllers, webhooks and a web app on top of any cluster.
3. **Core** — changes to storage, apiserver, scheduler and kubelet.

## Glossary

| Meme | Gachinetes concept |
|---|---|
| Boss of this gym | Leader of a component or of a shard |
| Wrestling | Contested write, contested leadership, preemption |
| Belt | Ownership of a field |
| Dungeon / Dungeon Master | Quarantine / its operator role |
| Slave | Ordinary workload identity |
| Boy next door | Placement next to a chatty peer |
| Three hundred bucks | Price of a resource or an action |
| Deep dark fantasies | Branches of cluster state |
| Sorry for what? | Causal explanation of a failure |
| ♂right version♂ | Signed, rebuilt image digest |
| Rest in peace | Undoable delete |
| Safeword | Instant revocation of elevated access |
| Aniki | Mentor: explains, reviews, suggests fixes |
| Tag team | Gang of pods scheduled all-or-nothing |
| Weight class | QoS class |
| Spotter | Second person for a dangerous operation |
| Protein | Pre-warmed nodes and images |
| Round | Checkpoint interval of a long job |
| Away match | Bursting a workload to another cluster |

`♂` appears only in display text. DNS names cannot contain it, so API groups
live under `gachinetes.dev` (for example `gym.gachinetes.dev`).

## Core

### Tape: storage as an append-only log

Kubernetes compacts etcd history, so past cluster state is lost.

The Tape is an etcd-API-compatible store (the same approach as kine, which
kuberoot already uses). The apiserver talks to it as to etcd, so watch,
informers and every existing controller keep working unchanged. Internally:

- every revision is kept in an append-only log;
- recent revisions are served hot; older segments move to object storage;
- compaction requests from the apiserver only trim the hot window.

This enables:

- **Rewind** — read the cluster at any revision or timestamp and diff two
  points in time (`gachictl rewind --to 2026-10-29T13:00Z`).
- **Replay** — re-run the event stream of an incident.
- **Graveyard** — deleted objects stay readable and restorable (see below).
- **Audit** without a separate audit pipeline.

### Rest in peace: undoable delete

A delete moves the object into the graveyard for a retention window instead of
erasing it. A new subresource restores it:
`gachictl undelete ns/prod --as-of 5m`. Restores replay the object and its
dependents in owner order. The graveyard is an ordinary read-only API.

### Deep dark fantasies: branches of cluster state

`--dry-run=server` checks one object; nobody learns what controllers, the
scheduler and autoscalers will do in response.

A fantasy is a copy-on-write branch of the Tape at a revision. Gachinetes
starts a branch apiserver plus the real scheduler and controller-manager
against it, applies the change and runs to quiescence. The result is a report:
new and changed objects, placements, preemptions, cost delta. A fantasy can be
merged (its writes are replayed on the trunk with conflict checks) or dropped.

```
gachictl fantasize -f upgrade.yaml --until-quiet 5m
```

Branch workloads never start real containers: branch kubelets are simulated.

### Belts: field ownership enforced by the apiserver

Today concurrency is optimistic (`resourceVersion`, 409, retry), and
server-side apply `--force` simply takes a field. Controller wars are possible
by design.

In Gachinetes every field has a belt holder: the field manager that owns it.
Only the holder may write the field. Anyone else must **challenge**; the
apiserver decides by policy (`BeltPolicy`: manager priority, explicit hand-over,
holder liveness). Every title change is recorded on the Tape.

Results: no controller wars, no 409 retry storms, a readable ownership history
for every field.

Rollout: start as a detector and a ValidatingAdmissionPolicy, then move the
enforcement into the apiserver field manager.

### Sorry for what?: causality in metadata

`ownerReferences` say who owns an object, not what caused a change.

Every write carries a cause chain (W3C `traceparent` plus the causing object
and revision). Client libraries used by Gachinetes controllers propagate it
automatically; for upstream controllers the apiserver infers it from the
informer event that preceded the write. Any failure explains itself by walking
the chain: Deployment edit → ReplicaSet → Pod → preemption by tag team X.

### Safeword: trust epochs

Service-account tokens are JWTs; revoking an issued token instantly is close
to impossible.

The apiserver keeps a **trust epoch**. Every elevated credential is bound to
the epoch it was issued in. Saying the safeword
(`gachictl safeword`) bumps the epoch and every elevated session dies at once.
Elevation is a `Contract` object: scope, duration, safeword, approvals.

### Spotter: two-person rule

Operations marked dangerous (delete of a namespace, CRD, PV; cluster-admin
contracts; changes to a production BeltPolicy) need a spotter: a second
identity approves within a time window. Enforced in admission, recorded on
the Tape.

## Scheduling and execution

### Weight classes

QoS classes become weight classes: flyweight (BestEffort), middleweight
(Burstable), heavyweight (Guaranteed), and a new **super-heavyweight** — an
exclusive node or whole GPUs. Preemption happens only within a class or from a
heavier class down, so the rules of a fight are always visible.

### Tag team: gang scheduling in the core

A `Team` is a first-class group of pods. The whole team enters the ring or
nobody does: no distributed training job sits on 60 of 64 GPUs. A team can be
spread across nodes by topology (NVLink domain, rack, zone).

### Three hundred bucks: money as the unit

- `ResourceClass` prices (per GPU model, CPU, memory, storage class).
- `Budget` per namespace in currency instead of cores.
- When resources are short, pods bid from their budget. A preempted pod gets
  credits and is first in line for rescheduling.
- `apply` prints the cost delta before applying. Manual actions (`exec`,
  `port-forward`, hand edits) are counted as toil with a price.

### Boy next door: traffic-aware placement

Flow data from Cilium Hubble shows who talks to whom. The scheduler and a
descheduler place the chattiest pairs on the same node or zone, cutting
latency and cross-zone traffic bills.

### Get your ass back here: live migration

Eviction means migration by default. A pod moves to another node with its
memory, IP and open connections (CRIU checkpoint/restore through the kubelet
checkpoint API, extended with restore). Drains become zero-downtime; Dungeon
quarantine becomes a live move to an isolated node with memory preserved.

### Rounds: checkpointed long jobs

A long job runs in rounds. Between rounds Gachinetes takes a checkpoint
(application hook, or CRIU when supported). A preempted or migrated job resumes
from the last round instead of from zero.

### Protein: pre-warmed capacity

AI images are many gigabytes and GPU nodes take minutes to join. Protein keeps
warm pools of nodes and pre-pulls images by forecast, and uses lazy image
loading so a container starts before the image is fully downloaded.

### Boss of this gym per shard

Controllers are sharded. Each shard has its own boss; replicas compete for
shards and take them from weaker holders. Leadership is decided by fitness
(etcd latency, queue depth, error rate), not only by who holds a lease, so a
sick leader is replaced before it fails.

## Add-ons

- **Wrestling detector** — finds manager pairs that keep rewriting one field
  (until belts are enforced, and for clusters without the core changes).
- **GymAutoscaler** — in-place resize as a training plan: progressive
  overload in small steps with rest periods, personal records, muscle memory
  for seasonal peaks, deload weeks that test whether resources are still
  needed.
- **Dungeon** — quarantine instead of kill: deny-all network except
  forensics, filesystem and log snapshot, debug container, release by the
  Dungeon Master only.
- **Leather** — hardened defaults: user namespaces, seccomp and AppArmor on,
  read-only root filesystems, default-deny network.
- **Locker room** — per-workload secret lockers: secrets encrypted with keys
  bound to the node TPM and the workload identity, never plaintext in storage.
- **Leather club is two blocks down** — a 404 from the ingress suggests the
  nearest route: typo, same service in another namespace, old name.
- **♂right version♂** — digests only; images rebuilt automatically on a
  patched base when a CVE lands, with SBOM and signature.
- **Sparring** — scheduled chaos (pod kills, network delay, node loss) with
  a resilience rank per service and a team leaderboard.
- **Performance artist** — traffic mirroring to a new version before a
  rollout.
- **Choreography** — progressive delivery in the core: canary, analysis and
  waves across clusters as one rollout object.
- **Aniki** — a mentor agent (built on the kuberoot agent operator): reviews
  manifests at admission, explains failures from the cause chain, proposes
  fixes. Never approves its own actions.
- **Away match** — bursting a team to another cluster over the mesh (for
  example GPUs from a public cloud) when the home gym is full.

## Interface

A VHS tape from the nineties.

- Scanlines, tracking noise, a timecode in the corner, VCR on-screen display
  (`PLAY ▶ 00:13:37`), a pixel font.
- Two themes: **Gym** (locker-room tiles, chrome) and **Dungeon** (leather,
  rivets, red light).
- **Gym floor** — nodes are racks and benches, pods are athletes; muscle size
  is resource use, so an over-provisioned pod looks bloated.
- **Ring** — replays of controller fights and belt changes on a timeline.
- **Tape controls are real** — `◀◀` and `▶` rewind the cluster through the
  Tape.
- **Danmaku** — events fly across the screen as on Niconico, filterable.
- **Chat** — alerts as a Twitch chat with emotes: gachiBASS for degradation,
  gachiHYPER for recovery, gachiGASM for a successful release.
- **Remix** — optional sonification: load drives the bass line, errors become
  samples in the beat.
- **Aniki** — the help and assistant panel.

## CLI

`gachictl`: `take-it boy -f` (apply), `wrestle` (resolve a belt challenge),
`rewind`, `undelete`, `fantasize`, `safeword`, `dungeon`, `spot`, `remix`.

## Plan

1. Distribution base: kuberoot profile `gachinetes`, `gachictl` skeleton,
   kubepkg packages.
2. Tape + graveyard + rewind; first VHS UI with real tape controls.
3. Wrestling detector, belts as policy, cause chains.
4. Fantasies.
5. Scheduler: weight classes, tag team, budgets and bids, boy next door.
6. Kubelet: live migration, rounds, protein.
7. Security: trust epochs and contracts, spotter, dungeon, leather, lockers.
8. GymAutoscaler, sparring, Aniki, away matches.

Every phase ends with conformance (the distribution must stay a certified
Kubernetes) and a live test on a stand. Heavy builds and stands run in CI or
in the Hetzner workshop cluster, never on a laptop.
