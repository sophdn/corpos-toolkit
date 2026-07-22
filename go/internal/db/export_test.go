package db

// SplitSQLStatementsForTest exposes the internal SQL statement splitter
// to migrate_test.go without making it part of the public package API.
// The splitter is load-bearing for trigger BEGIN..END handling; the
// migrate_test exercises the same edge cases the Rust source-of-truth
// covers in crates/shared-db/src/lib.rs.
func SplitSQLStatementsForTest(sql string) []string { return splitSQLStatements(sql) }
