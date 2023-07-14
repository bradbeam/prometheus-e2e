This repo contains code for testing a Prometheus pipeline using `e2e-framework`.
It is the supporting code for https://squiggly.dev/2023/07/end-to-end-testing-for-prometheus/.

This requires a working [kind](https://kind.sigs.k8s.io) installation.

To run:

```sh
go test -v
```

This will:
- create a kind cluster
- deploy a pod containing prometheus, fake-cadvisor, remote-write
- check to make sure we get our expected metrics
- destroy the kind cluster
