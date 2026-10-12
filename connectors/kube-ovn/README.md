# Connector of the Forwarder API to kube-ovn

The site-router operator does not touch the cluster network. For each gateway it writes a `Forwarder` (group `siterouter.aenix.io`) that says which pods forward traffic for which networks, and it reads the status back. This connector makes kube-ovn do what a `Forwarder` says: it lets the gateway pod forward, routes the networks to it for the pods of its namespace, and reports the addresses of the gateway and of the tenant.

It is a Go module of its own that depends only on the `api` module of this repository, so that it can move to a repository of its own.

## What it needs

- kube-ovn, with the `kubeovn-webhook` of Cozystack or anything that does what it does: copy the annotation `ovn.kubernetes.io/routes` of a Namespace onto a pod when the pod is made. kube-ovn applies the routes of a pod only when the pod is made.
- KubeVirt: a gateway is the pod that the VirtualMachineInstance named like the `Forwarder` controls by UID and lists among its active pods.
- The CRD of the `Forwarder`, which the chart of the operator installs.
- Kubernetes 1.30 or newer, for the `ValidatingAdmissionPolicy` of the deploy files.

## What it reads and writes

| object | reads | writes |
|---|---|---|
| `Forwarder` | spec, metadata | the finalizer `kube-ovn.siterouter.aenix.io/finalizer`; the status |
| VirtualMachineInstance | UID, `status.activePods` | nothing |
| Pod | labels, owner references, annotations, phase, addresses, `hostNetwork` | `ovn.kubernetes.io/port_security` (`"false"` on the gateway of a `Forwarder` that asks for networks, `"true"` when that ends) and `kube-ovn.siterouter.aenix.io/granted-by`, which names the `Forwarder` and decides nothing |
| Namespace | annotations | `ovn.kubernetes.io/routes`, merged by destination with what others wrote; `kube-ovn.siterouter.aenix.io/route-owners`, the gateway addresses each `Forwarder` routes to |
| Service | cluster IPs | nothing |

A grant is decided on what the connector verifies, never on an annotation it reads back from a pod. It is taken back by writing `"true"`: kube-ovn takes a pod with no annotation for one with the check off. A gateway is shut when its `Forwarder` asks for no networks, is deleted, or the pod is shown to be no longer the gateway; a restart of the connector, an apiserver that does not answer, or a status of KubeVirt that is late leaves a working gateway as it is.

Of two `Forwarder`s of a namespace that claim overlapping networks, the one made first keeps them; the other has the condition `Conflict` and gets nothing new until the conflict ends. A route of the namespace that no `Forwarder` owns is never taken over.

## Build

```sh
make build                                   # bin/kube-ovn-connector
make docker-build IMG=<registry>/<image>:<tag>  # the context is the root of the repository
make docker-buildx IMG=<registry>/<image>:<tag> PLATFORMS=linux/amd64,linux/arm64  # builds and pushes
```

## Deploy

Set the image in `deploy/kustomization.yaml` (`images`), then:

```sh
kubectl apply -k deploy
```

`deploy/` holds a namespace at the restricted level of pod security, the ServiceAccount, the ClusterRole generated from the RBAC markers, a lease for leader election, the roles of the authenticated metrics endpoint on port 8443, the Deployment, and a `ValidatingAdmissionPolicy` that lets the connector change on a pod and a Namespace only its own annotations, which RBAC cannot say. The namespace is named in each file and in the policy; change all of them together.

## Test

```sh
make test   # unit tests and tests on a real apiserver, with -race
make lint
```

The tests on a real apiserver run the connector as its ServiceAccount, with the generated ClusterRole and the admission policy of `deploy/`. There is no kube-ovn and no webhook in them: a pod is made the way the webhook would make it.
