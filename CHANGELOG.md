# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-11-08

First release. Gachinetes is a research distribution: a working study, not a
product. Phases 1–8 of the [design plan](https://github.com/tym83/gachinetes/blob/main/docs/design.md#plan) are implemented.

### Added

- **Tape.** The kuberoot store (etcd-API compatible) keeps cluster history
  past compaction in a journal. `gachi-tape` serves it decoded as the
  aggregated API `tape.gachinetes.dev` (front-proxy authentication,
  SubjectAccessReview per request), with Secrets decrypted using the API
  server's own encryption keys.
- **gachictl**: `get --at`, `rewind --to`, `history`, `graveyard`,
  `undelete`, `tape`, `bouts`, `belts`, `why`, `bill`, `safeword`,
  `dungeon`, and `vhs`, a web UI that plays the tape like a VHS deck
  (rewind, live view, graveyard, undelete).
- **gachi-referee**: `Bout` resources and events for field managers that keep
  taking a field from each other; `BeltPolicy` field ownership enforced by a
  validating webhook (refuse or warn).
- **gachi-fantasy**: copy-on-write branches of the cluster at any point of
  the tape, run with real controllers and scheduler and kwok nodes; a report
  of created, changed and removed objects and pod placements; `--merge` with
  conflict checks.
- **gachi-scheduler**: kube-scheduler with weight classes (a super-heavyweight
  holds its node alone), tag teams (all-or-nothing gang scheduling) and
  boy next door (placement next to chatty peers).
- **Three hundred bucks**: `PriceList` and namespace `Budget` in money, with
  admission refusing pods over budget.
- **Protein**: `ProteinShake` placeholders holding room for heavy pods and
  images pre-pulled on nodes.
- **gachi-mover**: live migration of running pods between nodes by CRIU
  checkpoint and restore on CRI-O (`Migration`), and rounds: scheduled
  checkpoints that bring a failed pod back from its last round.
- **Security**: `Contract` (time-bound cluster role with a spotter's
  approval), safeword (revoke every contract at once), spotted deletes of
  namespaces, CRDs and volumes, the dungeon (pod quarantine), leather
  (keeps pods off the host), `Locker` (Secret readable by its own service
  accounts only).
- **Coach**: `TrainingPlan` resizing a Deployment's pods in place.
- **Sparring, Aniki, away matches**: scheduled pod kills with service
  rankings, admission-time workload review, bursting unschedulable pods to
  another cluster.
- **Distribution**: `hack/kuberoot.sh` builds kuberoot as Gachinetes (edge
  profile, gachi-scheduler instead of kube-scheduler, the tape, referee and
  mover as node services, CRI-O with CRIU, a kernel with checkpoint/restore).
- **Test stands**: `hack/stand/` (control plane in one pod, 131/131
  scenarios) and `hack/vm/` (live migration, rounds and the coach on
  kuberoot VMs).
- Release workflow: linux amd64/arm64 binaries and multi-arch images
  `ghcr.io/tym83/gachinetes/<component>`.

### Known issues

- Conformance (hydrophone, 462 tests) on a three-VM kuberoot cluster:
  461/462 in the full run. The one failure, the tcp:8080 liveness probe
  test, was a kubelet probe timeout on a loaded VM; it passed 3/3 when run
  on its own.
- The between-people phase of the design is not implemented.
- The release images are built from this repository only; the kuberoot base
  images are not part of this release.

[Unreleased]: https://github.com/tym83/gachinetes/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/tym83/gachinetes/releases/tag/v0.1.0
