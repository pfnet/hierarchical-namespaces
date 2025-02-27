package utils

import (
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"

	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
)

func IsSingletonRQ(rq *v1.ResourceQuota) bool {
	return rq.Name == api.ResourceQuotaSingletonName
}

func IsScopedRQ(rq *v1.ResourceQuota) bool {
	return !IsSingletonRQ(rq)
}

func ScopedRQName(hrqName string) string {
	return api.ResourceQuotaSingletonName + "-" + hrqName
}

func ScopedHRQNameFromHRQName(rqName string) (string, error) {
	hrqName := strings.TrimPrefix(rqName, api.ResourceQuotaSingletonName+"-")
	if hrqName == api.ResourceQuotaSingletonName {
		return "", fmt.Errorf("invalid ScopedHRQ name: %s", hrqName)
	}

	return hrqName, nil
}
