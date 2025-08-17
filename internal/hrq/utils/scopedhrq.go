package utils

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
	"sigs.k8s.io/hierarchical-namespaces/internal/metadata"
)

const (
	hrqNameLabel      = "hnc.x-k8s.io/hrq-name"
	hrqNamespaceLabel = "hnc.x-k8s.io/hrq-namespace"
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
	_, nsLabelFound := metadata.GetLabel(rq, hrqNamespaceLabel)
	_, nameLabelFound := metadata.GetLabel(rq, hrqNameLabel)
	return !nsLabelFound || !nameLabelFound
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
	hash := md5.Sum([]byte(fmt.Sprintf("%s/%s", hrqNamespace, hrqName)))
	hashStr := hex.EncodeToString(hash[:])

	namespaceAndName := truncate(
		fmt.Sprintf("%s-%s", hrqNamespace, hrqName),
		validation.DNS1123SubdomainMaxLength-len(hashStr)-len(api.ResourceQuotaSingletonName)-2,
	)

	return fmt.Sprintf("%s-%s-%s", api.ResourceQuotaSingletonName, namespaceAndName, hashStr), nil
}

func ScopedHRQNameFromRQ(rq *v1.ResourceQuota) (types.NamespacedName, error) {
	namespace, nsOK := metadata.GetLabel(rq, hrqNamespaceLabel)
	name, nameOK := metadata.GetLabel(rq, hrqNameLabel)
	if nsOK && nameOK {
		return types.NamespacedName{Namespace: namespace, Name: name}, nil
	}
	return types.NamespacedName{}, fmt.Errorf("no matching HRQ found for RQ: %s", rq.Name)
}

func SetLabelsAnnotationsForScopedRQ(rq *v1.ResourceQuota, hrqNamespace string, hrqName string) {
	metadata.SetLabel(rq, hrqNamespaceLabel, hrqNamespace)
	metadata.SetLabel(rq, hrqNameLabel, hrqName)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
