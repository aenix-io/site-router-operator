# site-router-operator

A Kubernetes operator that brings up a routed site-to-site IPsec gateway. A namespaced `SiteRouter` resource (`siterouter.aenix.io/v1alpha1`) describes the remote site. The operator runs the gateway as a KubeVirt VM booted from an appliance image pinned by digest, pushes the configuration to it, confirms the source filter, and asks the cluster network to route the remote CIDRs to the gateway.

The operator is not tied to a platform, to a particular appliance or to a particular CNI. Two seams keep it that way: the engine seam (Go interfaces in the `api` module, implemented outside the operator) and the network seam (the `Forwarder` API, served by a connector for each network).

Status: early development, nothing is released.

## Development

The repository holds two Go modules: the operator (`.`) and its public API (`api/`). Requirements: Go 1.27, Docker, GNU make. Tools such as `controller-gen` and `setup-envtest` are installed into `bin/` by the Makefile.

```sh
make help            # all targets
make test            # unit and envtest tests with -race, both modules
make lint            # golangci-lint, both modules
make check-generated # fails if generated files or go.mod/go.sum are stale
make docker-build    # build the operator image
```

Contributor and agent rules are in [AGENTS.md](AGENTS.md).

## License

Apache License 2.0, see [LICENSE](LICENSE).
