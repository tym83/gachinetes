# Gachinetes ♂

A Kubernetes distribution rebuilt around the gachimuchi meme. The meme is
the vocabulary; under every term is a gap Kubernetes leaves open. It is
built on [kuberoot](https://github.com/kuberoot-dev/kuberoot), and most of
it also runs on any cluster.

[Design](docs/design.md) · [Дизайн](docs/design.ru.md) · [Русский](README.ru.md)

## What it does

**The tape.** The cluster store keeps its history past compaction (the
kuberoot store's tape), and `gachi-tape` serves it decoded as the aggregated
API `tape.gachinetes.dev`, Secrets opened with the API server's own keys.

```
gachictl get configmaps -n shop --at 15m     # the namespace as it was
gachictl rewind -n shop --to 13:00Z          # ◀◀ what changed since
gachictl history deployment/web -n shop      # every version
gachictl graveyard -n shop                   # what was deleted
gachictl undelete ns/shop                    # rest in peace? not today
gachictl vhs                                 # all of it as a VHS tape
```

**The referee.** `gachi-referee` watches every write. Field managers that
keep taking a field from each other make a `Bout` and an event on the
object. A `BeltPolicy` names fields and who holds them; anyone else's write
is refused or warned of. `gachictl why` tells what is wrong with an object,
which owner made it after which change, and the bouts around it.

**Fantasies.** `gachi-fantasy` plays a change out on a copy of the cluster
at any point of the tape, with real controllers and scheduler and kwok for
the nodes, and reports what would be made, changed and removed, where pods
would land and which would find no place. `--merge` then applies it unless
the cluster moved on.

**The scheduler.** `gachi-scheduler` is kube-scheduler with three rules: a
super-heavyweight holds its node alone; a tag team goes in all at once or
not at all; a pod lands next to the peers it talks to.

**Three hundred bucks.** A `PriceList` and namespace `Budget`s in money;
a pod that would take its namespace over budget is refused, and
`gachictl bill` shows the month.

**Protein.** A `ProteinShake` keeps placeholders holding room for heavy
pods, pushed out by real ones, and images held on the nodes.

**Get your ass back here.** `gachi-mover` moves running pods between nodes
by checkpoint and restore (CRIU): a `Migration` checkpoints the pod on its
node and restores it, memory and all, on another. Pods with a round
annotation are checkpointed on schedule and come back from their last round
when they fail or lose their node. Restoring needs CRI-O, which the
distribution runs.

**Contracts and the safeword.** A `Contract` binds a cluster role to a
person for a while; only someone who may grant the role, and not the one who
asks, approves it as their spotter. `gachictl safeword` ends every contract
at once. Namespaces, CRDs and volumes can be spotted: deleting them takes a
second person's approval.

**Dungeon, leather, lockers.** `gachictl dungeon` quarantines a pod instead
of killing it: out of its controller and its Services, kept for a look.
Leather, a label on a namespace, keeps pods off the host. A `Locker` keeps a
Secret readable by its own service accounts only.

**The coach.** A `TrainingPlan` resizes a Deployment's pods in place like a
lifter's program: progressive overload with rest between sets, personal
records, the week's peaks remembered ahead of time, and deloads when the
weight is no longer needed.

**Sparring, Aniki, away matches.** A sparring partner kills a pod of a
Deployment on schedule and ranks services by how fast they stand up again.
Aniki, in namespaces that ask for him, reviews workloads on admission and
warns of what will hurt them. An away match plays the pods that find no
room at home in another cluster, and brings them back when there is room.

## Running it

On kuberoot, as a distribution:

```
hack/kuberoot.sh build amd64
hack/kuberoot.sh media amd64 kuberoot.datastore=kuberoot-store kuberoot.store-tape
```

On another cluster: `deploy/` has the gym's resources and the referee; the
tape needs a store that keeps its history.

`hack/stand/` brings up a control plane in one pod and runs every scenario
against it; `hack/vm/` runs live migration, rounds and the coach on a
cluster of kuberoot VMs.

## Status

A working study, not a product. See [the design](docs/design.md) for what
is planned.

## License

Apache 2.0.
