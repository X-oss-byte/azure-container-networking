package metrics

import (
	"github.com/Azure/azure-container-networking/npm/util"
	"github.com/prometheus/client_golang/prometheus"
)

// RecordACLLatency should be used in Windows DP to record the latency of individual ACL operations.
func RecordACLLatency(timer *Timer, op OperationKind) {
	if util.IsWindowsDP() {
		labels := prometheus.Labels{
			operationLabel: string(op),
		}
		aclLatency.With(labels).Observe(timer.timeElapsed())
	}
}

// IncACLFailures should be used in Windows DP to record the number of failures for individual ACL operations.
func IncACLFailures(op OperationKind) {
	if util.IsWindowsDP() {
		labels := prometheus.Labels{
			operationLabel: string(op),
		}
		aclFailures.With(labels).Inc()
	}
}
