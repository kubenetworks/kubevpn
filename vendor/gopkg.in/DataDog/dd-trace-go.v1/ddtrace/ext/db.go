// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package ext

const (
	// DBApplication indicates the application using the database.
	DBApplication = "db.application"
	// DBName indicates the database name.
	DBName = "db.name"
	// DBType indicates the type of Database.
	DBType = "db.type"
	// DBInstance indicates the instance name of Database.
	DBInstance = "db.instance"
	// DBUser indicates the user name of Database, e.g. "readonly_user" or "reporting_user".
	DBUser = "db.user"
	// DBStatement records a database statement for the given database type.
	DBStatement = "db.statement"
	// DBSystem indicates the database management system (DBMS) product being used.
	DBSystem = "db.system"
)

// Available values for db.system.
const (
	DBSystemMemcached          = "memcached"
	DBSystemMySQL              = "mysql"
	DBSystemPostgreSQL         = "postgresql"
	DBSystemMicrosoftSQLServer = "mssql"
	// DBSystemOtherSQL is used for other SQL databases not listed above.
	DBSystemOtherSQL      = "other_sql"
	DBSystemElasticsearch = "elasticsearch"
	DBSystemRedis         = "redis"
	DBSystemMongoDB       = "mongodb"
	DBSystemCassandra     = "cassandra"
	DBSystemConsulKV      = "consul"
	DBSystemLevelDB       = "leveldb"
	DBSystemBuntDB        = "buntdb"
)

// MicrosoftSQLServer tags.
const (
	// MicrosoftSQLServerInstanceName indicates the Microsoft SQL Server instance name connecting to.
	MicrosoftSQLServerInstanceName = "db.mssql.instance_name"
)

// MongoDB tags.
const (
	// MongoDBCollection indicates the collection being accessed.
	MongoDBCollection = "db.mongodb.collection"
)

// Redis tags.
const (
	// RedisDatabaseIndex indicates the Redis database index connected to.
	RedisDatabaseIndex = "db.redis.database_index"
)

// Cassandra tags.
const (
	// CassandraQuery is the tag name used for cassandra queries.
	// Deprecated: this value is no longer used internally and will be removed in future versions.
	CassandraQuery = "cassandra.query"

	// CassandraBatch is the tag name used for cassandra batches.
	// Deprecated: this value is no longer used internally and will be removed in future versions.
	CassandraBatch = "cassandra.batch"

	// CassandraConsistencyLevel is the tag name to set for consitency level.
	CassandraConsistencyLevel = "cassandra.consistency_level"

	// CassandraCluster specifies the tag name that is used to set the cluster.
	CassandraCluster = "cassandra.cluster"

	// CassandraRowCount specifies the tag name to use when settings the row count.
	CassandraRowCount = "cassandra.row_count"

	// CassandraKeyspace is used as tag name for setting the key space.
	CassandraKeyspace = "cassandra.keyspace"

	// CassandraPaginated specifies the tag name for paginated queries.
	CassandraPaginated = "cassandra.paginated"

	// CassandraContactPoints holds the list of cassandra initial seed nodes used to discover the cluster.
	CassandraContactPoints = "db.cassandra.contact.points"
)
