# Installing the operator

The operator is installed with the Helm chart in `charts/site-router-operator`. A release publishes the chart to `oci://ghcr.io/aenix-io/charts/site-router-operator` and the image to `ghcr.io/aenix-io/site-router-operator`, for linux/amd64 and linux/arm64, both under the version of the tag without its `v`. The chart of a release runs the image by the digest that was built and signed.

## What the cluster needs

- Kubernetes 1.31 or newer: the CRDs validate networks with the CEL library that first shipped in 1.31, and the chart refuses an older cluster.
- KubeVirt and CDI, which run the gateways and import their disks. The operator watches VirtualMachines and their instances, so their CRDs must be installed before it starts.
- Cilium, when the gateways are isolated with `config.gateway.isolation.mode: cilium`, the default.
- A connector for the network of the cluster, which serves the `Forwarder` objects the operator writes. Without one, routes are asked for and never programmed.
- An engine for the appliance the gateways run, built into the image (`cmd/engines.go`). An operator whose configuration names an engine the image does not carry stops at start and says that the engine is not registered.

## Install

```sh
helm install site-router-operator oci://ghcr.io/aenix-io/charts/site-router-operator \
  --version <version> --namespace site-router-system --create-namespace \
  --values my-values.yaml
```

The chart does not render until the keys that have no default are set (see below); the error names each one that is missing. The value sets in `charts/site-router-operator/ci/` are examples of complete installations; their networks and images are made up.

To install from the tree with an image built locally, build it, put it where the nodes pull from, and point the chart at it:

```sh
make docker-build IMG=registry.example/site-router-operator:dev
docker push registry.example/site-router-operator:dev
helm install site-router-operator charts/site-router-operator \
  --namespace site-router-system --create-namespace \
  --set image.repository=registry.example/site-router-operator \
  --set image.tag=dev --set image.pullPolicy=Always \
  --values my-values.yaml
```

`make docker-buildx IMG=...` builds and pushes both platforms. The Dockerfile cross-compiles, so no platform is emulated.

## Values of the chart

Every value is documented in `values.yaml`, and `values.schema.json` refuses a value the chart does not have. Those that matter most:

- `image.repository`, `image.tag` (defaults to the version of the chart), `image.digest` (pulls by digest when set; set it back to `""` when you set another tag), `image.pullPolicy`, `imagePullSecrets`.
- `replicaCount` and `leaderElection.enabled`. Only the leader reconciles. More than one replica without leader election is refused.
- `resources`, `nodeSelector`, `tolerations`, `affinity`, `priorityClassName` of the operator pod. The pod runs as user 65532 with a read-only root filesystem, no capabilities and the RuntimeDefault seccomp profile.
- `metrics.enabled` (off by default) serves the metrics on `metrics.port` (8443). With `metrics.secure` (the default) they are served over HTTPS to callers whose token may get `/metrics`; the chart makes a ClusterRole named after the release and ending in `-metrics-reader` for the scraper, and binds it to nobody. `metrics.service.enabled` and `metrics.serviceMonitor.enabled` add a Service and a ServiceMonitor.
- `podDisruptionBudget.enabled` with `maxUnavailable` or `minAvailable`.
- `crds.install`, below.
- `extraArgs` passes more flags to the manager, such as `--zap-devel=false` for JSON logs.

## The configuration of the operator

`config` is the configuration of the operator, as `internal/config/config.go` reads it. The chart writes it to a ConfigMap as it is and mounts it where `--config` points; the operator reads it once, when it starts, and a change rolls the pods. The operator refuses to start on a key it does not know or a value it cannot use, and says which. `values.yaml` lists every key with what it does and the default of the operator.

These have no default and must be set:

| key | what it is |
|---|---|
| `config.managementCIDR` | where the management API of a gateway accepts calls from: the pod network of the platform, where the operator runs; an IPv4 network, never `0.0.0.0/0` |
| `config.clusterNetworks.podCIDR` | the pod network |
| `config.clusterNetworks.serviceCIDR` | the service network |
| `config.engine.name` | the engine built into the image |
| `config.engine.versions` | the versions of the disk image gateways can run, `{name, image}`, each image pinned by digest |
| `config.engine.latest` | the version new gateways are made with, one of the names in `versions` |

`config.clusterNetworks.joinCIDR` and `config.clusterNetworks.extraCIDRs` add the join network of the cluster network, where there is one, and any other network the platform keeps off the tenants. The addresses of the nodes and load balancers are discovered.

The chart sets two keys that the operator has no default for: `config.upgrade.policy: auto`, which moves every gateway to `engine.latest`, and `config.gateway.isolation.mode: cilium`. Under `cilium` the chart fills in `config.gateway.isolation.controller` with the namespace of the release and the selector labels of the operator pods, which are the only pods the management API of a gateway then admits; give it yourself to name others. `external` leaves the isolation to the platform and `none` needs `acknowledgeNone: true`.

Where gateways run and how their disks are stored is under `config.gateway`: `vm` (pod labels and annotations, `nodeSelector`, `tolerations`, `affinity`, `priorityClassName`, `runtimeClassName`), `dataVolume` (`storageClass`, used when a SiteRouter names none, `blockSize`, and how the importer pulls the image) and `service` (`loadBalancerClass`, which cannot be changed on a Service that exists, and `annotations`). `config.upgrade` and `config.timing` tune moves to another image and how often the operator looks.

## Isolation of the gateway

Under `cilium` the operator makes two CiliumNetworkPolicies for each gateway: one denies it the link-local range and `config.gateway.isolation.egressDenyCIDRs`, and the other admits to it the tunnel from the world and the management API for the operator. They add to the policies the namespace has and replace none of them. Cilium leaves a pod that a policy selects only what some policy allows, in the direction the policy is about (on Cilium 1.19 with kube-ovn, a policy of deny rules alone is enough). The two policies allow the tunnel and the management API and nothing else, so in a namespace with no other policy a gateway cannot reach its peer and takes no traffic from the pods that send through it.

The namespace of a gateway therefore has to admit those pods to the gateway and let the gateway out to the world. The baseline policies of a Cozystack tenant namespace do both. Where there are no such policies, set `external` or `none`, which make no policy of their own.

## Who may write a SiteRouter

The operator takes the `spec` of a SiteRouter as input it does not trust, and two things in it reach further than the SiteRouter does. Let the application that wraps the operator write SiteRouters, not the tenants, or have it settle these two.

- `spec.peer.auth.secretRef` names a Secret of the namespace of the SiteRouter, and the operator reads its `psk` key with its own permissions, whatever the author of the SiteRouter may read. Any Secret that holds that key is taken, except one a SiteRouter controls.
- The annotation `siterouter.aenix.io/upgrade-to` names the image of the gateway from the versions in `config.engine.versions`, under either `config.upgrade.policy`, and the operator moves the gateway to it, to an older version as well. Whoever may edit the annotations of a SiteRouter can put its gateway back on a version with a known flaw.

## CRDs

The chart installs the CRDs of `SiteRouter` and `Forwarder`. They are the ones controller-gen makes in `config/crd/bases`, copied into the chart by `make chart-manifests`, and `make check-generated` fails when the copy is behind. They are templates rather than in `crds/`, which Helm installs once and never upgrades, so an upgrade of the chart brings the schema of the operator it runs. They carry `helm.sh/resource-policy: keep`: uninstalling the release or setting `crds.install: false` leaves them in the cluster, since deleting a CRD deletes every object of it, and with every SiteRouter every gateway. Delete them by hand to remove them.

A platform that delivers the CRDs itself sets `crds.install: false`. Helm does not take over a CRD that exists and is not its own, so on a cluster where they were installed some other way the install fails until it is set. A platform that serves the Forwarder API itself, with the connectors of its network, sets `crds.forwarder.install: false`: the chart then installs the CRD of `SiteRouter` alone, and the operator uses the CRD of `Forwarder` that is there, which has to be the schema of the API module the operator was built with.

## What the operator is granted

The ClusterRole of the operator is the one controller-gen makes from the markers in `internal/controller/setup.go`, copied like the CRDs, so it grants what the code says it needs and nothing else. Leader election adds a Role in the namespace of the release: `get`, `create` and `update` on Leases, and `create` and `patch` on events. Secure metrics add `create` on `tokenreviews` and `subjectaccessreviews`. Tests run the operator as the service account of the chart against an apiserver that enforces RBAC.

## Verifying the image

The image of a release is signed with cosign, keyless, by the release workflow:

```sh
cosign verify ghcr.io/aenix-io/site-router-operator@<digest> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/aenix-io/site-router-operator/\.github/workflows/release\.yml@refs/tags/v'
```
