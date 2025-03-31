# Hacks

Gotta love this directory name, eh? Let's try to cover what's in here.

## Templates

`boilerplate.go.txt` includes the Apache header. Kubebuilder put it here and it
seems like as good a place as any.

`krew-hierarchical-namespaces.yaml` is a template for the Krew `kubectl-hns`
plugin.

## CI

Other projects seem to put their presubmits, postsubmits etc here, so we did
too. See [here](../README.md#test-infrastructure) for where these are configured
in Prow. Specifically:

* `ci-test.sh`: this is called as part of the presubmit tests. It runs all unit
  tests.
* `prow-run-e2e.sh`: this is called as part of the postsubmit and periodic
  tests. It builds the image, creates a Kind cluster, and runs all e2e tests.

See those tests for more information, including how to test them.
