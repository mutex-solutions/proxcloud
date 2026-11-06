package dbaas

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// parameters.go — the customer-tunable runtime settings surface, the managed-database
// equivalent of an RDS parameter group or a Trove configuration.
//
// TWO THINGS MAKE THIS AN ALLOWLIST RATHER THAN A PASSTHROUGH, and both are load-bearing:
//
//  1. Every operator owns some of its engine's configuration. CloudNativePG BLOCKS a long list
//     of GUCs at its validating webhook (archive_command, wal_level, ssl_*, listen_addresses…)
//     and rejects the whole Cluster if one appears — which, under ArgoCD, means the Application
//     goes SyncFailed and every LATER change to that database is blocked too. Percona and
//     mariadb do not validate: a setting that breaks group replication or binlog shipping is
//     accepted and silently destroys replication instead.
//  2. The customer's string reaches a config file verbatim. `Validate` therefore refuses
//     newlines, quotes and comment characters — a value like "1\n[mysqld]\nlog_bin=OFF" would
//     otherwise inject a whole section.
//
// WHO RESTARTS: not proxcloud. Every pinned operator applies a change itself and decides whether
// it needs a restart — CNPG reloads a sighup GUC in place and performs a rolling restart for a
// postmaster one; Percona and mariadb hash the config and roll the StatefulSet; Strimzi diffs
// the broker config and applies dynamically-updatable keys through the Admin API without any
// restart at all. `NeedsRestart` below exists to TELL the customer what will happen, not to
// make it happen.

// Param is one tunable setting offered to customers.
type Param struct {
	Name string
	// Kind drives client-side validation and the input widget: int | bytes | duration | enum | bool | string.
	Kind string
	// Min/Max bound numeric kinds ("" = unbounded). Bytes accept a unit suffix.
	Min, Max string
	// Enum lists the accepted values for Kind == "enum".
	Enum []string
	// Restart is true when applying the setting rolls the database's pods.
	Restart bool
	// Help is the one-line explanation the UI shows.
	Help string
}

// ParamBlockFor is the values key whose `.parameters` map an engine's settings live under —
// the chart reads `<block>.parameters` in that engine's template. Empty = no tunables.
func ParamBlockFor(engine string) string {
	switch engine {
	case EnginePostgreSQL, EngineMySQL, EngineMariaDB, EngineValkey, EngineOpenSearch, EngineKafka:
		return engine
	}
	return ""
}

// ParamsFor returns the tunables offered for an engine, or nil when it has none.
func ParamsFor(engine string) []Param { return engineParams[engine] }

// paramValueRE rejects anything that could break out of a config file line: newlines, quotes,
// backslashes and the comment characters of both INI and postgres conf.
var paramValueRE = regexp.MustCompile(`^[A-Za-z0-9_.:,%/+-]{1,128}$`)

// ValidateParameters checks a whole desired parameter set against the engine's allowlist.
// Fail here rather than at the operator: on CNPG a rejected Cluster blocks every later change
// to the database, and on the mysql engines an accepted-but-wrong value quietly breaks
// replication.
func ValidateParameters(engine string, params map[string]string) error {
	offered := map[string]Param{}
	for _, p := range ParamsFor(engine) {
		offered[p.Name] = p
	}
	if len(offered) == 0 && len(params) > 0 {
		return fmt.Errorf("parameters: engine %s has no tunable settings", engine)
	}
	for name, raw := range params {
		p, ok := offered[name]
		if !ok {
			return fmt.Errorf("parameters: %q is not tunable on %s", name, engine)
		}
		v := strings.TrimSpace(raw)
		if !paramValueRE.MatchString(v) {
			return fmt.Errorf("parameters: %q has an invalid value", name)
		}
		switch p.Kind {
		case "enum":
			if !contains(p.Enum, v) {
				return fmt.Errorf("parameters: %q must be one of %v", name, p.Enum)
			}
		case "bool":
			if !contains([]string{"on", "off", "ON", "OFF", "true", "false", "0", "1"}, v) {
				return fmt.Errorf("parameters: %q must be a boolean", name)
			}
		case "int", "duration":
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("parameters: %q must be a number", name)
			}
			if err := inRange(name, n, p.Min, p.Max); err != nil {
				return err
			}
		case "bytes":
			n, err := parseBytes(v)
			if err != nil {
				return fmt.Errorf("parameters: %q must be a size like 256MB", name)
			}
			if err := inRange(name, n, p.Min, p.Max); err != nil {
				return err
			}
		}
	}
	return nil
}

// ParametersNeedRestart reports whether applying this set rolls the pods, so the UI can say so
// before the customer commits rather than after their connections drop.
func ParametersNeedRestart(engine string, params map[string]string) bool {
	for _, p := range ParamsFor(engine) {
		if _, set := params[p.Name]; set && p.Restart {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func inRange(name string, n float64, min, max string) error {
	if min != "" {
		lo, err := parseBytes(min)
		if err == nil && n < lo {
			return fmt.Errorf("parameters: %q must be at least %s", name, min)
		}
	}
	if max != "" {
		hi, err := parseBytes(max)
		if err == nil && n > hi {
			return fmt.Errorf("parameters: %q must be at most %s", name, max)
		}
	}
	return nil
}

// parseBytes accepts a bare number or a size with a unit suffix (kB/MB/GB, case-insensitive,
// the spelling postgres and my.cnf both accept).
func parseBytes(v string) (float64, error) {
	s := strings.TrimSpace(v)
	mult := float64(1)
	for suffix, m := range map[string]float64{"kb": 1 << 10, "mb": 1 << 20, "gb": 1 << 30} {
		if len(s) > len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix) {
			mult = m
			s = s[:len(s)-len(suffix)]
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return n * mult, err
}

// engineParams is the offered set per engine. Curated, not exhaustive: each entry is a setting
// customers actually ask for AND that the operator will accept. Everything the operator manages
// itself is deliberately absent — see the file header for what happens otherwise.
var engineParams = map[string][]Param{
	EnginePostgreSQL: {
		{Name: "max_connections", Kind: "int", Min: "10", Max: "5000", Restart: true, Help: "Connection slots. Each costs memory even when idle; use a pooler before raising this far."},
		{Name: "shared_buffers", Kind: "bytes", Min: "16MB", Max: "64GB", Restart: true, Help: "Postgres page cache. Roughly 25% of the database's memory."},
		{Name: "work_mem", Kind: "bytes", Min: "64kB", Max: "512MB", Help: "Memory per sort or hash step — allocated per step per connection, not per query."},
		{Name: "maintenance_work_mem", Kind: "bytes", Min: "64MB", Max: "2GB", Help: "Memory for VACUUM, index builds and restores."},
		{Name: "effective_cache_size", Kind: "bytes", Min: "128MB", Max: "128GB", Help: "Planner hint for how much cache is available. Allocates nothing."},
		{Name: "statement_timeout", Kind: "duration", Min: "0", Max: "3600000", Help: "Abort a statement after this many milliseconds. 0 disables."},
		{Name: "idle_in_transaction_session_timeout", Kind: "duration", Min: "0", Max: "86400000", Help: "Kill sessions idle inside a transaction (ms). The usual cause of bloat and blocked DDL."},
		{Name: "log_min_duration_statement", Kind: "duration", Min: "-1", Max: "86400000", Help: "Log statements slower than this (ms). -1 off, 0 logs everything."},
		{Name: "random_page_cost", Kind: "int", Min: "1", Max: "10", Help: "Planner's cost for random I/O. 1.1 suits SSD-backed storage."},
		{Name: "max_wal_size", Kind: "bytes", Min: "128MB", Max: "64GB", Help: "WAL allowed between checkpoints. Higher trades disk for fewer checkpoints."},
		{Name: "autovacuum_vacuum_scale_factor", Kind: "int", Min: "0", Max: "1", Help: "Fraction of a table that must change before autovacuum runs."},
		{Name: "autovacuum_max_workers", Kind: "int", Min: "1", Max: "16", Restart: true, Help: "Concurrent autovacuum workers."},
	},
	EngineMySQL: {
		{Name: "max_connections", Kind: "int", Min: "10", Max: "5000", Help: "Connection slots."},
		{Name: "wait_timeout", Kind: "int", Min: "10", Max: "31536000", Help: "Seconds an idle non-interactive connection is kept."},
		{Name: "interactive_timeout", Kind: "int", Min: "10", Max: "31536000", Help: "Seconds an idle interactive connection is kept."},
		{Name: "max_allowed_packet", Kind: "bytes", Min: "1MB", Max: "1GB", Help: "Largest single packet or row."},
		{Name: "slow_query_log", Kind: "bool", Help: "Record queries slower than long_query_time."},
		{Name: "long_query_time", Kind: "int", Min: "0", Max: "3600", Help: "Slow-query threshold in seconds."},
		{Name: "sort_buffer_size", Kind: "bytes", Min: "32kB", Max: "64MB", Help: "Per-connection sort buffer."},
		{Name: "join_buffer_size", Kind: "bytes", Min: "128kB", Max: "64MB", Help: "Per-connection buffer for joins without indexes."},
		{Name: "tmp_table_size", Kind: "bytes", Min: "1MB", Max: "1GB", Help: "Largest in-memory temporary table before it spills to disk."},
		{Name: "innodb_lock_wait_timeout", Kind: "int", Min: "1", Max: "3600", Help: "Seconds a transaction waits for a row lock."},
		{Name: "innodb_buffer_pool_size", Kind: "bytes", Min: "128MB", Max: "128GB", Restart: true, Help: "InnoDB cache. Usually 60-70% of the database's memory."},
		{Name: "innodb_log_file_size", Kind: "bytes", Min: "64MB", Max: "16GB", Restart: true, Help: "Redo log size. Larger absorbs write bursts at the cost of recovery time."},
		{Name: "innodb_flush_method", Kind: "enum", Enum: []string{"O_DIRECT", "fsync", "O_DIRECT_NO_FSYNC"}, Restart: true, Help: "How InnoDB writes to disk."},
	},
	EngineMariaDB: {
		{Name: "max_connections", Kind: "int", Min: "10", Max: "5000", Help: "Connection slots."},
		{Name: "wait_timeout", Kind: "int", Min: "10", Max: "31536000", Help: "Seconds an idle connection is kept."},
		{Name: "max_allowed_packet", Kind: "bytes", Min: "1MB", Max: "1GB", Help: "Largest single packet or row."},
		{Name: "max_statement_time", Kind: "int", Min: "0", Max: "86400", Help: "Abort a statement after this many seconds. 0 disables."},
		{Name: "slow_query_log", Kind: "bool", Help: "Record queries slower than long_query_time."},
		{Name: "long_query_time", Kind: "int", Min: "0", Max: "3600", Help: "Slow-query threshold in seconds."},
		{Name: "innodb_lock_wait_timeout", Kind: "int", Min: "1", Max: "3600", Help: "Seconds a transaction waits for a row lock."},
		{Name: "innodb_io_capacity", Kind: "int", Min: "100", Max: "100000", Help: "Expected IOPS of the storage, used to pace background flushing."},
		{Name: "tmp_table_size", Kind: "bytes", Min: "1MB", Max: "1GB", Help: "Largest in-memory temporary table."},
		{Name: "sort_buffer_size", Kind: "bytes", Min: "32kB", Max: "64MB", Help: "Per-connection sort buffer."},
		{Name: "innodb_buffer_pool_size", Kind: "bytes", Min: "128MB", Max: "128GB", Help: "InnoDB cache. Resizable online on MariaDB."},
		{Name: "innodb_log_file_size", Kind: "bytes", Min: "64MB", Max: "16GB", Restart: true, Help: "Redo log size."},
		{Name: "innodb_flush_method", Kind: "enum", Enum: []string{"O_DIRECT", "fsync", "O_DIRECT_NO_FSYNC"}, Restart: true, Help: "How InnoDB writes to disk."},
	},
	EngineValkey: {
		{Name: "maxmemory-policy", Kind: "enum", Enum: []string{"noeviction", "allkeys-lru", "allkeys-lfu", "allkeys-random", "volatile-lru", "volatile-lfu", "volatile-random", "volatile-ttl"}, Help: "What to evict when memory fills. noeviction returns errors on write instead."},
		{Name: "timeout", Kind: "int", Min: "0", Max: "86400", Help: "Close idle client connections after this many seconds. 0 keeps them."},
		{Name: "maxmemory-samples", Kind: "int", Min: "3", Max: "64", Help: "Sampling accuracy of the LRU/LFU approximation."},
	},
	EngineKafka: {
		{Name: "log.retention.ms", Kind: "int", Min: "60000", Max: "31536000000", Help: "How long records are kept, in milliseconds."},
		{Name: "log.retention.bytes", Kind: "int", Min: "-1", Max: "1099511627776", Help: "Size cap per partition. -1 for no size limit."},
		{Name: "log.segment.bytes", Kind: "bytes", Min: "1MB", Max: "1GB", Help: "Segment size; retention deletes whole segments."},
		{Name: "message.max.bytes", Kind: "bytes", Min: "1kB", Max: "64MB", Help: "Largest record the broker accepts."},
		{Name: "compression.type", Kind: "enum", Enum: []string{"producer", "gzip", "snappy", "lz4", "zstd", "uncompressed"}, Help: "Broker-side compression."},
		{Name: "num.replica.fetchers", Kind: "int", Min: "1", Max: "16", Help: "Threads replicating from other brokers."},
		{Name: "num.partitions", Kind: "int", Min: "1", Max: "1000", Restart: true, Help: "Default partitions for auto-created topics."},
		{Name: "auto.create.topics.enable", Kind: "bool", Restart: true, Help: "Create a topic on first use."},
	},
	EngineOpenSearch: {
		{Name: "indices.query.bool.max_clause_count", Kind: "int", Min: "1024", Max: "65536", Restart: true, Help: "Maximum clauses in a boolean query."},
		{Name: "search.max_buckets", Kind: "int", Min: "1000", Max: "1000000", Restart: true, Help: "Maximum aggregation buckets in one response."},
		{Name: "http.max_content_length", Kind: "bytes", Min: "1MB", Max: "2GB", Restart: true, Help: "Largest HTTP request body."},
		{Name: "indices.memory.index_buffer_size", Kind: "string", Restart: true, Help: "Share of heap for indexing buffers, e.g. 10%."},
		{Name: "cluster.max_shards_per_node", Kind: "int", Min: "100", Max: "10000", Restart: true, Help: "Shard cap per node."},
	},
}
