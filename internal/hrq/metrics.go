package hrq

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
)

func RegisterMetrics(mgr ctrl.Manager) error {
	if err := metrics.Registry.Register(&hrqCollector{
		client:  mgr.GetClient(),
		logger:  mgr.GetLogger().WithValues("collector", "hrqCollector"),
		timeout: time.Second * 10,
	}); err != nil {
		return err
	}
	return nil
}

type hrqCollector struct {
	timeout time.Duration
	client  client.Client
	logger  logr.Logger
}

func (c *hrqCollector) desc() *prometheus.Desc {
	return prometheus.NewDesc(
		"hnc_hierarchicalresourcequota",
		"HRQ hard/used like kube_resourcequota",
		[]string{"namespace", "hrq", "resource", "type"},
		nil,
	)
}

func (c *hrqCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc()
}

func (c *hrqCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	var hrqs api.HierarchicalResourceQuotaList
	if err := c.client.List(ctx, &hrqs); err != nil {
		c.logger.Error(err, "Failed to list HRQs during metrics collection")
		return
	}

	for _, hrq := range hrqs.Items {
		for typeLabel, resList := range map[string]corev1.ResourceList{
			"hard": hrq.Status.Hard,
			"used": hrq.Status.Used,
		} {
			for res, qty := range resList {
				v := qty.AsApproximateFloat64()
				ch <- prometheus.MustNewConstMetric(
					c.desc(),
					prometheus.GaugeValue,
					float64(v),
					hrq.Namespace,
					hrq.Name,
					string(res),
					typeLabel,
				)
			}
		}
	}
}
