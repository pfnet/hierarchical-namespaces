package utils

import (
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
	"sigs.k8s.io/hierarchical-namespaces/internal/metadata"
)

func IsSingletonRQ(rq *v1.ResourceQuota) bool {
	return rq.Name == api.ResourceQuotaSingletonName
}

func IsScopedRQ(rq *v1.ResourceQuota) bool {
	return !IsSingletonRQ(rq)
}

// IsHNCManagedRQ checks if an RQ has the HNC cleanup label
func IsHNCManagedRQ(rq *v1.ResourceQuota) bool {
	if label, ok := metadata.GetLabel(rq, api.HRQLabelCleanup); ok {
		return label == "true"
	}
	return false
}

// IsLegacyScopedRQ checks if an RQ is a legacy scoped RQ (old format without namespace)
// and is managed by HNC
func IsLegacyScopedRQ(rq *v1.ResourceQuota) bool {
	if IsSingletonRQ(rq) {
		return false
	}
	// Only consider RQs managed by HNC for migration
	if !IsHNCManagedRQ(rq) {
		return false
	}
	// Legacy format: hnc-singleton-hrqname (no namespace part)
	// New format: hnc-singleton-namespace--hrqname
	trimmed := strings.TrimPrefix(rq.Name, api.ResourceQuotaSingletonName+"-")
	return !strings.Contains(trimmed, "--")
}

// LegacyScopedRQName generates the legacy RQ name for backward compatibility
func LegacyScopedRQName(hrqName string) string {
	return api.ResourceQuotaSingletonName + "-" + hrqName
}

// HRQNameFromLegacyRQName extracts HRQ name from legacy RQ name
func HRQNameFromLegacyRQName(rqName string) (string, error) {
	if rqName == api.ResourceQuotaSingletonName {
		return "", fmt.Errorf("invalid legacy RQ name: %s", rqName)
	}

	hrqName := strings.TrimPrefix(rqName, api.ResourceQuotaSingletonName+"-")
	if hrqName == rqName {
		return "", fmt.Errorf("not a legacy scoped RQ name: %s", rqName)
	}

	return hrqName, nil
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
