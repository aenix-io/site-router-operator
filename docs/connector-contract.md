# The contract of a connector

A connector makes a cluster network do what a `Forwarder` asks: it finds the gateway, lets it forward, and routes networks to it. The operator writes the `Forwarder` and reads the answer, and knows no network. Whoever writes a connector, for kube-ovn, for another network or for a platform that serves the API itself, keeps what is below. Part of what keeps a tenant's traffic where it belongs is done here and not in the operator, so a connector that does not keep it is not one the operator can be used with.

The suite in `connectors/conformance` runs what a test can check, and the comment of each scenario names its clause. A connector runs it in its tests (see [Running the suite](#running-the-suite)). What the suite does not reach, such as the minute in clause 1 and the grant that is kept in clause 10, is still the contract. The `Forwarder` API is in `api/v1alpha1/forwarder_types.go`.

## The request

A `Forwarder` is named like the VirtualMachineInstance of the gateway, `site-router-<name of the SiteRouter>`, and is in the namespace of the tenant. Its `spec` says which pods are the gateway (`workloadSelector`), which pods of the tenant need a route to it (`scopeSelector`), and which networks are routed to it (`cidrs`).

1. **The answer.** A connector that serves a `Forwarder` writes its status for the generation it has seen: `observedGeneration`, and the conditions `Accepted`, `Resolved`, `Granted`, `RoutesProgrammed` and `Conflict`, each for that generation. The operator reads only the types and the status of a condition, never the reason or the message, and says to the tenant what it says itself. A `Forwarder` that no connector has answered for a minute is reported as one nobody serves.

## The gateway

2. **Who the gateway is.** The gateway is the pod that the VirtualMachineInstance named like the `Forwarder` controls, by the UID of that instance, and that the instance lists among its active pods in `status.activePods`, which only KubeVirt writes. A label is written by whoever makes the pod, and so is an owner reference, so neither is enough, and a pod the instance does not list is not the gateway yet. A selected pod that is not the gateway is let forward and routed to by no one. When the active pods are not known, the connector changes nothing (clause 12).

3. **The addresses of the gateway.** `status.forwarder.addresses` holds the addresses of the gateway pods in the addressing of the cluster network, and nothing else. They are the next hop of the routes.

## The tenant

4. **The scope.** `status.scope.addresses` holds the addresses of the pods selected by `scopeSelector` and the cluster IP of each Service of the namespace. It leaves out the pods that are being deleted, whose address goes back to the pool, the pods that have finished, the pods on the host network, whose address is that of a node, the gateways, which are the pods that the `workloadSelector` of any `Forwarder` of the namespace selects, and the Services that carry the label `siterouter.aenix.io/instance`, which are the ones in front of a gateway and take its tunnel. An address that is not the tenant's to reach must not be here: the gateway lets decrypted traffic through to what is.

5. **No address of somebody else.** No condition, no event and no field of the status names an address that is not the tenant's: not the gateway of another tenant that a route of the namespace goes to, not a node.

## The grant

6. **No networks, no grant.** While `cidrs` is empty the connector grants nothing and programs no routes: `Granted` and `RoutesProgrammed` are False.

7. **Networks, grant and routes.** With networks, a gateway that is found and no conflict, the connector lets the gateway pod forward the traffic of addresses that are not its own, which on most networks means ending the anti-spoofing the network does for the pod, and routes every network to the address of the gateway for the pods of the namespace. Then `Granted` and `RoutesProgrammed` are True. The grant is for the gateway and for no other pod (clause 2).

8. **Taking it back.** The grant and the routes of a `Forwarder` are taken back when its `cidrs` become empty, when the `Forwarder` is deleted, and for a pod that stops being the gateway, whether KubeVirt stops listing it or its labels go. The pod that replaces it gets them. A connector that has a finalizer on the `Forwarder` keeps it until it has taken them back. The operator withdraws by emptying `cidrs` and waits until `Granted` and `RoutesProgrammed` are no longer True for the generation it asked, before it deletes the gateway.

9. **What is not the connector's.** A route of the namespace that no `Forwarder` owns is never changed or taken over, and is never taken back.

## Conflicts

10. **The one made first keeps it.** Of two `Forwarder`s of a namespace that ask for networks that overlap, the one made first keeps them, whatever their names say. The other has `Conflict` True, is granted and routed nothing new, and keeps a grant it already has. A route that no `Forwarder` owns and that overlaps a network asked for is a conflict too.

## The pods that are late

11. **Pending workloads.** On a network that gives a pod its routes only when the pod is made, the pods of the scope that were made before the routes do not have them. `status.pendingWorkloads` names them, and a pod made after the routes is not named. The operator tells the tenant that they need a restart.

## What goes wrong

12. **A restart.** A connector that stops and comes back while KubeVirt has not yet written the active pods of the instance leaves the gateway as it was: it keeps the grant and the routes, and writes to neither the pod nor the namespace, until the pods are known.

13. **A cluster that cannot be read.** A connector that cannot read the cluster changes nothing until it can. It does not take a grant back on the strength of a read that failed.

## Running the suite

The suite is the Go module `github.com/aenix-io/site-router-operator/connectors/conformance`, and a connector runs it from a test:

```go
func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Setup{
		Client:  k8sClient, // an administrator of the apiserver the connector serves
		Network: myNetwork{},
		Start: func(t *testing.T) func() { return startMyConnector(t).Stop },
	})
}
```

It needs an apiserver with the CRD of the `Forwarder` and the CRD of the VirtualMachineInstance of KubeVirt, and the connector running against it; an apiserver of the envtest kind does, and there need be no network. The suite makes the namespaces, the pods and the instance, writes their status as KubeVirt and the kubelet would, and reads what the connector made of them. What only the network can say is asked of a `Network`: how a pod is made there (`Admit`), whether it is let forward (`Forwards`), which routes the namespace has (`Routes`, `AddRoute`) and which a pod carries (`PodRoutes`). `Start` runs the connector and returns what stops it. `StartUnreadable`, which is optional, runs the connector with a client whose reads fail while a flag is set, for clause 13; without it that scenario is skipped.

`connectors/kube-ovn/test/e2e/conformance_test.go` is an example: kube-ovn says it all in annotations.
