package main

import (
	"database/sql"
)

// The colocation groups screen: the groups of tables whose data is laid out
// so that a join between them runs without moving a row.
//
// A colocation group names a bucket count, a replication number and a
// distribution key, and every table that joins the group is bucketed the
// same way. SHOW PROC '/colocation_group' lists the groups; the proc path
// under one of them is where each bucket's replicas live.

const sqlColocationGroups = "SHOW PROC '/colocation_group'"

// colocationGroup is one group of colocated tables.
type colocationGroup struct {
	// GroupID is the id the frontend answers for the group, and the last
	// part of its proc path.
	GroupID string `json:"groupId"`
	// GroupName is the name the group was created with.
	GroupName string `json:"groupName"`
	// TableIDs and TableNames are the tables in the group, written by the
	// frontend as comma-separated lists. A dropped table leaves its id
	// behind, marked with an asterisk, and its name as [deleted].
	TableIDs   string `json:"tableIds"`
	TableNames string `json:"tableNames"`
	// Buckets is how many buckets the group is split into, and Replicas
	// how many copies of each bucket the backends hold.
	Buckets  string `json:"buckets"`
	Replicas string `json:"replicas"`
	// DistCols is the distribution key, as the frontend prints the type
	// with it: "bigint(20)".
	DistCols string `json:"distCols"`
	// Stable is what the frontend calls the group: IsStable, as it prints
	// it.
	Stable bool `json:"stable"`
}

// colocationOut is what the list shows.
type colocationOut struct {
	Error  string            `json:"error,omitempty"`
	Groups []colocationGroup `json:"groups"`
}

// colocationBucket is one bucket of a group, and the backends that hold it.
type colocationBucket struct {
	BucketIndex string `json:"bucketIndex"`
	// BackendIDs are the backends that hold this bucket's replicas,
	// comma-separated, in the frontend's order.
	BackendIDs string `json:"backendIds"`
}

// colocationDetailOut is one group's placement: where its buckets live.
type colocationDetailOut struct {
	Error string `json:"error,omitempty"`
	// GroupID is the group the buckets belong to.
	GroupID string `json:"groupId"`
	// Buckets are the group's buckets, in the frontend's order.
	Buckets []colocationBucket `json:"buckets"`
}

// collectColocationGroups reads the groups the frontend holds.
func collectColocationGroups(db *sql.DB) colocationOut {
	out := colocationOut{Groups: []colocationGroup{}}
	res, err := queryRows(db, sqlColocationGroups)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	for i := range res.rows {
		out.Groups = append(out.Groups, colocationGroup{
			GroupID:    res.value(i, "GroupId"),
			GroupName:  res.value(i, "GroupName"),
			TableIDs:   res.value(i, "TableIds"),
			TableNames: res.value(i, "TableNames"),
			Buckets:    res.value(i, "BucketsNum"),
			Replicas:   res.value(i, "ReplicationNum"),
			DistCols:   res.value(i, "DistCols"),
			Stable:     res.value(i, "IsStable") == "true",
		})
	}
	return out
}

// collectColocationDetail reads where one group's buckets live.
func collectColocationDetail(db *sql.DB, groupID string) colocationDetailOut {
	out := colocationDetailOut{GroupID: groupID, Buckets: []colocationBucket{}}
	if !validGroupID(groupID) {
		out.Error = "not a colocation group id: " + groupID
		return out
	}
	res, err := queryRows(db, "SHOW PROC '/colocation_group/"+groupID+"'")
	if err != nil {
		out.Error = err.Error()
		return out
	}
	for i := range res.rows {
		out.Buckets = append(out.Buckets, colocationBucket{
			BucketIndex: res.value(i, "BucketIndex"),
			BackendIDs:  res.value(i, "BackendIds"),
		})
	}
	return out
}

// validGroupID reports whether id is safe to put into the proc path.
//
// The group id comes from the page, and the proc path is assembled into the
// statement, so it is held to the shape the frontend answers with: digits,
// and the separators a group id is written with.
func validGroupID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}
