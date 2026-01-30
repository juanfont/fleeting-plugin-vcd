package vcd

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// All metrics include "instance_group" label to differentiate between multiple runners
const instanceGroupLabel = "instance_group"

var (
	// Instance Lifecycle Metrics
	InstancesCreatedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fleeting_vcd_instances_created_total",
		Help: "Total number of instances created",
	}, []string{instanceGroupLabel})

	InstancesDeletedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fleeting_vcd_instances_deleted_total",
		Help: "Total number of instances deleted",
	}, []string{instanceGroupLabel})

	InstancesFailedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fleeting_vcd_instances_failed_total",
		Help: "Total number of failed instance operations",
	}, []string{instanceGroupLabel, "operation"})

	InstancesCurrentByState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fleeting_vcd_instances_current",
		Help: "Current number of instances by state",
	}, []string{instanceGroupLabel, "state"})

	InstanceCreationDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fleeting_vcd_instance_creation_duration_seconds",
		Help:    "Time taken to create an instance",
		Buckets: []float64{30, 60, 90, 120, 180, 300, 600, 900, 1200}, // 30s to 20min
	}, []string{instanceGroupLabel})

	InstanceDeletionDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fleeting_vcd_instance_deletion_duration_seconds",
		Help:    "Time taken to delete an instance",
		Buckets: []float64{10, 30, 60, 120, 300, 600, 1200, 1800, 3600}, // 10s to 60min
	}, []string{instanceGroupLabel})

	// Garbage Collection Metrics
	GCRunsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fleeting_vcd_gc_runs_total",
		Help: "Total number of garbage collection runs",
	}, []string{instanceGroupLabel})

	GCInstancesCollectedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fleeting_vcd_gc_instances_collected_total",
		Help: "Total number of instances cleaned up by garbage collection",
	}, []string{instanceGroupLabel})

	GCDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fleeting_vcd_gc_duration_seconds",
		Help:    "Time taken to run garbage collection",
		Buckets: []float64{1, 5, 10, 30, 60, 120, 300},
	}, []string{instanceGroupLabel})

	// VCD API Metrics
	APICallsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fleeting_vcd_api_calls_total",
		Help: "Total number of VCD API calls",
	}, []string{instanceGroupLabel, "operation"})

	APIErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fleeting_vcd_api_errors_total",
		Help: "Total number of VCD API errors",
	}, []string{instanceGroupLabel, "operation"})

	APIDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fleeting_vcd_api_duration_seconds",
		Help:    "Time taken for VCD API calls",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60},
	}, []string{instanceGroupLabel, "operation"})

	// Pool Status Metrics
	PoolSize = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fleeting_vcd_pool_size",
		Help: "Current pool size",
	}, []string{instanceGroupLabel})

	PoolMaxSize = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fleeting_vcd_pool_max_size",
		Help: "Configured maximum pool size",
	}, []string{instanceGroupLabel})

	// State Manager Metrics
	StateManagerInstancesTotal = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fleeting_vcd_state_manager_instances_total",
		Help: "Total instances tracked in state manager",
	}, []string{instanceGroupLabel})

	StateManagerInstancesByState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fleeting_vcd_state_manager_instances_by_state",
		Help: "Instances by fleeting state in state manager",
	}, []string{instanceGroupLabel, "state"})

	StateManagerInstancesPreexisting = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fleeting_vcd_state_manager_instances_preexisting",
		Help: "Preexisting instances (discovered, not created by us)",
	}, []string{instanceGroupLabel})

	StateManagerUpdatesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fleeting_vcd_state_manager_updates_total",
		Help: "Total state manager update operations",
	}, []string{instanceGroupLabel})

	StateManagerPrunesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fleeting_vcd_state_manager_prunes_total",
		Help: "Total state manager prune operations",
	}, []string{instanceGroupLabel})

	StateManagerGetsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fleeting_vcd_state_manager_gets_total",
		Help: "Total state manager get operations",
	}, []string{instanceGroupLabel, "found"})
)
