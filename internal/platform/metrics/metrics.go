// Package metrics declares the Prometheus metrics exported on :9100/metrics.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Registry is the process-wide registry.
var Registry = prometheus.NewRegistry()

var (
	HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_http_requests_total", Help: "HTTP requests by route, method and status.",
	}, []string{"route", "method", "status"})
	HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "hammurapi_http_request_duration_seconds", Help: "HTTP request latency.", Buckets: prometheus.DefBuckets,
	}, []string{"route", "method"})
	GitAPIErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_git_api_errors_total", Help: "Failed git provider API calls.",
	}, []string{"provider", "op"})
	WebhookEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_webhook_events_total", Help: "Webhook events by result.",
	}, []string{"result"})
	KafkaLag = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hammurapi_kafka_consumer_lag", Help: "Kafka consumer lag by topic.",
	}, []string{"topic"})
	AgentSessions = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "hammurapi_agent_sessions_active", Help: "Active ACP sessions in this pod.",
	})
	AgentProcesses = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "hammurapi_agent_processes_up", Help: "Running agent processes in this pod.",
	})
	GitProviderUp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "hammurapi_git_provider_up", Help: "1 if the last git provider call succeeded.",
	})
	GateTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_gate_transitions_total", Help: "Gate status transitions.",
	}, []string{"area", "to"})
)

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		HTTPRequests, HTTPDuration, GitAPIErrors, WebhookEvents, KafkaLag,
		AgentSessions, AgentProcesses, GitProviderUp, GateTransitions,
	)
}
