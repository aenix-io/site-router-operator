# Acceptance suite of a gateway

A live topology of two VMs, the gateway A under test and the remote site B, and the flows between them. It is run by hand against a cluster, the result is written down, and nothing runs it in CI.

## What the cluster needs

- KubeVirt and CDI, a storage class, and a load balancer that serves `LoadBalancer` Services.
- kube-ovn with the connector in `connectors/kube-ovn` and the `kubeovn-webhook` that copies the routes of a namespace onto a pod when it is created.
- Cilium, for the policies of the gateway (`gateway.isolation.mode: cilium`).
- The operator, installed with its chart as the release `site-router-operator` in the namespace `site-router-system`, with a configuration that lists the image of the gateway and the networks of the cluster. The pods of the operator carry the labels `app.kubernetes.io/name: site-router-operator` and `app.kubernetes.io/instance: site-router-operator`, which is the identity the chart puts in `gateway.isolation.controller`. The probe pod takes the same identity, so that it can call the management API of the gateway. If the operator is installed otherwise, change `probe-driver.yaml`, the `podLogs` selectors in `chainsaw-test.yaml` and `PROBE_NS`.
- The tools `chainsaw` (0.2.15 was used), `kubectl`, `jq`, `base64`.

## Running it

```sh
APPLIANCE_DISK_REF=ghcr.io/aenix-io/barerouter/kubevirt-disk@sha256:<digest> \
  chainsaw test test/e2e/site-router --config test/e2e/site-router/.chainsaw.yaml
```

`APPLIANCE_DISK_REF` is the image of the gateway, `<repository>@sha256:<digest>` with no tag, because CDI cannot read a reference that carries both. B boots from it, so that both ends run the same appliance. Name the same digest in the configuration of the operator.

Two more settings are read from the environment of the run. `API_KEY_FIELD` is the key of the Secret `site-router-a-api-key` that holds the management key of the gateway, `api-key` by default, which is the key the barerouter engine uses, and depends on the engine. `PROBE_NS` is the namespace of the probe pod, `site-router-system` by default.

## What it asserts

The status of the SiteRouter, the objects of the gateway, the tunnel and its state as the operator reads it, the routes and the grant that the connector makes, the pods that lag, the flows in both directions with the source preserved, the clamp of the MSS, the refusal of a request without the key from a pod of the tenant, the negative-security matrix, and the withdrawal on delete.

The namespace of a run is a plain one, so the suite gives it the Cilium policies of a tenant namespace first (`tenant-baseline.yaml`). The policies of the operator select the gateway and, on the Cilium of the cluster this was written against, a pod that is selected by an egress deny and has no egress allowed sends nothing; the tunnel is dialled from a pod of the cluster, which only the baseline admits. Without the baseline the gateway has no way out and cannot be reached from B. With it the port of the management API is open to every pod of the namespace, as it is in a tenant namespace, because the rules of the baseline let the cluster in and the policy of the gateway only adds to them. The key is the control that is left, and the suite asserts that a request without it, or with another, is refused.

The negative-security matrix reads the firewall counters of the gateway over the management API of the appliance, so `_probe-lib.sh` is the part of the suite that knows the engine. Another engine needs its own evidence there.
