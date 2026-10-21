package main

import (
	"context"
	"database/sql"
	"strings"
)

// The Browser screen: the objects of the connected database, and their
// catalog.
//
// StarRocks has no schema level below the database, so the tree is the
// database and its groups. The catalog view that carries the equivalent of
// pg_class and pg_index is information_schema.tables_config: the table
// model, the primary key, the partition column, the distribution, the sort
// key and the properties.
//
// Materialized views are absent from information_schema.tables and from
// SHOW TABLES, so they are listed from SHOW MATERIALIZED VIEWS.

// browserGroup is one group of the tree: tables, views or materialized
// views.
type browserGroup struct {
	Key     string          `json:"key"`
	Label   string          `json:"label"`
	Objects []browserObject `json:"objects"`
}

// browserObject is one object of the tree.
type browserObject struct {
	Name string `json:"name"`
	// Type is BASE TABLE, VIEW or MATERIALIZED VIEW.
	Type string `json:"type"`
	// Model is the table model: PRIMARY_KEYS, UNIQUE_KEYS, DUP_KEYS or
	// AGG_KEYS. It is empty for a view.
	Model string `json:"model"`
	// Rows is the number of rows the object holds, and Size its size in
	// bytes. They are summed over the partitions.
	Rows float64 `json:"rows"`
	Size float64 `json:"size"`
}

// browserTreeOut is the tree.
type browserTreeOut struct {
	Error    string         `json:"error,omitempty"`
	Database string         `json:"database"`
	Groups   []browserGroup `json:"groups"`
}

// browserColumn is one column of an object.
type browserColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	// Key reads PRI, UNI, DUP or AGG for a key column, and is empty
	// otherwise.
	Key     string `json:"key"`
	Default string `json:"default"`
	Comment string `json:"comment"`
}

// browserPartition is one partition of an object.
type browserPartition struct {
	Name            string  `json:"name"`
	Ordinal         int     `json:"ordinal"`
	PartitionKey    string  `json:"partitionKey"`
	PartitionValue  string  `json:"partitionValue"`
	DistributionKey string  `json:"distributionKey"`
	Buckets         int     `json:"buckets"`
	Replication     int     `json:"replication"`
	Rows            float64 `json:"rows"`
	Size            float64 `json:"size"`
}

// objectDetailOut is everything the object window shows about one object.
type objectDetailOut struct {
	Error      string             `json:"error,omitempty"`
	Name       string             `json:"name"`
	Type       string             `json:"type"`
	Model      string             `json:"model"`
	DDL        string             `json:"ddl"`
	Columns    []browserColumn    `json:"columns"`
	Partitions []browserPartition `json:"partitions"`

	// The catalog of a table, as tables_config reports it.
	PrimaryKey       string `json:"primaryKey"`
	PartitionKey     string `json:"partitionKey"`
	DistributeKey    string `json:"distributeKey"`
	DistributeType   string `json:"distributeType"`
	DistributeBucket int    `json:"distributeBucket"`
	SortKey          string `json:"sortKey"`
	Properties       string `json:"properties"`

	// Editable says whether the grid can be edited: only a primary key
	// table accepts an UPDATE.
	Editable bool `json:"editable"`
	// PKColumns are the columns of the primary key, in order.
	PKColumns []string `json:"pkColumns"`
}

// browserDatabase is the name of the database the connection is on.
func browserDatabase(db *sql.DB) string {
	var name string
	if err := db.QueryRow("SELECT DATABASE()").Scan(&name); err != nil {
		return ""
	}
	return name
}

// collectBrowserTree lists the objects of the connected database.
func collectBrowserTree(db *sql.DB) browserTreeOut {
	out := browserTreeOut{Groups: []browserGroup{}}
	out.Database = browserDatabase(db)
	if out.Database == "" {
		out.Error = "the connection names no database"
		return out
	}

	rows, err := db.Query(sqlObjectList, out.Database)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	res, err := scanResult(rows)
	if err != nil {
		out.Error = err.Error()
		return out
	}

	sizes := objectSizes(db, out.Database)

	tables := []browserObject{}
	views := []browserObject{}
	for i := range res.rows {
		obj := browserObject{
			Name:  res.value(i, "table_name"),
			Type:  res.value(i, "table_type"),
			Model: res.value(i, "TABLE_MODEL"),
		}
		if s, ok := sizes[obj.Name]; ok {
			obj.Rows = s.rows
			obj.Size = s.size
		}
		if strings.EqualFold(obj.Type, "VIEW") {
			views = append(views, obj)
			continue
		}
		tables = append(tables, obj)
	}

	mvs, err := collectMaterializedViews(db, out.Database)
	if err != nil {
		out.Error = err.Error()
		return out
	}

	out.Groups = []browserGroup{
		{Key: "tables", Label: "Tables", Objects: tables},
		{Key: "views", Label: "Views", Objects: views},
		{Key: "materialized_views", Label: "Materialized Views", Objects: mvs},
	}
	return out
}

// objectSize is the size and the row count of one object.
type objectSize struct {
	rows float64
	size float64
}

// objectSizes reads the totals of every object of a database, summed over
// its partitions.
//
// information_schema.partitions is empty on 4.0, so partitions_meta is the
// one that answers.
func objectSizes(db *sql.DB, database string) map[string]objectSize {
	out := map[string]objectSize{}
	rows, err := db.Query(sqlPartitionMeta, database)
	if err != nil {
		return out
	}
	res, err := scanResult(rows)
	if err != nil {
		return out
	}
	for i := range res.rows {
		name := res.value(i, "TABLE_NAME")
		s := out[name]
		s.rows += numOrZero(res.value(i, "ROW_COUNT"))
		if v, ok := humanBytes(res.value(i, "DATA_SIZE")); ok {
			s.size += v
		}
		out[name] = s
	}
	return out
}

// collectMaterializedViews lists the materialized views of a database. They
// are absent from information_schema.tables, so this is the only way.
func collectMaterializedViews(db *sql.DB, database string) ([]browserObject, error) {
	res, err := queryRows(db, sqlMaterializedViews)
	if err != nil {
		return nil, err
	}
	out := []browserObject{}
	for i := range res.rows {
		if !strings.EqualFold(res.value(i, "database_name"), database) {
			continue
		}
		out = append(out, browserObject{
			Name: res.value(i, "name"),
			Type: "MATERIALIZED VIEW",
			Rows: numOrZero(res.value(i, "rows")),
		})
	}
	return out, nil
}

// collectObjectDetail reads the catalog of one object.
func collectObjectDetail(db *sql.DB, database, name string) objectDetailOut {
	out := objectDetailOut{
		Name:       name,
		Columns:    []browserColumn{},
		Partitions: []browserPartition{},
		PKColumns:  []string{},
	}

	// A materialized view is in none of the catalog views: its definition
	// comes from SHOW MATERIALIZED VIEWS and its columns from DESC.
	if ddl, ok := materializedViewDDL(db, database, name); ok {
		out.Type = "MATERIALIZED VIEW"
		out.DDL = ddl
		cols, err := queryRows(db, "DESC "+quoteIdent(name))
		if err != nil {
			out.Error = err.Error()
			return out
		}
		for i := range cols.rows {
			c := browserColumn{
				Name:     cols.value(i, "Field"),
				Type:     cols.value(i, "Type"),
				Nullable: strings.EqualFold(cols.value(i, "Null"), "YES"),
				Key:      cols.value(i, "Key"),
				Default:  cols.value(i, "Default"),
			}
			out.Columns = append(out.Columns, c)
		}
		return out
	}

	// The catalog of the object: the model, the keys, the distribution.
	res, err := queryRowsArgs(db, sqlObjectConfig, database, name)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	if len(res.rows) == 0 {
		out.Error = "no such object: " + name
		return out
	} else {
		out.Type = res.value(0, "TABLE_TYPE")
		out.Model = res.value(0, "TABLE_MODEL")
		out.PrimaryKey = res.value(0, "PRIMARY_KEY")
		out.PartitionKey = res.value(0, "PARTITION_KEY")
		out.DistributeKey = res.value(0, "DISTRIBUTE_KEY")
		out.DistributeType = res.value(0, "DISTRIBUTE_TYPE")
		out.DistributeBucket = int(numOrZero(res.value(0, "DISTRIBUTE_BUCKET")))
		out.SortKey = res.value(0, "SORT_KEY")
		out.Properties = res.value(0, "PROPERTIES")
	}

	cols, err := queryRowsArgs(db, sqlObjectColumns, database, name)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	for i := range cols.rows {
		c := browserColumn{
			Name:     cols.value(i, "COLUMN_NAME"),
			Type:     cols.value(i, "COLUMN_TYPE"),
			Nullable: strings.EqualFold(cols.value(i, "IS_NULLABLE"), "YES"),
			Key:      cols.value(i, "COLUMN_KEY"),
			Default:  cols.value(i, "COLUMN_DEFAULT"),
			Comment:  cols.value(i, "COLUMN_COMMENT"),
		}
		out.Columns = append(out.Columns, c)
		if c.Key == "PRI" {
			out.PKColumns = append(out.PKColumns, c.Name)
		}
	}

	parts, err := queryRowsArgs(db, sqlObjectPartitions, database, name)
	if err == nil {
		for i := range parts.rows {
			p := browserPartition{
				Name:            parts.value(i, "PARTITION_NAME"),
				Ordinal:         int(numOrZero(parts.value(i, "PARTITION_ID"))),
				PartitionKey:    parts.value(i, "PARTITION_KEY"),
				PartitionValue:  parts.value(i, "PARTITION_VALUE"),
				DistributionKey: parts.value(i, "DISTRIBUTION_KEY"),
				Buckets:         int(numOrZero(parts.value(i, "BUCKETS"))),
				Replication:     int(numOrZero(parts.value(i, "REPLICATION_NUM"))),
				Rows:            numOrZero(parts.value(i, "ROW_COUNT")),
			}
			if v, ok := humanBytes(parts.value(i, "DATA_SIZE")); ok {
				p.Size = v
			}
			out.Partitions = append(out.Partitions, p)
		}
	}

	ddl, err := objectDDL(db, name, out.Type)
	if err == nil {
		out.DDL = ddl
	}

	// Only a primary key table accepts an UPDATE. A unique key table
	// rejects it as firmly as a duplicate key one, which is not what the
	// name suggests.
	out.Editable = out.Model == "PRIMARY_KEYS" && len(out.PKColumns) > 0
	return out
}

// materializedViewDDL reads the definition of a materialized view, and says
// whether the name is one at all.
//
// A materialized view is absent from information_schema.tables and from
// information_schema.columns, so SHOW MATERIALIZED VIEWS is the only place
// its definition is written down.
func materializedViewDDL(db *sql.DB, database, name string) (string, bool) {
	res, err := queryRows(db, sqlMaterializedViews)
	if err != nil {
		return "", false
	}
	for i := range res.rows {
		if !strings.EqualFold(res.value(i, "database_name"), database) {
			continue
		}
		if !strings.EqualFold(res.value(i, "name"), name) {
			continue
		}
		return res.value(i, "text"), true
	}
	return "", false
}

// objectDDL reads the statement that creates an object.
func objectDDL(db *sql.DB, name, objType string) (string, error) {
	stmt := "SHOW CREATE TABLE " + quoteIdent(name)
	if strings.EqualFold(objType, "VIEW") {
		stmt = "SHOW CREATE VIEW " + quoteIdent(name)
	}
	res, err := queryRows(db, stmt)
	if err != nil {
		return "", err
	}
	if len(res.rows) == 0 {
		return "", nil
	}
	// The second column carries the statement; the first is the name.
	for _, col := range res.cols {
		if strings.EqualFold(col, "Create Table") || strings.EqualFold(col, "Create View") {
			return res.value(0, col), nil
		}
	}
	return res.value(0, res.cols[len(res.cols)-1]), nil
}

// quoteIdent quotes a StarRocks identifier.
func quoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// The catalog queries. The database and the object are parameters, never
// interpolated: they come from the connection and from the page.
const (
	sqlObjectList = `SELECT t.table_name, t.table_type, c.TABLE_MODEL, c.PRIMARY_KEY,
	                      c.PARTITION_KEY, c.DISTRIBUTE_KEY, c.DISTRIBUTE_TYPE,
	                      c.DISTRIBUTE_BUCKET, c.SORT_KEY, c.TABLE_ID
	                 FROM information_schema.tables t
	                 LEFT JOIN information_schema.tables_config c
	                   ON c.TABLE_SCHEMA = t.table_schema AND c.TABLE_NAME = t.table_name
	                 WHERE t.table_schema = ?`

	sqlObjectConfig = `SELECT t.table_type, c.TABLE_MODEL, c.PRIMARY_KEY, c.PARTITION_KEY,
	                       c.DISTRIBUTE_KEY, c.DISTRIBUTE_TYPE, c.DISTRIBUTE_BUCKET,
	                       c.SORT_KEY, c.PROPERTIES
	                  FROM information_schema.tables t
	                  JOIN information_schema.tables_config c
	                    ON c.TABLE_SCHEMA = t.table_schema AND c.TABLE_NAME = t.table_name
	                  WHERE t.table_schema = ? AND t.table_name = ?`

	sqlObjectColumns = `SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COLUMN_KEY,
	                         COLUMN_DEFAULT, COLUMN_COMMENT
	                    FROM information_schema.columns
	                    WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
	                    ORDER BY ORDINAL_POSITION`

	sqlObjectPartitions = `SELECT PARTITION_NAME, PARTITION_ID, PARTITION_KEY,
	                              PARTITION_VALUE, DISTRIBUTION_KEY, BUCKETS,
	                              REPLICATION_NUM, DATA_SIZE, ROW_COUNT
	                         FROM information_schema.partitions_meta
	                         WHERE DB_NAME = ? AND TABLE_NAME = ?
	                         ORDER BY PARTITION_ID`

	sqlPartitionMeta = `SELECT TABLE_NAME, DATA_SIZE, ROW_COUNT
	                        FROM information_schema.partitions_meta
	                        WHERE DB_NAME = ?`

	sqlMaterializedViews = "SHOW MATERIALIZED VIEWS"
)

// scanResult reads a result set that was queried with parameters.
func scanResult(rows *sql.Rows) (*result, error) {
	cols, err := rows.Columns()
	if err != nil {
		_ = rows.Close()
		return nil, err
	}
	out := &result{cols: cols}
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out.rows = append(out.rows, vals)
	}
	err = rows.Err()
	_ = rows.Close()
	return out, err
}

// collectBrowserTreeJSON opens a connection, reads the tree, and closes it.
func collectBrowserTreeJSON(_ context.Context, dsn string) (browserTreeOut, error) {
	db, err := openSR(dsn)
	if err != nil {
		return browserTreeOut{Error: err.Error()}, err
	}
	defer func() { _ = db.Close() }()
	return collectBrowserTree(db), nil
}

// collectObjectDetailJSON opens a connection, reads one object, and closes
// it.
func collectObjectDetailJSON(_ context.Context, dsn, name string) (objectDetailOut, error) {
	db, err := openSR(dsn)
	if err != nil {
		return objectDetailOut{Error: err.Error()}, err
	}
	defer func() { _ = db.Close() }()
	return collectObjectDetail(db, browserDatabase(db), name), nil
}
