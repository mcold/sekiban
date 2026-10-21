package main

import (
	"database/sql"
	"strconv"
	"strings"
)

// The Cluster screen: the frontend that answers the queries, the backends
// that do the work, and the health of the tablets in between.

const (
	sqlFrontendsProc = "SHOW PROC '/frontends'"
	sqlStatisticProc = "SHOW PROC '/statistic'"
)

// frontend is one frontend, as seen in SHOW PROC '/frontends'.
type frontend struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IP        string `json:"ip"`
	Role      string `json:"role"`
	Alive     bool   `json:"alive"`
	Helper    bool   `json:"helper"`
	Heartbeat string `json:"heartbeat"`
	Started   string `json:"started"`
	JournalID int64  `json:"journalId"`
	Version   string `json:"version"`
	ErrMsg    string `json:"errMsg"`
	QueryPort string `json:"queryPort"`
	HTTPPort  string `json:"httpPort"`
}

// backend is one backend, as seen in SHOW PROC '/backends'.
type backend struct {
	ID             string  `json:"id"`
	IP             string  `json:"ip"`
	Alive          bool    `json:"alive"`
	Version        string  `json:"version"`
	CPUCores       float64 `json:"cpuCores"`
	CPUUsedPct     float64 `json:"cpuUsedPct"`
	MemLimit       float64 `json:"memLimit"`
	MemUsedPct     float64 `json:"memUsedPct"`
	DiskTotal      float64 `json:"diskTotal"`
	DiskUsed       float64 `json:"diskUsed"`
	DiskUsedPct    float64 `json:"diskUsedPct"`
	TabletNum      float64 `json:"tabletNum"`
	RunningQueries float64 `json:"runningQueries"`
	Heartbeat      string  `json:"heartbeat"`
	Started        string  `json:"started"`
	StatusCode     string  `json:"statusCode"`
	ErrMsg         string  `json:"errMsg"`
}

// clusterStat is the health of the tablets, summed over the cluster.
type clusterStat struct {
	Databases    int `json:"databases"`
	Tables       int `json:"tables"`
	Partitions   int `json:"partitions"`
	Indexes      int `json:"indexes"`
	Tablets      int `json:"tablets"`
	Replicas     int `json:"replicas"`
	Unhealthy    int `json:"unhealthy"`
	Inconsistent int `json:"inconsistent"`
	Cloning      int `json:"cloning"`
	ErrorState   int `json:"errorState"`
}

// clusterOut is what the Cluster screen shows.
type clusterOut struct {
	Error     string      `json:"error,omitempty"`
	Frontends []frontend  `json:"frontends"`
	Backends  []backend   `json:"backends"`
	Stat      clusterStat `json:"stat"`
}

// collectCluster reads the cluster's shape and its health.
func collectCluster(db *sql.DB) (clusterOut, error) {
	out := clusterOut{Frontends: []frontend{}, Backends: []backend{}}

	fes, err := collectFrontends(db)
	if err != nil {
		return out, err
	}
	out.Frontends = fes

	bes, err := collectBackends(db)
	if err != nil {
		return out, err
	}
	out.Backends = bes

	stat, err := collectClusterStat(db)
	if err != nil {
		return out, err
	}
	out.Stat = stat
	return out, nil
}

// collectFrontends reads the frontends.
func collectFrontends(db *sql.DB) ([]frontend, error) {
	res, err := queryRows(db, sqlFrontendsProc)
	if err != nil {
		return nil, err
	}
	out := make([]frontend, 0, len(res.rows))
	for i := range res.rows {
		f := frontend{
			ID:        res.value(i, "Id"),
			Name:      res.value(i, "Name"),
			IP:        res.value(i, "IP"),
			Role:      res.value(i, "Role"),
			Alive:     strings.EqualFold(res.value(i, "Alive"), "true"),
			Helper:    strings.EqualFold(res.value(i, "IsHelper"), "true"),
			Heartbeat: res.value(i, "LastHeartbeat"),
			Started:   res.value(i, "StartTime"),
			Version:   res.value(i, "Version"),
			ErrMsg:    res.value(i, "ErrMsg"),
			QueryPort: res.value(i, "QueryPort"),
			HTTPPort:  res.value(i, "HttpPort"),
		}
		if v, err := strconv.ParseInt(strings.TrimSpace(res.value(i, "ReplayedJournalId")), 10, 64); err == nil {
			f.JournalID = v
		}
		out = append(out, f)
	}
	return out, nil
}

// collectBackends reads the backends and their resource use.
func collectBackends(db *sql.DB) ([]backend, error) {
	res, err := queryRows(db, "SHOW PROC '/backends'")
	if err != nil {
		return nil, err
	}
	out := make([]backend, 0, len(res.rows))
	for i := range res.rows {
		b := backend{
			ID:             res.value(i, "BackendId"),
			IP:             res.value(i, "IP"),
			Alive:          strings.EqualFold(res.value(i, "Alive"), "true"),
			Version:        res.value(i, "Version"),
			CPUCores:       numOrZero(res.value(i, "CpuCores")),
			TabletNum:      numOrZero(res.value(i, "TabletNum")),
			RunningQueries: numOrZero(res.value(i, "NumRunningQueries")),
			Heartbeat:      res.value(i, "LastHeartbeat"),
			Started:        res.value(i, "LastStartTime"),
			StatusCode:     res.value(i, "StatusCode"),
			ErrMsg:         res.value(i, "ErrMsg"),
		}
		if v, ok := humanBytes(res.value(i, "MemLimit")); ok {
			b.MemLimit = v
		}
		if v, ok := parseHuman(res.value(i, "MemUsedPct")); ok {
			b.MemUsedPct = v.Value
		}
		if v, ok := parseHuman(res.value(i, "CpuUsedPct")); ok {
			b.CPUUsedPct = v.Value
		}
		if v, ok := humanBytes(res.value(i, "TotalCapacity")); ok {
			b.DiskTotal = v
		}
		if v, ok := humanBytes(res.value(i, "DataUsedCapacity")); ok {
			b.DiskUsed = v
		}
		if v, ok := parseHuman(res.value(i, "UsedPct")); ok {
			b.DiskUsedPct = v.Value
		}
		out = append(out, b)
	}
	return out, nil
}

// collectClusterStat reads the tablet health of the whole cluster.
//
// The proc lists one row per database and a last row whose id is "Total",
// which is the cluster-wide sum: the number of tablets, their replicas, and
// the ones that are unhealthy, inconsistent, cloning or in error.
func collectClusterStat(db *sql.DB) (clusterStat, error) {
	res, err := queryRows(db, sqlStatisticProc)
	if err != nil {
		return clusterStat{}, err
	}
	var stat clusterStat
	for i := range res.rows {
		if !strings.EqualFold(strings.TrimSpace(res.value(i, "DbId")), "Total") {
			continue
		}
		stat.Tables = int(numOrZero(res.value(i, "TableNum")))
		stat.Partitions = int(numOrZero(res.value(i, "PartitionNum")))
		stat.Indexes = int(numOrZero(res.value(i, "IndexNum")))
		stat.Tablets = int(numOrZero(res.value(i, "TabletNum")))
		stat.Replicas = int(numOrZero(res.value(i, "ReplicaNum")))
		stat.Unhealthy = int(numOrZero(res.value(i, "UnhealthyTabletNum")))
		stat.Inconsistent = int(numOrZero(res.value(i, "InconsistentTabletNum")))
		stat.Cloning = int(numOrZero(res.value(i, "CloningTabletNum")))
		stat.ErrorState = int(numOrZero(res.value(i, "ErrorStateTabletNum")))
	}
	// The Total row carries the database count in the name column.
	if len(res.rows) > 0 {
		last := len(res.rows) - 1
		if n, err := strconv.Atoi(strings.TrimSpace(res.value(last, "DbName"))); err == nil {
			stat.Databases = n
		}
	}
	return stat, nil
}
