# AGENTS.md

Guidance for AI coding assistants and agents working in this repository.

## Project

`site-router-operator` is a Kubernetes operator. A namespaced `SiteRouter` custom resource brings up a routed site-to-site IPsec gateway: a KubeVirt VM booting an appliance image pinned by digest, its seed and credentials Secrets, a LoadBalancer Service for the tunnel and, optionally, Cilium policies. The operator also validates remote CIDRs against cluster networks, pushes configuration to the appliance, confirms the source filter, and asks the network to route remote CIDRs to the gateway.

The operator is not tied to Cozystack, to a particular appliance or to a particular CNI. Keep it that way.

## Architecture rules

- Two seams, both small:
  - Engine seam: Go interfaces and neutral types in the `api` module (`api/engine`). The operator talks to the appliance only through them. Engine implementations live in other modules.
  - Network seam: the `Forwarder` API (CRD shipped by this repository's chart). The operator writes and reads it, a connector for each network does the rest. Connectors live outside the operator.
- `internal/*` must not import Cozystack, Flux or lineage packages, and must not name VyOS or barerouter. A test checks the import graph. The operator package must not import an engine implementation (only `cmd/` registers one).
- Platform settings are typed operator config, not fields of the `SiteRouter` spec. The spec holds only what the app wrapper may set. Treat the spec as untrusted input: schema and CEL for static checks, deny-set in the controller.
- Materialization is a pure function from spec, operator config, the engine's bootstrap plan and the `Forwarder` status to Kubernetes objects. It never reads the cluster. Applying is separate (create-if-absent for credentials, owner references on everything). `assertInvariants` rejects objects that break the guarantees below.
- Invariants of the gateway objects: the pod template carries the discovery label; the default interface is a bridge on the pod network; the boot volume is the planned DataVolume and the seed volume is the planned Secret; the tunnel Service keeps `externalTrafficPolicy: Local`; the pod template has no `port_security` annotation (the connector grants it); `loadBalancerClass` is not changed after creation.
- Reconcile order: validate, connect, apply, confirm, grant and routes, observe. Routes are programmed only after the engine confirms the source filter.
- Secrets never reach logs, events, conditions or status. Redact on every engine call, not only on apply.
- No validating webhook in v0.1. Status lives in `status.conditions` on the CR.

## Layout and commands

- Two Go modules: the operator (`.`) and its public API (`api/`, import path `github.com/aenix-io/site-router-operator/api`). The root `go.mod` replaces `api` with `./api`. Run Go commands for the API from `api/`. The API group is `siterouter.aenix.io`.
- `make manifests generate` regenerates CRDs, RBAC and deepcopy code. `make check-generated` fails when they, or `go.mod` and `go.sum`, are stale; run it in a tree with no uncommitted changes.
- `make test` runs unit and envtest tests with `-race` in both modules. `make test-crd` runs the CRD schema tests and the tests that apply a gateway on Kubernetes 1.31, 1.34 and the newest supported version; the CEL rules are tested on a real apiserver, because older apiservers estimate their cost differently and can refuse a CRD that a newer one accepts. `make lint` runs golangci-lint in both.
- `make check-commits` checks commit messages against the rules below. CI runs the same `hack/check-commits.sh` on every pull request.

## Conventions

- Go: controller-runtime and kubebuilder style. Tests run with `-race`. `make generate` must leave no diff.
- Git: Conventional Commits (`type(scope): description`) with `--signoff`. An agent-assisted commit adds one `Assisted-by: LLM` trailer.
- Commit bodies and code comments explain why, not what. No review-round narration, no references to plans, decision IDs or task IDs that are not in the repository.
- Markdown, PR bodies and issue comments: one continuous line per paragraph. Commit messages are wrapped at about 72 columns.

## What not to do

- Do not edit generated files (`zz_generated.*`, generated CRDs). Change the source and regenerate.
- Do not edit `go.mod` or `go.sum` by hand. Use `go get` and `go mod tidy`.
- Do not force-push to `main`.
- Do not credit an AI model or its vendor by name in a trailer, a PR description or a comment, and do not put session URLs there.
- Do not push, open PRs, comment, create issues or dispatch workflows without the maintainer's explicit approval of the exact text.
