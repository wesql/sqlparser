/*
Copyright ApeCloud, Inc.
Licensed under the Apache v2(found in the LICENSE file in the root directory).
*/
package cdc

import (
	"context"
	"flag"
	"fmt"
	"github.com/wesql/sqlparser"
	"github.com/wesql/sqlparser/go/sqltypes"
	binlogdatapb "github.com/wesql/sqlparser/go/vt/proto/binlogdata"
	querypb "github.com/wesql/sqlparser/go/vt/proto/query"
	topodatapb "github.com/wesql/sqlparser/go/vt/proto/topodata"
	vtgatepb "github.com/wesql/sqlparser/go/vt/proto/vtgate"
	"github.com/wesql/sqlparser/go/vt/proto/vtgateservice"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"io"
	"log"
	"strings"
)

type CdcConsumer struct {
	Ctx             context.Context
	VtgateClient    vtgateservice.VitessClient
	VtgateCloseFunc func()

	EventReader vtgateservice.Vitess_VStreamClient

	ColumnInfoMap map[string]*ColumnInfo
}

func NewCdcConsumer() (cc *CdcConsumer) {
	flag.Parse()
	err := checkFlags()
	if err != nil {
		log.Fatalf("error: %v", err)
	}
	return &CdcConsumer{
		Ctx: context.Background(),
	}
}

func (cc *CdcConsumer) Open() {
	// 1. Connect to the vtgate server.
	cc.VtgateClient, cc.VtgateCloseFunc = openWeScaleClient()
	SpiOpen(cc.VtgateClient)
	// 2. Build ColumnInfo Map
	cc.ReloadColInfoMap(DefaultConfig.TableSchema, DefaultConfig.SourceTableName)
}

func (cc *CdcConsumer) Close() {
	cc.VtgateCloseFunc()
	SpiClose(cc.VtgateClient)
}

func (cc *CdcConsumer) Run() {
	// 3. Create a VStream request.
	cc.StartVStream()

	// 4. Read the stream and process the events.
	var fields []*querypb.Field
	var pkFields []*querypb.Field
	var currentGTID string
	var currentPK *querypb.QueryResult
	// todo cdc: consider replace it with a channel
	var resultList []*RowResult
	for {
		resp, err := cc.EventReader.Recv()
		if err == io.EOF {
			fmt.Printf("stream ended\n")
			return
		}
		if err != nil {
			fmt.Printf("error: %v\n", err)
			return
		}
		eventList := resp.Events
		for _, event := range eventList {
			switch event.Type {
			case binlogdatapb.VEventType_FIELD:
				fields = event.FieldEvent.Fields
				pkFields = getPkFields(cc.ColumnInfoMap, fields)
			case binlogdatapb.VEventType_ROW:
				resultList = ProcessRowEvent(event, fields, resultList)
			case binlogdatapb.VEventType_VGTID:
				if len(event.Vgtid.GetShardGtids()) > 0 && event.Vgtid.GetShardGtids()[0].Gtid != "" {
					currentGTID = event.Vgtid.GetShardGtids()[0].Gtid
				}
				if len(event.Vgtid.GetShardGtids()) > 0 && len(event.Vgtid.GetShardGtids()[0].TablePKs) > 0 {
					currentPK = event.Vgtid.GetShardGtids()[0].TablePKs[0].Lastpk
					if currentPK != nil {
						currentPK.Fields = pkFields
					} else {
					}
				}
			case binlogdatapb.VEventType_COMMIT:
				if len(resultList) == 0 {
					continue
				}
				ExecuteBatch(cc.Ctx, cc.VtgateClient, cc.ColumnInfoMap, pkFields, currentGTID, currentPK, resultList)
				// clear the result list
				resultList = make([]*RowResult, 0)
			case binlogdatapb.VEventType_COPY_COMPLETED:
				fmt.Println("copy completed")
			}
		}
	}
}

func ProcessRowEvent(event *binlogdatapb.VEvent, fields []*querypb.Field, resultList []*RowResult) []*RowResult {
	for _, rowChange := range event.RowEvent.RowChanges {
		before := false
		after := false
		if rowChange.Before != nil {
			before = true
		}
		if rowChange.After != nil {
			after = true
		}
		switch {
		case !before && after:
			// insert
			res := sqltypes.CustomProto3ToResult(fields, &querypb.QueryResult{
				Fields: fields,
				Rows: []*querypb.Row{
					rowChange.After,
				},
			})
			resultList = append(resultList, &RowResult{RowType: INSERT, Before: nil, After: res})

		case before && !after:
			// delete
			res := sqltypes.CustomProto3ToResult(fields, &querypb.QueryResult{
				Fields: fields,
				Rows: []*querypb.Row{
					rowChange.Before,
				},
			})
			resultList = append(resultList, &RowResult{RowType: DELETE, Before: res, After: nil})

		case before && after:
			// update
			res1 := sqltypes.CustomProto3ToResult(fields, &querypb.QueryResult{
				Fields: fields,
				Rows: []*querypb.Row{
					rowChange.Before,
				},
			})
			res2 := sqltypes.CustomProto3ToResult(fields, &querypb.QueryResult{
				Fields: fields,
				Rows: []*querypb.Row{
					rowChange.After,
				},
			})
			resultList = append(resultList, &RowResult{RowType: UPDATE, Before: res1, After: res2})

		default:
			panic("unreachable code")
		}
	}
	return resultList
}

func (cc *CdcConsumer) StartVStream() {
	lastGtid, lastPK, err := SpiLoadGTIDAndLastPK(cc.Ctx, cc.VtgateClient)
	if err != nil {
		log.Fatalf("failed to load gtid and lastpk: %v", err)
	}
	vgtid := &binlogdatapb.VGtid{
		ShardGtids: []*binlogdatapb.ShardGtid{{
			Keyspace: DefaultConfig.TableSchema,
			Shard:    "0",
			Gtid:     lastGtid,
			TablePKs: []*binlogdatapb.TableLastPK{{
				TableName: DefaultConfig.SourceTableName,
				Lastpk:    lastPK,
			}},
		}}}
	filter := &binlogdatapb.Filter{
		Rules: []*binlogdatapb.Rule{{
			Match:  DefaultConfig.SourceTableName,
			Filter: DefaultConfig.FilterStatement,
		}},
	}
	flags := &vtgatepb.VStreamFlags{}
	req := &vtgatepb.VStreamRequest{
		TabletType: topodatapb.TabletType_PRIMARY,
		Vgtid:      vgtid,
		Filter:     filter,
		Flags:      flags,
	}
	cc.EventReader, err = cc.VtgateClient.VStream(cc.Ctx, req)
	if err != nil {
		log.Fatalf("failed to create vstream: %v", err)
	}
	fmt.Printf("start streaming\n\n\n\n")
}

func openWeScaleClient() (vtgateservice.VitessClient, func()) {
	conn, err := grpc.Dial(fmt.Sprintf("%s:%s", DefaultConfig.WeScaleHost, DefaultConfig.WeScaleGrpcPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Fatalf("failed to connect to vtgate: %v", err)
	}
	client := vtgateservice.NewVitessClient(conn)
	closeFunc := func() {
		conn.Close()
	}
	return client, closeFunc
}

func generateInsertParsedQuery(tableSchema, tableName string, result *sqltypes.Result) *sqlparser.ParsedQuery {
	fieldNameList := make([]string, 0)
	vals := make([]string, 0)
	vars := make([]any, 0)
	for _, field := range result.Fields {
		fieldNameList = append(fieldNameList, field.Name)
		vars = append(vars, sqlparser.String(sqlparser.NewArgument(field.Name)))
		vals = append(vals, "%a")
	}
	queryTemplate := fmt.Sprintf("insert into %s.%s (%s) values (%s)", tableSchema, tableName, strings.Join(fieldNameList, ","), strings.Join(vals, ","))
	return sqlparser.BuildParsedQuery(queryTemplate, vars...)
}

func generateInsertQueryBindVariables(result *sqltypes.Result) map[string]*querypb.BindVariable {
	bindVars := make(map[string]*querypb.BindVariable)
	for _, namedValues := range result.Named().Rows {
		for colName, value := range namedValues {
			bindVars[colName] = sqltypes.ValueBindVariable(value)
		}
	}
	return bindVars
}

func getCharsetAndCollation(columnName string, colInfoMap map[string]*ColumnInfo) (string, string) {
	if colInfo, ok := colInfoMap[columnName]; ok {
		return colInfo.CharSet, colInfo.Collation
	}
	return "", ""
}

func generatePKConstraint(pkFields []*querypb.Field, colInfoMap map[string]*ColumnInfo) string {
	buf := sqlparser.NewTrackedBuffer(nil)
	type charSetCollation struct {
		charSet   string
		collation string
	}
	var charSetCollations []*charSetCollation
	separator := "("
	for _, pkname := range pkFields {
		charSet, collation := getCharsetAndCollation(pkname.Name, colInfoMap)
		charSetCollations = append(charSetCollations, &charSetCollation{charSet: charSet, collation: collation})
		buf.Myprintf("%s%s%v%s", separator, charSet, &sqlparser.ColName{Name: sqlparser.NewIdentifierCI(pkname.Name)}, collation)
		separator = ","
	}
	separator = ") = ("
	for i := range pkFields {
		buf.WriteString(separator)
		buf.WriteString(charSetCollations[i].charSet)
		separator = ","
		buf.WriteString("%a")
		buf.WriteString(charSetCollations[i].collation)
	}
	buf.WriteString(")")
	return buf.String()
}

func GenerateInsertSQL(rowResult *RowResult) (string, error) {
	parsedInsert := generateInsertParsedQuery(DefaultConfig.TableSchema, DefaultConfig.TargetTableName, rowResult.After)
	bindVars := generateInsertQueryBindVariables(rowResult.After)
	insertSql, err := parsedInsert.GenerateQuery(bindVars, nil)
	if err != nil {
		return "", err
	}
	return insertSql, nil
}

func generateDeleteParsedQuery(tableSchema, tableName string, pkFields []*querypb.Field, colInfoMap map[string]*ColumnInfo) *sqlparser.ParsedQuery {
	queryTemplate := fmt.Sprintf("delete from %s.%s", tableSchema, tableName)
	vars := make([]any, 0)

	buf := sqlparser.NewTrackedBuffer(nil)
	buf.WriteString(" where ")
	buf.WriteString(generatePKConstraint(pkFields, colInfoMap))
	for _, field := range pkFields {
		vars = append(vars, sqlparser.String(sqlparser.NewArgument(field.Name)))
	}

	queryTemplate = fmt.Sprintf("%s%s", queryTemplate, buf.String())
	return sqlparser.BuildParsedQuery(queryTemplate, vars...)
}

func generateDeleteQueryBindVariables(result *sqltypes.Result, pkFields []*querypb.Field) map[string]*querypb.BindVariable {
	pkMap := make(map[string]bool)
	for _, pkField := range pkFields {
		pkMap[pkField.Name] = true
	}

	bindVars := make(map[string]*querypb.BindVariable)
	for _, namedValues := range result.Named().Rows {
		for colName, value := range namedValues {
			if _, ok := pkMap[colName]; ok {
				bindVars[colName] = sqltypes.ValueBindVariable(value)
			}
		}
	}
	return bindVars
}

func GenerateDeleteSQL(rowResult *RowResult, pkFields []*querypb.Field, colInfoMap map[string]*ColumnInfo) (string, error) {
	parsedDelete := generateDeleteParsedQuery(DefaultConfig.TableSchema, DefaultConfig.TargetTableName, pkFields, colInfoMap)
	bindVars := generateDeleteQueryBindVariables(rowResult.Before, pkFields)
	deleteSQL, err := parsedDelete.GenerateQuery(bindVars, nil)
	if err != nil {
		return "", err
	}
	return deleteSQL, nil
}

func generateUpdateParsedQuery(tableSchema, tableName string, allFields []*querypb.Field, pkFields []*querypb.Field, colInfoMap map[string]*ColumnInfo) *sqlparser.ParsedQuery {
	queryTemplate := fmt.Sprintf("update %s.%s", tableSchema, tableName)
	vars := make([]any, 0)

	buf := sqlparser.NewTrackedBuffer(nil)
	buf.WriteString(" set ")
	separator := ""
	for _, col := range allFields {
		buf.Myprintf("%s%s=", separator, col.Name)
		buf.Myprintf("%s", "%a")
		separator = ","
		vars = append(vars, sqlparser.String(sqlparser.NewArgument(col.Name)))
	}

	separator = ""
	buf.WriteString(" where ")
	buf.WriteString(generatePKConstraint(pkFields, colInfoMap))
	for _, field := range pkFields {
		vars = append(vars, sqlparser.String(sqlparser.NewArgument(field.Name)))
	}

	queryTemplate = fmt.Sprintf("%s%s", queryTemplate, buf.String())
	return sqlparser.BuildParsedQuery(queryTemplate, vars...)
}

func generateUpdateQueryBindVariables(before *sqltypes.Result, after *sqltypes.Result, pkFields []*querypb.Field) map[string]*querypb.BindVariable {
	pkMap := make(map[string]bool)
	for _, pkField := range pkFields {
		pkMap[pkField.Name] = true
	}

	// bind vars of set value part
	bindVars := make(map[string]*querypb.BindVariable)
	for _, namedValues := range after.Named().Rows {
		for colName, value := range namedValues {
			bindVars[colName] = sqltypes.ValueBindVariable(value)
		}
	}

	// bind vars of where part
	for _, namedValues := range before.Named().Rows {
		for colName, value := range namedValues {
			if _, ok := pkMap[colName]; ok {
				bindVars["pk_"+colName] = sqltypes.ValueBindVariable(value)
			}
		}
	}
	return bindVars
}

func GenerateUpdateSQL(rowResult *RowResult, pkFields []*querypb.Field, colInfoMap map[string]*ColumnInfo) (string, error) {
	parsedUpdate := generateUpdateParsedQuery(DefaultConfig.TableSchema, DefaultConfig.TargetTableName, rowResult.Before.Fields, pkFields, colInfoMap)
	bindVars := generateUpdateQueryBindVariables(rowResult.Before, rowResult.After, pkFields)
	updateSQL, err := parsedUpdate.GenerateQuery(bindVars, nil)
	if err != nil {
		return "", err
	}
	return updateSQL, nil
}

func ExecuteBatch(
	ctx context.Context,
	client vtgateservice.VitessClient,
	colInfoMap map[string]*ColumnInfo,
	pkFields []*querypb.Field,
	currentGTID string,
	currentPK *querypb.QueryResult,
	resultList []*RowResult,
) {
	queryList := make([]*querypb.BoundQuery, 0)
	// begin
	queryList = append([]*querypb.BoundQuery{{Sql: "begin"}}, queryList...)
	// store gtid and pk
	err := SpiStoreGtidAndLastPK(currentGTID, currentPK, client)
	if err != nil {
		log.Fatalf("failed to store gtid and lastpk: %v", err)
	}
	// store table data
	err = SpiStoreTableData(resultList, colInfoMap, pkFields, client)
	if err != nil {
		log.Fatalf("failed to store table data: %v", err)
	}
	// commit
	queryList = append(queryList, &querypb.BoundQuery{
		Sql: "commit",
	})
	// todo cdc: make sure it's actually in the same transaction
	r, err := client.ExecuteBatch(ctx, &vtgatepb.ExecuteBatchRequest{Queries: queryList})
	if err != nil {
		log.Fatalf("failed to execute batch: %v", err)
	}
	for i, result := range r.Results {
		if result.Error != nil {
			log.Printf("failed to execute query %d: %v", i, result.Error)
		}
	}
	for _, query := range queryList {
		log.Printf("execute %s\n", query.Sql)
	}
}