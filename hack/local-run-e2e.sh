#!/bin/bash

set -euf -o pipefail

PROJECT_ROOT=$(git rev-parse --show-toplevel)
TOOLS_DIR="${PROJECT_ROOT}/hack/bin"
export PATH="${TOOLS_DIR}:${PATH}"

KIND_VERSION=v0.33.0
KIND_NODE_IMAGE="kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed"
KREW_VERSION=v0.4.5
KUBECTL_VERSION=v1.36.4

export KUBEBUILDER_CONTROLPLANE_STOP_TIMEOUT=120s

PFNET_MODE=false
while [[ $# -gt 0 ]]; do
  key="$1"
  case $key in
    --pfnet)
      PFNET_MODE=true
      shift
      ;;
  esac
done

mkdir -p "${TOOLS_DIR}"

export KREW_ROOT=${TOOLS_DIR}/krew
export PATH="${KREW_ROOT}/bin:${PATH}"

(
	cd "$(mktemp -d)"
	OS="$(uname | tr '[:upper:]' '[:lower:]')"
	ARCH="$(uname -m | sed -e 's/x86_64/amd64/' -e 's/\(arm\)\(64\)\?.*/\1\2/' -e 's/aarch64$/arm64/')"

	echo "Installing kubectl..."
	if [ ! -f "${TOOLS_DIR}/kubectl" ]; then
		curl -Lo kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/${OS}/${ARCH}/kubectl"
		chmod +x kubectl
		mv kubectl "${TOOLS_DIR}/"
	else
		echo "kubectl already exists in ${TOOLS_DIR}, skipping installation."
	fi

	echo "Installing and starting Kind..."
	if [ ! -f "${TOOLS_DIR}/kind" ]; then
		curl -Lo kind "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-${OS}-${ARCH}"
		chmod +x kind
		mv kind "${TOOLS_DIR}/"
	else
		echo "kind already exists in ${TOOLS_DIR}, skipping installation."
	fi

	echo "Installing krew..."
	if [ ! -d "${TOOLS_DIR}/krew" ]; then
		KREW="krew-${OS}_${ARCH}"
		curl -fsSLO "https://github.com/kubernetes-sigs/krew/releases/download/${KREW_VERSION}/${KREW}.tar.gz"
		tar zxvf "${KREW}.tar.gz"
		"./${KREW}" install krew
	else 
		echo "krew already exists in ${TOOLS_DIR}, skipping installation."
		# Install ginkgo to the TOOLS_DIR directly
		if [ ! -f "${TOOLS_DIR}/ginkgo" ]; then
			GOBIN=${TOOLS_DIR} go install github.com/onsi/ginkgo/v2/ginkgo@v2.1.4
		else
			echo "ginkgo already exists in ${TOOLS_DIR}, skipping installation."
		fi
	fi
)

echo "Installing krew plugins..."
if ! kubectl krew list | grep -q hns; then
	HNC_IMG_TAG=v0.0.0-dev make -C "${PROJECT_ROOT}" krew-install
else
	echo "krew plugin hns already exists, skipping installation."
fi

echo "Starting Kind cluster..."
KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-hnc-e2e}
CONFIG=kind KIND=${KIND_CLUSTER_NAME} KIND_NODE_IMAGE=${KIND_NODE_IMAGE} make kind-reboot deploy-hrq

kind get kubeconfig --name ${KIND_CLUSTER_NAME} > /tmp/kind-hnc-config
export KUBECONFIG=/tmp/kind-hnc-config
export HNC_REPAIR=${HNC_REPAIR:-${PROJECT_ROOT}/manifests/hrq.yaml}
KUBELET_VERSION=$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.kubeletVersion}')
if [[ "${KUBELET_VERSION}" != v1.36.* ]]; then
	echo "Expected Kubernetes 1.36, got ${KUBELET_VERSION}" >&2
	exit 1
fi
echo "Running against Kubernetes ${KUBELET_VERSION}"
if [ "${PFNET_MODE}" = true ]; then
	echo "Running e2e tests only for pfnet..."
	kubectl -n hnc-system patch deployment hnc-controller-manager -p '{"spec":{"template":{"spec":{"containers":[{"name":"manager","resources":{"limits":{"cpu":null}}}]}}}}'
	kubectl -n hnc-system wait --for=condition=available deployment/hnc-controller-manager --timeout=5m
	ginkgo run -p --label-filter pfnet ./test/e2e/...
else 
	kubectl -n hnc-system wait --for=condition=available deployment/hnc-controller-manager --timeout=5m
	go clean -testcache
	if [ -z "${HNC_FOCUS+x}" ]; then
		echo "Running all e2e tests..."
		go test -v -timeout 0 ./test/e2e/...
	else
		echo "Running e2e tests with focus: ${HNC_FOCUS}"
		go test -v -timeout 0 ./test/e2e/... -args --ginkgo.focus "${HNC_FOCUS}"
	fi
fi
