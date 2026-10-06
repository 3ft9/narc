package main

import (
	"github.com/actions/scaleset"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	scaleSetLabels = []string{"target", "scaleset"}

	runnersByState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "narc_runners",
		Help: "Live runners by state (starting, idle, busy).",
	}, append(scaleSetLabels, "state"))
	assignedJobs = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "narc_github_assigned_jobs",
		Help: "Jobs assigned to the scale set, from GitHub statistics.",
	}, scaleSetLabels)
	runningJobs = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "narc_github_running_jobs",
		Help: "Jobs running on the scale set, from GitHub statistics.",
	}, scaleSetLabels)
	dispatchFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "narc_dispatch_failures_total",
		Help: "Failed attempts to start a runner (JIT config, variable write or dispatch).",
	}, scaleSetLabels)
	bootToJobStart = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "narc_boot_to_job_start_seconds",
		Help:    "Time from dispatch to the runner starting a job.",
		Buckets: []float64{5, 10, 20, 30, 45, 60, 90, 120, 180, 300, 600, 1800},
	}, scaleSetLabels)
	deregistrations = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "narc_runner_deregistrations_total",
		Help: "Runners removed from GitHub because their allocation ended without completing a job.",
	}, scaleSetLabels)
	variableSweeps = promauto.NewCounter(prometheus.CounterOpts{
		Name: "narc_variable_sweeps_total",
		Help: "Orphaned JIT config variables deleted by the startup sweep.",
	})
)

// statsRecorder feeds listener statistics into the GitHub gauges.
type statsRecorder struct{ target, name string }

func (r statsRecorder) RecordStatistics(s *scaleset.RunnerScaleSetStatistic) {
	if s == nil {
		return
	}
	assignedJobs.WithLabelValues(r.target, r.name).Set(float64(s.TotalAssignedJobs))
	runningJobs.WithLabelValues(r.target, r.name).Set(float64(s.TotalRunningJobs))
}
func (statsRecorder) RecordJobStarted(*scaleset.JobStarted)     {}
func (statsRecorder) RecordJobCompleted(*scaleset.JobCompleted) {}
func (statsRecorder) RecordDesiredRunners(int)                  {}
