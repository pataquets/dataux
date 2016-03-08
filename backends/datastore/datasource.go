package datastore

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	u "github.com/araddon/gou"
	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/jwt"
	"google.golang.org/cloud"
	"google.golang.org/cloud/datastore"

	"github.com/araddon/qlbridge/datasource"
	"github.com/araddon/qlbridge/rel"
	"github.com/araddon/qlbridge/schema"
	"github.com/araddon/qlbridge/value"
)

const (
	DataSourceLabel = "google-datastore"
)

var (
	GoogleJwt     *string = flag.String("googlejwt", os.Getenv("GOOGLEJWT"), "Path to google JWT oauth token file")
	GoogleProject *string = flag.String("googleproject", os.Getenv("GOOGLEPROJECT"), "Google Datastore Project Id")

	ErrNoSchema = fmt.Errorf("No schema or configuration exists")

	// Ensure our Google DataStore implements datasource.DataSource interface
	_ schema.DataSource = (*GoogleDSDataSource)(nil)
)

func init() {
	// We need to register our DataSource provider here
	datasource.Register(DataSourceLabel, &GoogleDSDataSource{})
}

// Google Datastore Data Source, is a singleton, non-threadsafe connection
//  to a backend mongo server
type GoogleDSDataSource struct {
	db             string
	namespace      string
	databases      []string
	tablesLower    []string          // Lower cased
	tablesOriginal map[string]string // google is case sensitive  map[lower]Original
	cloudProjectId string
	jwtFile        string
	authConfig     *jwt.Config
	dsCtx          context.Context
	dsClient       *datastore.Client
	conf           *schema.SourceConfig
	schema         *schema.SourceSchema
	mu             sync.Mutex
	closed         bool
}

type DatastoreMutator struct {
	tbl *schema.Table
	sql rel.SqlStatement
	ds  *GoogleDSDataSource
}

func (m *GoogleDSDataSource) Setup(ss *schema.SourceSchema) error {

	if m.schema != nil {
		return nil
	}

	m.schema = ss
	m.conf = ss.Conf
	m.db = strings.ToLower(ss.Name)

	m.cloudProjectId = *GoogleProject
	m.jwtFile = *GoogleJwt

	//u.Infof("Init:  %#v", m.schema.Conf)
	if m.schema.Conf == nil {
		return fmt.Errorf("Schema conf not found")
	}
	jh := u.JsonHelper(conf.Settings)
	if pid := jh.String("projectid"); pid != "" {
		m.cloudProjectId = pid
	}
	if jwt := jh.String("jwt"); jwt != "" {
		m.jwtFile = jwt
	}

	// This will return an error if the database name we are using is not found
	if err := m.connect(); err != nil {
		return err
	}

	return m.loadSchema()
}

func (m *GoogleDSDataSource) loadSchema() error {

	// Load a list of projects?  Namespaces?
	// if err := m.loadNamespaces(); err != nil {
	// 	u.Errorf("could not load google datastore namespace: %v", err)
	// 	return err
	// }

	if err := m.loadTableNames(); err != nil {
		u.Errorf("could not load google datastore kinds: %v", err)
		return err
	}
	return nil
}

func (m *GoogleDSDataSource) Close() error {
	u.Infof("Closing GoogleDSDataSource %p", m)
	m.mu.Lock()
	defer m.mu.Unlock()

	m.closed = true
	return nil
}

func (m *GoogleDSDataSource) connect() error {

	//u.Infof("connecting GoogleDSDataSource: host='%s'  conf=%#v", host, m.schema.Conf)
	m.mu.Lock()
	defer m.mu.Unlock()

	jsonKey, err := ioutil.ReadFile(m.jwtFile)
	if err != nil {
		u.Errorf("Could not open Google Auth Token JWT file %v", err)
		return err
	}

	conf, err := google.JWTConfigFromJSON(
		jsonKey,
		datastore.ScopeDatastore,
		datastore.ScopeUserEmail,
	)
	if err != nil {
		u.Errorf("could not use google datastore JWT token: %v", err)
		return err
	}
	m.authConfig = conf

	ctx := context.Background()
	client, err := datastore.NewClient(ctx, m.cloudProjectId, cloud.WithTokenSource(conf.TokenSource(ctx)))
	if err != nil {
		u.Errorf("could not create google datastore client: project:%s jwt:%s  err=%v", m.cloudProjectId, m.jwtFile, err)
		return err
	}
	m.dsClient = client
	m.dsCtx = ctx
	return nil
}

func (m *GoogleDSDataSource) DataSource() schema.DataSource {
	return m
}
func (m *GoogleDSDataSource) Tables() []string {
	return m.schema.Tables()
}

func (m *GoogleDSDataSource) Open(tableName string) (schema.SourceConn, error) {
	u.Debugf("Open(%v)", tableName)
	if m.schema == nil {
		u.Warnf("no schema?")
		return nil, nil
	}
	tableName = strings.ToLower(tableName)
	tbl, err := m.schema.Table(tableName)
	if err != nil {
		return nil, err
	}
	if tbl == nil {
		u.Errorf("Could not find table for '%s'.'%s'", m.schema.Name, tableName)
		return nil, fmt.Errorf("Could not find '%v'.'%v' schema", m.schema.Name, tableName)
	}

	gdsSource := NewSqlToDatstore(tbl, m.dsClient, m.dsCtx)
	return gdsSource, nil
}

func (m *GoogleDSDataSource) selectQuery(stmt *rel.SqlSelect) (*ResultReader, error) {

	//u.Debugf("get sourceTask for %v", stmt)
	tblName := strings.ToLower(stmt.From[0].Name)

	tbl, err := m.schema.Table(tblName)
	if err != nil {
		return nil, err
	}
	if tbl == nil {
		u.Errorf("Could not find table for '%s'.'%s'", m.schema.Name, tblName)
		return nil, fmt.Errorf("Could not find '%v'.'%v' schema", m.schema.Name, tblName)
	}

	sqlDs := NewSqlToDatstore(tbl, m.dsClient, m.dsCtx)
	//u.Debugf("SqlToDatstore: %#v", sqlDs)
	resp, err := sqlDs.Query(stmt)
	if err != nil {
		u.Errorf("Google datastore query interpreter failed: %v", err)
		return nil, err
	}
	return resp, nil
}

func (m *GoogleDSDataSource) Table(table string) (*schema.Table, error) {
	//u.Debugf("get table for %s", table)
	return m.loadTableSchema(table, "")
}

func (m *GoogleDSDataSource) loadDatabases() error {

	dbs := make([]string, 0)
	sort.Strings(dbs)
	m.databases = dbs
	u.Debugf("found database names: %v", m.databases)
	found := false
	for _, db := range dbs {
		if strings.ToLower(db) == strings.ToLower(m.schema.Name) {
			found = true
		}
	}
	if !found {
		u.Warnf("could not find database: %v", m.schema.Name)
		return fmt.Errorf("Could not find that database: %v", m.schema.Name)
	}

	return nil
}

// Load only table/collection names, not full schema
func (m *GoogleDSDataSource) loadTableNames() error {

	tablesLower := make([]string, 0)
	tablesOriginal := make(map[string]string)
	rows := pageQuery(m.dsClient.Run(m.dsCtx, datastore.NewQuery("__kind__")))
	for _, row := range rows {
		if !strings.HasPrefix(row.key.Name(), "__") {
			tableLower := strings.ToLower(row.key.Name())
			//u.Debugf("found table %q  %#v", tableLower, row.key)
			tablesLower = append(tablesLower, tableLower)
			tablesOriginal[tableLower] = row.key.Name()
			m.loadTableSchema(tableLower, row.key.Name())
		}
	}
	m.tablesLower = tablesLower
	m.tablesOriginal = tablesOriginal
	return nil
}

func (m *GoogleDSDataSource) loadTableSchema(tableLower, tableOriginal string) (*schema.Table, error) {

	if tableOriginal == "" {
		name, ok := m.tablesOriginal[tableLower]
		if !ok {
			m.loadTableNames()
			if name, ok = m.tablesOriginal[tableLower]; !ok {
				return nil, fmt.Errorf("That table %q not found", tableLower)
			}
		}
		tableOriginal = name
	}

	if m.schema == nil {
		return nil, fmt.Errorf("no schema in use")
	}

	tbl, _ := m.schema.Table(tableLower)
	if tbl != nil {
		return tbl, nil
	}

	/*
		- Datastore keeps list of all indexed properties available
		- then we will need to ?? sample some others?
		TODO:
			- Need to recurse through enough records to get good idea of types
	*/
	u.Debugf("loadTableSchema lower:%q original:%q", tableLower, tableOriginal)
	tbl = schema.NewTable(tableOriginal, m.schema)
	colNames := make([]string, 0)

	// We are going to scan this table, introspecting a few rows
	// to see what types they might be
	props := pageQuery(m.dsClient.Run(m.dsCtx, datastore.NewQuery(tableOriginal).Limit(20)))
	for _, row := range props {

		for i, p := range row.props {
			//u.Warnf("%#v ", p)
			colName := strings.ToLower(p.Name)

			if tbl.HasField(colName) {
				continue
			}
			u.Debugf("%d found col: %s %T=%v", i, colName, p.Value, p.Value)
			switch val := p.Value.(type) {
			case *datastore.Key:
				//u.Debugf("found datastore.Key: %v='%#v'", colName, val)
				tbl.AddField(schema.NewFieldBase(p.Name, value.StringType, 24, "Key"))
				//tbl.AddValues([]driver.Value{p.Name, "string", "NO", "PRI", "Key", ""})
			case string:
				//u.Debugf("found property.Value string: %v='%#v'", colName, val)
				tbl.AddField(schema.NewFieldBase(colName, value.StringType, 32, "string"))
				//tbl.AddValues([]driver.Value{colName, "string", "NO", "", "", "string"})
			case int:
				//u.Debugf("found int: %v='%v'", colName, val)
				tbl.AddField(schema.NewFieldBase(colName, value.IntType, 32, "int"))
				//tbl.AddValues([]driver.Value{colName, "int", "NO", "", "", "int"})
			case int64:
				//u.Debugf("found int64: %v='%v'", colName, val)
				tbl.AddField(schema.NewFieldBase(colName, value.IntType, 32, "long"))
				//tbl.AddValues([]driver.Value{colName, "long", "NO", "", "", "long"})
			case float64:
				//u.Debugf("found float64: %v='%v'", colName, val)
				tbl.AddField(schema.NewFieldBase(colName, value.NumberType, 32, "float64"))
				//tbl.AddValues([]driver.Value{colName, "float64", "NO", "", "", "float64"})
			case bool:
				//u.Debugf("found string: %v='%v'", colName, val)
				tbl.AddField(schema.NewFieldBase(colName, value.BoolType, 1, "bool"))
				//tbl.AddValues([]driver.Value{colName, "bool", "NO", "", "", "bool"})
			case time.Time:
				//u.Debugf("found time.Time: %v='%v'", colName, val)
				tbl.AddField(schema.NewFieldBase(colName, value.TimeType, 32, "datetime"))
				//tbl.AddValues([]driver.Value{colName, "datetime", "NO", "", "", "datetime"})
			// case *time.Time: // datastore doesn't allow pointers
			// 	//u.Debugf("found time.Time: %v='%v'", colName, val)
			// 	tbl.AddField(schema.NewFieldBase(colName, value.TimeType, 32, "datetime"))
			// 	tbl.AddValues([]driver.Value{colName, "datetime", "NO", "", "", "datetime"})
			case []uint8:
				tbl.AddField(schema.NewFieldBase(colName, value.ByteSliceType, 256, "[]byte"))
				//tbl.AddValues([]driver.Value{colName, "binary", "NO", "", "", "[]byte"})
			default:
				u.Warnf("gds unknown type %T  %#v", val, p)
			}
			colNames = append(colNames, colName)
		}
	}
	//if len(tbl.FieldMap) > 0 {

	u.Infof("caching schema:%p   %q  cols=%v", m.schema, tableOriginal, colNames)
	m.schema.AddTable(tbl)
	tbl.SetColumns(colNames)
	return tbl, nil
	//}

	//return nil, fmt.Errorf("not found")
}

func titleCase(table string) string {
	table = strings.ToLower(table)
	return strings.ToUpper(table[0:1]) + table[1:]
}

func discoverType(iVal interface{}) value.ValueType {

	switch iVal.(type) {
	case map[string]interface{}:
		return value.MapValueType
	case int:
		return value.IntType
	case int64:
		return value.IntType
	case float64:
		return value.NumberType
	case string:
		return value.StringType
	case time.Time:
		return value.TimeType
	case *time.Time:
		return value.TimeType
	case []uint8:
		return value.ByteSliceType
	case []string:
		return value.StringsType
	case []interface{}:
		return value.SliceValueType
	default:
		u.Warnf("not recognized type:  %T %#v", iVal, iVal)
	}
	return value.NilType
}

func pageQuery(iter *datastore.Iterator) []schemaType {
	rows := make([]schemaType, 0)
	for {
		row := schemaType{}
		if key, err := iter.Next(&row); err != nil {
			if err == datastore.Done {
				break
			}
			u.Errorf("error: %v", err)
			break
		} else {
			row.key = key
			//u.Debugf("key:  %#v", key)
			rows = append(rows, row)
		}
	}
	return rows
}

type schemaType struct {
	Vals  map[string]interface{}
	props []datastore.Property
	key   *datastore.Key
}

func (m *schemaType) Load(props []datastore.Property) error {
	m.Vals = make(map[string]interface{}, len(props))
	m.props = props
	for _, p := range props {
		m.Vals[p.Name] = p.Value
	}
	return nil
}
func (m *schemaType) Save() ([]datastore.Property, error) {
	return nil, nil
}