package mysql

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"io/ioutil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	log "github.com/hashicorp/go-hclog"

	"github.com/armon/go-metrics"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/hashicorp/errwrap"
	"github.com/hashicorp/vault/helper/strutil"
	"github.com/hashicorp/vault/physical"
)

// Verify MySQLBackend satisfies the correct interfaces
var _ physical.Backend = (*MySQLBackend)(nil)

// Unreserved tls key
// Reserved values are "true", "false", "skip-verify"
const mysqlTLSKey = "default"

// MySQLBackend is a physical backend that stores data
// within MySQL database.
type MySQLBackend struct {
	dbTable    string
	client     *sql.DB
	statements map[string]*sql.Stmt
	logger     log.Logger
	permitPool *physical.PermitPool
}

// NewMySQLBackend constructs a MySQL backend using the given API client and
// server address and credential for accessing mysql database.
func NewMySQLBackend(conf map[string]string, logger log.Logger) (physical.Backend, error) {
	var err error

	// Get the MySQL credentials to perform read/write operations.
	username, ok := conf["username"]
	if !ok || username == "" {
		return nil, fmt.Errorf("missing username")
	}
	password, ok := conf["password"]
	if !ok || username == "" {
		return nil, fmt.Errorf("missing password")
	}

	// Get or set MySQL server address. Defaults to localhost and default port(3306)
	address, ok := conf["address"]
	if !ok {
		address = "127.0.0.1:3306"
	}

	// Get the MySQL database and table details.
	database, ok := conf["database"]
	if !ok {
		database = "vault"
	}
	table, ok := conf["table"]
	if !ok {
		table = "vault"
	}
	dbTable := database + "." + table

	maxIdleConnStr, ok := conf["max_idle_connections"]
	var maxIdleConnInt int
	if ok {
		maxIdleConnInt, err = strconv.Atoi(maxIdleConnStr)
		if err != nil {
			return nil, errwrap.Wrapf("failed parsing max_idle_connections parameter: {{err}}", err)
		}
		if logger.IsDebug() {
			logger.Debug("max_idle_connections set", "max_idle_connections", maxIdleConnInt)
		}
	}

	maxConnLifeStr, ok := conf["max_connection_lifetime"]
	var maxConnLifeInt int
	if ok {
		maxConnLifeInt, err = strconv.Atoi(maxConnLifeStr)
		if err != nil {
			return nil, errwrap.Wrapf("failed parsing max_connection_lifetime parameter: {{err}}", err)
		}
		if logger.IsDebug() {
			logger.Debug("max_connection_lifetime set", "max_connection_lifetime", maxConnLifeInt)
		}
	}

	maxParStr, ok := conf["max_parallel"]
	var maxParInt int
	if ok {
		maxParInt, err = strconv.Atoi(maxParStr)
		if err != nil {
			return nil, errwrap.Wrapf("failed parsing max_parallel parameter: {{err}}", err)
		}
		if logger.IsDebug() {
			logger.Debug("max_parallel set", "max_parallel", maxParInt)
		}
	} else {
		maxParInt = physical.DefaultParallelOperations
	}

	dsnParams := url.Values{}
	tlsCaFile, ok := conf["tls_ca_file"]
	if ok {
		if err := setupMySQLTLSConfig(tlsCaFile); err != nil {
			return nil, fmt.Errorf("failed register TLS config: %v", err)
		}

		dsnParams.Add("tls", mysqlTLSKey)
	}

	// Create MySQL handle for the database.
	dsn := username + ":" + password + "@tcp(" + address + ")/?" + dsnParams.Encode()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to mysql: %v", err)
	}
	db.SetMaxOpenConns(maxParInt)
	if maxIdleConnInt != 0 {
		db.SetMaxIdleConns(maxIdleConnInt)
	}
	if maxConnLifeInt != 0 {
		db.SetConnMaxLifetime(time.Duration(maxConnLifeInt) * time.Second)
	}
	// Check schema exists
	var schemaExist bool
	schemaRows, err := db.Query("SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", database)
	if err != nil {
		return nil, fmt.Errorf("failed to check mysql schema exist: %v", err)
	}
	defer schemaRows.Close()
	schemaExist = schemaRows.Next()

	// Check table exists
	var tableExist bool
	tableRows, err := db.Query("SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_NAME = ? AND TABLE_SCHEMA = ?", table, database)

	if err != nil {
		return nil, fmt.Errorf("failed to check mysql table exist: %v", err)
	}
	defer tableRows.Close()
	tableExist = tableRows.Next()

	// Create the required database if it doesn't exists.
	if !schemaExist {
		if _, err := db.Exec("CREATE DATABASE IF NOT EXISTS " + database); err != nil {
			return nil, fmt.Errorf("failed to create mysql database: %v", err)
		}
	}

	// Create the required table if it doesn't exists.
	if !tableExist {
		create_query := "CREATE TABLE IF NOT EXISTS " + dbTable +
			" (vault_key varbinary(512), vault_value mediumblob, PRIMARY KEY (vault_key))"
		if _, err := db.Exec(create_query); err != nil {
			return nil, fmt.Errorf("failed to create mysql table: %v", err)
		}
	}

	// Setup the backend.
	m := &MySQLBackend{
		dbTable:    dbTable,
		client:     db,
		statements: make(map[string]*sql.Stmt),
		logger:     logger,
		permitPool: physical.NewPermitPool(maxParInt),
	}

	// Prepare all the statements required
	statements := map[string]string{
		"put": "INSERT INTO " + dbTable +
			" VALUES( ?, ? ) ON DUPLICATE KEY UPDATE vault_value=VALUES(vault_value)",
		"get":    "SELECT vault_value FROM " + dbTable + " WHERE vault_key = ?",
		"delete": "DELETE FROM " + dbTable + " WHERE vault_key = ?",
		"list":   "SELECT vault_key FROM " + dbTable + " WHERE vault_key LIKE ?",
	}
	for name, query := range statements {
		if err := m.prepare(name, query); err != nil {
			return nil, err
		}
	}

	return m, nil
}

// prepare is a helper to prepare a query for future execution
func (m *MySQLBackend) prepare(name, query string) error {
	stmt, err := m.client.Prepare(query)
	if err != nil {
		return fmt.Errorf("failed to prepare '%s': %v", name, err)
	}
	m.statements[name] = stmt
	return nil
}

// Put is used to insert or update an entry.
func (m *MySQLBackend) Put(ctx context.Context, entry *physical.Entry) error {
	defer metrics.MeasureSince([]string{"mysql", "put"}, time.Now())

	m.permitPool.Acquire()
	defer m.permitPool.Release()

	_, err := m.statements["put"].Exec(entry.Key, entry.Value)
	if err != nil {
		return err
	}
	return nil
}

// Get is used to fetch and entry.
func (m *MySQLBackend) Get(ctx context.Context, key string) (*physical.Entry, error) {
	defer metrics.MeasureSince([]string{"mysql", "get"}, time.Now())

	m.permitPool.Acquire()
	defer m.permitPool.Release()

	var result []byte
	err := m.statements["get"].QueryRow(key).Scan(&result)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	ent := &physical.Entry{
		Key:   key,
		Value: result,
	}
	return ent, nil
}

// Delete is used to permanently delete an entry
func (m *MySQLBackend) Delete(ctx context.Context, key string) error {
	defer metrics.MeasureSince([]string{"mysql", "delete"}, time.Now())

	m.permitPool.Acquire()
	defer m.permitPool.Release()

	_, err := m.statements["delete"].Exec(key)
	if err != nil {
		return err
	}
	return nil
}

// List is used to list all the keys under a given
// prefix, up to the next prefix.
func (m *MySQLBackend) List(ctx context.Context, prefix string) ([]string, error) {
	defer metrics.MeasureSince([]string{"mysql", "list"}, time.Now())

	m.permitPool.Acquire()
	defer m.permitPool.Release()

	// Add the % wildcard to the prefix to do the prefix search
	likePrefix := prefix + "%"
	rows, err := m.statements["list"].Query(likePrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to execute statement: %v", err)
	}

	var keys []string
	for rows.Next() {
		var key string
		err = rows.Scan(&key)
		if err != nil {
			return nil, fmt.Errorf("failed to scan rows: %v", err)
		}

		key = strings.TrimPrefix(key, prefix)
		if i := strings.Index(key, "/"); i == -1 {
			// Add objects only from the current 'folder'
			keys = append(keys, key)
		} else if i != -1 {
			// Add truncated 'folder' paths
			keys = strutil.AppendIfMissing(keys, string(key[:i+1]))
		}
	}

	sort.Strings(keys)
	return keys, nil
}

// Establish a TLS connection with a given CA certificate
// Register a tsl.Config associated with the same key as the dns param from sql.Open
// foo:bar@tcp(127.0.0.1:3306)/dbname?tls=default
func setupMySQLTLSConfig(tlsCaFile string) error {
	rootCertPool := x509.NewCertPool()

	pem, err := ioutil.ReadFile(tlsCaFile)
	if err != nil {
		return err
	}

	if ok := rootCertPool.AppendCertsFromPEM(pem); !ok {
		return err
	}

	err = mysql.RegisterTLSConfig(mysqlTLSKey, &tls.Config{
		RootCAs: rootCertPool,
	})
	if err != nil {
		return err
	}

	return nil
}
