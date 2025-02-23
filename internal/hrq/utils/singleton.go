package utils

import (
	"fmt"

	v1 "k8s.io/api/core/v1"

	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
)

func IsSingletonRQ(rq *v1.ResourceQuota) bool {
	return rq.Name == api.ResourceQuotaSingletonName
}

func IsScopedRQ(rq *v1.ResourceQuota) bool {
	return !IsSingletonRQ(rq)
}

func ScoepdRQName(hrqName string) string {
	return fmt.Sprintf("%s-%s", api.ResourceQuotaSingletonName, hrqName)
}
