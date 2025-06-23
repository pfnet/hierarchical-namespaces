package utils

import (
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
)

func IsSingletonRQ(rq *v1.ResourceQuota) bool {
	return rq.Name == api.ResourceQuotaSingletonName
}

func IsScopedRQ(rq *v1.ResourceQuota) bool {
	return !IsSingletonRQ(rq)
}

func ScopedRQName(hrqNamespace string, hrqName string) (string, error) {
	if strings.Contains(hrqNamespace, "--") {
		return "", fmt.Errorf("hrq namespace cannot contain '--': %s", hrqNamespace)
	}
	return api.ResourceQuotaSingletonName + "-" + hrqNamespace + "--" + hrqName, nil
}

func ScopedHRQNameFromRQName(rqName string) (types.NamespacedName, error) {
	if rqName == api.ResourceQuotaSingletonName {
		return types.NamespacedName{}, fmt.Errorf("invalid RQ name for ScopedHRQ name: %s", rqName)
	}

	parts := strings.SplitN(strings.TrimPrefix(rqName, api.ResourceQuotaSingletonName+"-"), "--", 2)
	if len(parts) != 2 {
		return types.NamespacedName{}, fmt.Errorf("invalid RQ name for ScopedHRQ: %s", rqName)
	}

	return types.NamespacedName{Namespace: parts[0], Name: parts[1]}, nil
}
