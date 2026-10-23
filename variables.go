package main

import (
	"database/sql"
	"strings"
)

// The Frontend variables screen: the configuration the frontend runs with.
//
// ADMIN SHOW FRONTEND CONFIG is the SQL view of it, and it is the same list
// the frontend's own /variable page prints: one row per setting, with its
// current value, its type, whether it can be changed while the frontend is
// running, and what it means.

const sqlFrontendConfig = "ADMIN SHOW FRONTEND CONFIG"

// variableEntry is one setting of the frontend.
type variableEntry struct {
	Key string `json:"key"`
	// Value is the setting as the frontend holds it, written the way it
	// would be written in fe.conf.
	Value string `json:"value"`
	// Type is the setting's type: String, int, long, boolean, and so on.
	Type string `json:"type"`
	// Mutable says whether the setting can be changed at run time.
	Mutable bool `json:"mutable"`
	// Comment is what the setting does.
	Comment string `json:"comment"`
}

// variablesOut is what the screen shows.
type variablesOut struct {
	Error string `json:"error,omitempty"`
	// Rows are the settings, in the order the frontend answers them: by
	// key.
	Rows []variableEntry `json:"rows"`
	// MutableCount is how many of them can be changed at run time.
	MutableCount int `json:"mutableCount"`
}

// collectVariables reads the frontend's configuration.
func collectVariables(db *sql.DB) variablesOut {
	out := variablesOut{Rows: []variableEntry{}}
	res, err := queryRows(db, sqlFrontendConfig)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	for i := range res.rows {
		entry := variableEntry{
			Key:     res.value(i, "Key"),
			Value:   res.value(i, "Value"),
			Type:    res.value(i, "Type"),
			Mutable: strings.EqualFold(res.value(i, "IsMutable"), "true"),
			Comment: res.value(i, "Comment"),
		}
		if entry.Mutable {
			out.MutableCount++
		}
		out.Rows = append(out.Rows, entry)
	}
	return out
}
