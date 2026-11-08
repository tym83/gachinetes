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

### The tape in S3: history that outlives the cluster

The tape survives compaction, not the loss of every control plane.
`gachi-tape` ships it to S3-compatible storage as it is written: a base
(every key at a revision) and then the journal after it, in segments, read
from the store's `/tape/v1/journal`. One `gachi-tape` ships at a time, the
holder of the lease `gachi-tape-replicator`; it takes its settings from
flags (`--replicate s3://BUCKET/PREFIX`) or from the Secret
`gachinetes/gachi-tape-replica` in the keys kuberoot's state backups use
(`bucket`, `s3Endpoint`, `s3Region`, `s3AccessKey`, `s3SecretKey`,
`encryptionRecipient`, `retention`). Objects are gzipped JSON lines and
encrypted with age; unencrypted shipping has to be asked for.

A new base comes every day (`baseEvery`), so that a restore need not replay
everything and retention can drop what is older: the bucket keeps the
newest base at least `retention` old and everything after it. When the tape
was cut past what the bucket holds (the shipper was away longer than the
store keeps history), the bucket starts a new base instead of keeping a
journal with a hole.

After the cluster is gone:

```
gachictl tape restore --from s3://tapes/gym --at 2026-11-08T13:00Z \
  --age-identity key.txt --data-dir /var/lib/kuberoot-store
```

takes the oldest base whose journal reaches the moment without a gap,
writes the tape file and has `kuberoot-store tape restore` make a store
member of it: the keys at their own revisions, and rewind, history,
graveyard and undelete from that base on. The API server's encryption keys
are not in the tape; they are needed to read the Secrets back.

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

**Previews of pull requests.** A fantasy kept up is a preview environment
that costs a control plane, not a cluster:

```
gachictl fantasy preview --pr https://github.com/acme/shop/pull/42 --manifests deploy/ --ttl 72h
gachictl fantasy merge --pr acme/shop#42      # after the request is merged
gachictl fantasy discard --pr acme/shop#42    # after it is closed
```

`preview` runs a Job in `gachi-fantasies` (image `gachi-fantasy-preview`:
gachi-fantasy, kuberoot-store, the control plane's binaries, kwok) that
branches the tape, applies the request's manifests (from a ConfigMap),
waits for quiet and publishes the report and the branch's kubeconfig to a
ConfigMap and a Secret beside it; a Service puts the branch's API server at
`pr-<repo>-<n>.gachi-fantasies.svc:6443`, and a second kubeconfig reaches it
through `kubectl port-forward`. A new push starts the preview over.
Everything belongs to the Job, so the time to live is the Job's: the
fantasy stops after `--ttl`, the Job's deadline is the backstop, and a
finished Job goes with all it owns. `merge` applies the manifests to the
cluster with the same conflict check as `--merge`, then discards the
preview. [examples/github/fantasy-preview.yaml](../examples/github/fantasy-preview.yaml)
wires it to pull request events and posts the report as a comment.

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

## Between people

The same fights happen between people working on one cluster.

- **Console ring** — during an incident only one person at a time holds
  `exec`, `edit` and `rollout` in a namespace. Others see who holds the ring
  and queue; a challenge takes it when the holder agrees or stays silent past
  a timeout. Two people fixing production at once no longer overwrite each
  other.
- **People's belts** — field ownership extends to people: the image of a
  production Deployment has a belt holder, like CODEOWNERS for live objects.
  Someone else's change is a challenge the holder accepts or declines.
- **Team bouts for capacity** — when the cluster is short, teams bid from
  their budgets. Idle quota can be lent for a sparring session; the owner
  takes it back by preemption, with a warning.
- **Spotter as pairing** — the two-person rule calls a specific person, not a
  chat channel.
- **Clashing fantasies** — two people preparing changes in their own branches
  see the conflict before anything is applied: who breaks what for whom.
- **Gym titles** — Boss of this gym for the most incidents closed in a month,
  Performance artist for a Friday-evening deploy; counted from cause chains.
- **Toil wallet** — every engineer has a budget for manual actions (`exec`,
  `port-forward`, hand edits); who reaches into production by hand most is
  visible in numbers.
- **Sorry for what? in reviews** — the postmortem assembles itself from the
  cause chain: who changed what and what followed. No blame, and no "it
  happened by itself" either.
- **Dungeon for sessions** — a session that behaves suspiciously (mass reads
  of Secrets, a login from a new place) is not killed but moved to the
  Dungeon: read-only until the Dungeon Master looks at it.

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
