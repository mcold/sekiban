package main

import (
	"database/sql"
)

// The Resources screen: what the cluster is made of, and the limits that
// decide who gets what.
//
// StarRocks has moved the old per-user limits into three places: resource
// groups, which cap a workload's share; warehouses, which hold the compute
// a query runs on; and the configuration itself.

// resource is one entry of the cluster's configuration.
type resource struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Key  string `json:"key"`
	// Value is the raw value: it can be a number, a size or a path.
	Value string `json:"value"`
}

// warehouse is one warehouse: a pool of compute that queries run on.
type warehouse struct {
	ID                  string  `json:"id"`
	Name                string  `json:"name"`
	State               string  `json:"state"`
	NodeCount           float64 `json:"nodeCount"`
	CurrentClusterCount float64 `json:"currentClusterCount"`
	MaxClusterCount     float64 `json:"maxClusterCount"`
	StartedClusters     float64 `json:"startedClusters"`
	RunningSQL          float64 `json:"runningSql"`
	QueuedSQL           float64 `json:"queuedSql"`
	Property            string  `json:"property"`
	Comment             string  `json:"comment"`
}

// resourceGroup is one resource group: the limits that apply to the queries
// that match its classifiers.
type resourceGroup struct {
	Name                   string  `json:"name"`
	ID                     string  `json:"id"`
	CPUWeight              float64 `json:"cpuWeight"`
	ExclusiveCPUCores      float64 `json:"exclusiveCpuCores"`
	MemLimit               string  `json:"memLimit"`
	BigQueryCPUSecondLimit string  `json:"bigQueryCpuSecondLimit"`
	BigQueryScanRowsLimit  string  `json:"bigQueryScanRowsLimit"`
	BigQueryMemLimit       string  `json:"bigQueryMemLimit"`
	ConcurrencyLimit       float64 `json:"concurrencyLimit"`
	SpillMemLimitThreshold string  `json:"spillMemLimitThreshold"`
	Classifiers            string  `json:"classifiers"`
}

// warehouseQueue is the queue state of one warehouse.
type warehouseQueue struct {
	WarehouseID       string  `json:"warehouseId"`
	WarehouseName     string  `json:"warehouseName"`
	QueuePending      float64 `json:"queuePending"`
	QueueRunning      float64 `json:"queueRunning"`
	MaxPending        float64 `json:"maxPending"`
	MaxPendingSeconds float64 `json:"maxPendingSeconds"`
	EarliestWait      float64 `json:"earliestWait"`
	MaxRequiredSlots  float64 `json:"maxRequiredSlots"`
	SumRequiredSlots  float64 `json:"sumRequiredSlots"`
	RemainingSlots    float64 `json:"remainingSlots"`
	MaxSlots          float64 `json:"maxSlots"`
}

// resourcesOut is what the screen shows.
type resourcesOut struct {
	Error      string           `json:"error,omitempty"`
	Resources  []resource       `json:"resources"`
	Groups     []resourceGroup  `json:"groups"`
	Warehouses []warehouse      `json:"warehouses"`
	Queues     []warehouseQueue `json:"queues"`
}

// collectResources reads the cluster's resources and limits.
func collectResources(db *sql.DB) resourcesOut {
	out := resourcesOut{
		Resources:  []resource{},
		Groups:     []resourceGroup{},
		Warehouses: []warehouse{},
		Queues:     []warehouseQueue{},
	}

	res, err := queryRows(db, "SHOW PROC '/resources'")
	if err == nil {
		for i := range res.rows {
			out.Resources = append(out.Resources, resource{
				Name:  res.value(i, "Name"),
				Type:  res.value(i, "ResourceType"),
				Key:   res.value(i, "Key"),
				Value: res.value(i, "Value"),
			})
		}
	}

	whs, err := queryRows(db, "SHOW PROC '/warehouses'")
	if err == nil {
		for i := range whs.rows {
			out.Warehouses = append(out.Warehouses, warehouse{
				ID:                  whs.value(i, "Id"),
				Name:                whs.value(i, "Name"),
				State:               whs.value(i, "State"),
				NodeCount:           numOrZero(whs.value(i, "NodeCount")),
				CurrentClusterCount: numOrZero(whs.value(i, "CurrentClusterCount")),
				MaxClusterCount:     numOrZero(whs.value(i, "MaxClusterCount")),
				StartedClusters:     numOrZero(whs.value(i, "StartedClusters")),
				RunningSQL:          numOrZero(whs.value(i, "RunningSql")),
				QueuedSQL:           numOrZero(whs.value(i, "QueuedSql")),
				Property:            whs.value(i, "Property"),
				Comment:             whs.value(i, "Comment"),
			})
		}
	}

	groups, err := queryRows(db, "SHOW RESOURCE GROUPS")
	if err == nil {
		for i := range groups.rows {
			out.Groups = append(out.Groups, resourceGroup{
				Name:                   groups.value(i, "name"),
				ID:                     groups.value(i, "id"),
				CPUWeight:              numOrZero(groups.value(i, "cpu_weight")),
				ExclusiveCPUCores:      numOrZero(groups.value(i, "exclusive_cpu_cores")),
				MemLimit:               groups.value(i, "mem_limit"),
				BigQueryCPUSecondLimit: groups.value(i, "big_query_cpu_second_limit"),
				BigQueryScanRowsLimit:  groups.value(i, "big_query_scan_rows_limit"),
				BigQueryMemLimit:       groups.value(i, "big_query_mem_limit"),
				ConcurrencyLimit:       numOrZero(groups.value(i, "concurrency_limit")),
				SpillMemLimitThreshold: groups.value(i, "spill_mem_limit_threshold"),
				Classifiers:            groups.value(i, "classifiers"),
			})
		}
	}

	queues, err := queryRows(db, sqlWarehouseMetrics)
	if err == nil {
		for i := range queues.rows {
			out.Queues = append(out.Queues, warehouseQueue{
				WarehouseID:       queues.value(i, "WAREHOUSE_ID"),
				WarehouseName:     queues.value(i, "WAREHOUSE_NAME"),
				QueuePending:      numOrZero(queues.value(i, "QUEUE_PENDING_LENGTH")),
				QueueRunning:      numOrZero(queues.value(i, "QUEUE_RUNNING_LENGTH")),
				MaxPending:        numOrZero(queues.value(i, "MAX_PENDING_LENGTH")),
				MaxPendingSeconds: numOrZero(queues.value(i, "MAX_PENDING_TIME_SECOND")),
				EarliestWait:      numOrZero(queues.value(i, "EARLIEST_QUERY_WAIT_TIME")),
				MaxRequiredSlots:  numOrZero(queues.value(i, "MAX_REQUIRED_SLOTS")),
				SumRequiredSlots:  numOrZero(queues.value(i, "SUM_REQUIRED_SLOTS")),
				RemainingSlots:    numOrZero(queues.value(i, "REMAIN_SLOTS")),
				MaxSlots:          numOrZero(queues.value(i, "MAX_SLOTS")),
			})
		}
	}
	return out
}

// sqlWarehouseMetrics is the queue state of the warehouses.
const sqlWarehouseMetrics = `SELECT WAREHOUSE_ID, WAREHOUSE_NAME, QUEUE_PENDING_LENGTH,
	                                 QUEUE_RUNNING_LENGTH, MAX_PENDING_LENGTH,
	                                 MAX_PENDING_TIME_SECOND, EARLIEST_QUERY_WAIT_TIME,
	                                 MAX_REQUIRED_SLOTS, SUM_REQUIRED_SLOTS,
	                                 REMAIN_SLOTS, MAX_SLOTS
	                            FROM information_schema.warehouse_metrics`
