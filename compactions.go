package main

import (
	"database/sql"
	"strconv"
	"strings"
)

// The Compactions and Tablet health screen.
//
// Compaction is the real background cost of an MPP engine: the work that
// merges the small files a load leaves behind. It is also the closest thing
// StarRocks has to PostgreSQL's vacuum, which the cluster does not have.

// compaction is one compaction in flight.
type compaction struct {
	Partition  string `json:"partition"`
	TxnID      string `json:"txnId"`
	StartTime  string `json:"startTime"`
	CommitTime string `json:"commitTime"`
	FinishTime string `json:"finishTime"`
	Error      string `json:"error"`
	// Profile is the server's own progress report for the compaction.
	Profile string `json:"profile"`
}

// tabletHealth is the health of the tablets of one database.
type tabletHealth struct {
	Unhealthy    int `json:"unhealthy"`
	Inconsistent int `json:"inconsistent"`
	Cloning      int `json:"cloning"`
	ErrorState   int `json:"errorState"`
	// The tablet ids themselves, so a problem can be named rather than
	// only counted.
	UnhealthyTablets    []string `json:"unhealthyTablets"`
	InconsistentTablets []string `json:"inconsistentTablets"`
	CloningTablets      []string `json:"cloningTablets"`
	ErrorStateTablets   []string `json:"errorStateTablets"`
}

// beCompaction is the compaction state of one backend.
type beCompaction struct {
	BEID                  string  `json:"beId"`
	Candidates            float64 `json:"candidates"`
	BaseConcurrency       float64 `json:"baseConcurrency"`
	CumulativeConcurrency float64 `json:"cumulativeConcurrency"`
	LatestScore           float64 `json:"latestScore"`
	CandidateMaxScore     float64 `json:"candidateMaxScore"`
	ManualConcurrency     float64 `json:"manualConcurrency"`
	ManualCandidates      float64 `json:"manualCandidates"`
}

// beTxn is a transaction in flight on a backend.
type beTxn struct {
	BEID        string  `json:"beId"`
	LoadID      string  `json:"loadId"`
	TxnID       string  `json:"txnId"`
	PartitionID string  `json:"partitionId"`
	TabletID    string  `json:"tabletId"`
	CreateTime  string  `json:"createTime"`
	CommitTime  string  `json:"commitTime"`
	NumRow      float64 `json:"numRow"`
	DataSize    float64 `json:"dataSize"`
}

// compactionsOut is what the screen shows.
type compactionsOut struct {
	Error       string         `json:"error,omitempty"`
	Compactions []compaction   `json:"compactions"`
	Health      tabletHealth   `json:"health"`
	Backends    []beCompaction `json:"backends"`
	Txns        []beTxn        `json:"txns"`
}

// collectCompactions reads the compaction state of the cluster.
func collectCompactions(db *sql.DB, database string) compactionsOut {
	out := compactionsOut{Compactions: []compaction{}, Backends: []beCompaction{}, Txns: []beTxn{}}

	res, err := queryRows(db, "SHOW PROC '/compactions'")
	if err != nil {
		out.Error = err.Error()
		return out
	}
	for i := range res.rows {
		out.Compactions = append(out.Compactions, compaction{
			Partition:  res.value(i, "Partition"),
			TxnID:      res.value(i, "TxnID"),
			StartTime:  res.value(i, "StartTime"),
			CommitTime: res.value(i, "CommitTime"),
			FinishTime: res.value(i, "FinishTime"),
			Error:      res.value(i, "Error"),
			Profile:    res.value(i, "Profile"),
		})
	}

	dbID, err := databaseID(db, database)
	if err == nil && dbID > 0 {
		out.Health = collectTabletHealth(db, dbID)
	}

	bes, err := queryRows(db, sqlBECompactions)
	if err == nil {
		for i := range bes.rows {
			out.Backends = append(out.Backends, beCompaction{
				BEID:                  bes.value(i, "BE_ID"),
				Candidates:            numOrZero(bes.value(i, "CANDIDATES_NUM")),
				BaseConcurrency:       numOrZero(bes.value(i, "BASE_COMPACTION_CONCURRENCY")),
				CumulativeConcurrency: numOrZero(bes.value(i, "CUMULATIVE_COMPACTION_CONCURRENCY")),
				LatestScore:           numOrZero(bes.value(i, "LATEST_COMPACTION_SCORE")),
				CandidateMaxScore:     numOrZero(bes.value(i, "CANDIDATE_MAX_SCORE")),
				ManualConcurrency:     numOrZero(bes.value(i, "MANUAL_COMPACTION_CONCURRENCY")),
				ManualCandidates:      numOrZero(bes.value(i, "MANUAL_COMPACTION_CANDIDATES_NUM")),
			})
		}
	}

	txns, err := queryRows(db, sqlBETxns)
	if err == nil {
		for i := range txns.rows {
			t := beTxn{
				BEID:        txns.value(i, "BE_ID"),
				LoadID:      txns.value(i, "LOAD_ID"),
				TxnID:       txns.value(i, "TXN_ID"),
				PartitionID: txns.value(i, "PARTITION_ID"),
				TabletID:    txns.value(i, "TABLET_ID"),
				CreateTime:  txns.value(i, "CREATE_TIME"),
				CommitTime:  txns.value(i, "COMMIT_TIME"),
				NumRow:      numOrZero(txns.value(i, "NUM_ROW")),
			}
			if v, ok := humanBytes(txns.value(i, "DATA_SIZE")); ok {
				t.DataSize = v
			}
			out.Txns = append(out.Txns, t)
		}
	}
	return out
}

// collectTabletHealth reads the health of the tablets of one database.
//
// The proc writes the ids of the tablets in each state, not a count, so the
// count is derived from the list.
func collectTabletHealth(db *sql.DB, dbID int64) tabletHealth {
	h := tabletHealth{
		UnhealthyTablets:    []string{},
		InconsistentTablets: []string{},
		CloningTablets:      []string{},
		ErrorStateTablets:   []string{},
	}
	res, err := queryRows(db, "SHOW PROC '/statistic/"+strconv.FormatInt(dbID, 10)+"'")
	if err != nil {
		return h
	}
	if len(res.rows) == 0 {
		return h
	}
	h.UnhealthyTablets = splitIDs(res.value(0, "UnhealthyTablets"))
	h.InconsistentTablets = splitIDs(res.value(0, "InconsistentTablets"))
	h.CloningTablets = splitIDs(res.value(0, "CloningTablets"))
	h.ErrorStateTablets = splitIDs(res.value(0, "ErrorStateTablets"))
	h.Unhealthy = len(h.UnhealthyTablets)
	h.Inconsistent = len(h.InconsistentTablets)
	h.Cloning = len(h.CloningTablets)
	h.ErrorState = len(h.ErrorStateTablets)
	return h
}

// splitIDs reads a comma-separated list of tablet ids.
func splitIDs(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// The backend views, which are the SQL projection of what the backends
// publish about themselves.
const (
	sqlBECompactions = `SELECT BE_ID, CANDIDATES_NUM, BASE_COMPACTION_CONCURRENCY,
	                            CUMULATIVE_COMPACTION_CONCURRENCY,
	                            LATEST_COMPACTION_SCORE, CANDIDATE_MAX_SCORE,
	                            MANUAL_COMPACTION_CONCURRENCY,
	                            MANUAL_COMPACTION_CANDIDATES_NUM
	                       FROM information_schema.be_compactions`

	sqlBETxns = `SELECT BE_ID, LOAD_ID, TXN_ID, PARTITION_ID, TABLET_ID,
	                    CREATE_TIME, COMMIT_TIME, NUM_ROW, DATA_SIZE
	               FROM information_schema.be_txns`
)
