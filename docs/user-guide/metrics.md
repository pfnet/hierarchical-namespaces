# Metrics

The metrics endpoint is exposed on `:8080` by default (customizable with the `-metrics-addr` flag).

## Metric List

### `hnc_hierarchicalresourcequota`

This metric exposes resource limits and usage for HierarchicalResourceQuotas.

#### Labels

- `hrq`: Name of the HierarchicalResourceQuota
- `namespace`: Namespace of the HierarchicalResourceQuota
- `resource`: Resource type (e.g., `cpu`, `memory`, `pods`)
- `type`: Either `hard` (limit) or `used` (current usage)

#### Example

```
# HELP hnc_hierarchicalresourcequota HRQ hard/used like kube_resourcequota
# TYPE hnc_hierarchicalresourcequota gauge
hnc_hierarchicalresourcequota{hrq="team-quota",namespace="team-a",resource="cpu",type="hard"} 100
hnc_hierarchicalresourcequota{hrq="team-quota",namespace="team-a",resource="cpu",type="used"} 45
hnc_hierarchicalresourcequota{hrq="team-quota",namespace="team-a",resource="memory",type="hard"} 536870912
hnc_hierarchicalresourcequota{hrq="team-quota",namespace="team-a",resource="memory",type="used"} 268435456
```
