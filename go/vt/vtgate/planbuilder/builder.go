/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package planbuilder

import (
	"sort"

	"vitess.io/vitess/go/vt/vtgate/planbuilder/plancontext"

	"vitess.io/vitess/go/sqltypes"
	querypb "vitess.io/vitess/go/vt/proto/query"

	"vitess.io/vitess/go/vt/vterrors"

	"vitess.io/vitess/go/vt/key"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vtgate/engine"
	"vitess.io/vitess/go/vt/vtgate/vindexes"

	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
)

const (
	// V3 is also the default planner
	V3 = querypb.ExecuteOptions_V3
	// Gen4 uses the default Gen4 planner, which is the greedy planner
	Gen4 = querypb.ExecuteOptions_Gen4
	// Gen4GreedyOnly uses only the faster greedy planner
	Gen4GreedyOnly = querypb.ExecuteOptions_Gen4Greedy
	// Gen4Left2Right tries to emulate the V3 planner by only joining plans in the order they are listed in the FROM-clause
	Gen4Left2Right = querypb.ExecuteOptions_Gen4Left2Right
	// Gen4WithFallback first attempts to use the Gen4 planner, and if that fails, uses the V3 planner instead
	Gen4WithFallback = querypb.ExecuteOptions_Gen4WithFallback
	// Gen4CompareV3 executes queries on both Gen4 and V3 to compare their results.
	Gen4CompareV3 = querypb.ExecuteOptions_Gen4CompareV3
)

var (
	plannerVersions = []plancontext.PlannerVersion{V3, Gen4, Gen4GreedyOnly, Gen4Left2Right, Gen4WithFallback, Gen4CompareV3}
)

type truncater interface {
	SetTruncateColumnCount(int)
}

// TestBuilder builds a plan for a query based on the specified vschema.
// This method is only used from tests
func TestBuilder(query string, vschema plancontext.VSchema, keyspace string) (*engine.Plan, error) {
	stmt, reserved, err := sqlparser.Parse2(query)
	if err != nil {
		return nil, err
	}
	result, err := sqlparser.RewriteAST(stmt, keyspace, sqlparser.SQLSelectLimitUnset, "", nil)
	if err != nil {
		return nil, err
	}

	reservedVars := sqlparser.NewReservedVars("vtg", reserved)
	return BuildFromStmt(query, result.AST, reservedVars, vschema, result.BindVarNeeds, true, true)
}

// BuildFromStmt builds a plan based on the AST provided.
func BuildFromStmt(query string, stmt sqlparser.Statement, reservedVars *sqlparser.ReservedVars, vschema plancontext.VSchema, bindVarNeeds *sqlparser.BindVarNeeds, enableOnlineDDL, enableDirectDDL bool) (*engine.Plan, error) {
	instruction, err := createInstructionFor(query, stmt, reservedVars, vschema, enableOnlineDDL, enableDirectDDL)
	if err != nil {
		return nil, err
	}
	plan := &engine.Plan{
		Type:         sqlparser.ASTToStatementType(stmt),
		Original:     query,
		Instructions: instruction,
		BindVarNeeds: bindVarNeeds,
	}
	return plan, nil
}

func getConfiguredPlanner(vschema plancontext.VSchema, v3planner func(string) stmtPlanner, stmt sqlparser.Statement, query string) (stmtPlanner, error) {
	planner, ok := getPlannerFromQuery(stmt)
	if !ok {
		// if the query doesn't specify the planner, we check what the configuration is
		planner = vschema.Planner()
	}
	switch planner {
	case Gen4CompareV3:
		return gen4CompareV3Planner(query), nil
	case Gen4Left2Right, Gen4GreedyOnly:
		return gen4Planner(query, planner), nil
	case Gen4WithFallback:
		fp := &fallbackPlanner{
			primary:  gen4Planner(query, querypb.ExecuteOptions_Gen4),
			fallback: v3planner(query),
		}
		return fp.plan, nil
	case V3:
		return v3planner(query), nil
	default:
		// default is gen4 plan
		return gen4Planner(query, Gen4), nil
	}
}

// getPlannerFromQuery chooses the planner to use based on the query
// The default planner can be overridden using /*vt+ PLANNER=gen4 */
// We will also fall back on the gen4 planner if we encounter outer join,
// since there are known problems with the v3 planner and outer joins
func getPlannerFromQuery(stmt sqlparser.Statement) (version plancontext.PlannerVersion, found bool) {
	version, found = getPlannerFromQueryHint(stmt)
	if found {
		return
	}

	_ = sqlparser.Walk(func(node sqlparser.SQLNode) (kontinue bool, err error) {
		join, ok := node.(*sqlparser.JoinTableExpr)
		if ok {
			if join.Join == sqlparser.LeftJoinType || join.Join == sqlparser.RightJoinType {
				version = querypb.ExecuteOptions_Gen4
				found = true
				return false, nil
			}
		}
		return true, nil
	}, stmt)

	return
}

func getPlannerFromQueryHint(stmt sqlparser.Statement) (plancontext.PlannerVersion, bool) {
	cm, isCom := stmt.(sqlparser.Commented)
	if !isCom {
		return plancontext.PlannerVersion(0), false
	}

	d := cm.GetParsedComments().Directives()
	val, ok := d[sqlparser.DirectiveQueryPlanner]
	if !ok {
		return plancontext.PlannerVersion(0), false
	}
	return plancontext.PlannerNameToVersion(val)
}

func buildRoutePlan(stmt sqlparser.Statement, reservedVars *sqlparser.ReservedVars, vschema plancontext.VSchema, f func(statement sqlparser.Statement, reservedVars *sqlparser.ReservedVars, schema plancontext.VSchema) (engine.Primitive, error)) (engine.Primitive, error) {
	if vschema.Destination() != nil {
		return buildPlanForBypass(stmt, reservedVars, vschema)
	}
	return f(stmt, reservedVars, vschema)
}

type stmtPlanner func(sqlparser.Statement, *sqlparser.ReservedVars, plancontext.VSchema) (engine.Primitive, error)

func createInstructionFor(query string, stmt sqlparser.Statement, reservedVars *sqlparser.ReservedVars, vschema plancontext.VSchema, enableOnlineDDL, enableDirectDDL bool) (engine.Primitive, error) {
	switch stmt := stmt.(type) {
	case *sqlparser.Select:
		configuredPlanner, err := getConfiguredPlanner(vschema, buildSelectPlan, stmt, query)
		if err != nil {
			return nil, err
		}
		return buildRoutePlan(stmt, reservedVars, vschema, configuredPlanner)
	case *sqlparser.Insert:
		return buildRoutePlan(stmt, reservedVars, vschema, buildInsertPlan)
	case *sqlparser.Update:
		configuredPlanner, err := getConfiguredPlanner(vschema, buildUpdatePlan, stmt, query)
		if err != nil {
			return nil, err
		}
		return buildRoutePlan(stmt, reservedVars, vschema, configuredPlanner)
	case *sqlparser.Delete:
		return buildRoutePlan(stmt, reservedVars, vschema, buildDeletePlan)
	case *sqlparser.Union:
		configuredPlanner, err := getConfiguredPlanner(vschema, buildUnionPlan, stmt, query)
		if err != nil {
			return nil, err
		}
		return buildRoutePlan(stmt, reservedVars, vschema, configuredPlanner)
	case sqlparser.DDLStatement:
		return buildGeneralDDLPlan(query, stmt, reservedVars, vschema, enableOnlineDDL, enableDirectDDL)
	case *sqlparser.AlterMigration:
		return buildAlterMigrationPlan(query, vschema, enableOnlineDDL)
	case *sqlparser.RevertMigration:
		return buildRevertMigrationPlan(query, stmt, vschema, enableOnlineDDL)
	case *sqlparser.ShowMigrationLogs:
		return buildShowMigrationLogsPlan(query, vschema, enableOnlineDDL)
	case *sqlparser.ShowThrottledApps:
		return buildShowThrottledAppsPlan(query, vschema)
	case *sqlparser.AlterVschema:
		return buildVSchemaDDLPlan(stmt, vschema)
	case *sqlparser.Use:
		return buildUsePlan(stmt, vschema)
	case sqlparser.Explain:
		return buildExplainPlan(stmt, reservedVars, vschema, enableOnlineDDL, enableDirectDDL)
	case *sqlparser.OtherRead, *sqlparser.OtherAdmin:
		return buildOtherReadAndAdmin(query, vschema)
	case *sqlparser.Set:
		return buildSetPlan(stmt, vschema)
	case *sqlparser.Load:
		return buildLoadPlan(query, vschema)
	case sqlparser.DBDDLStatement:
		return buildRoutePlan(stmt, reservedVars, vschema, buildDBDDLPlan)
	case *sqlparser.SetTransaction:
		return buildRoutePlan(stmt, reservedVars, vschema, buildSetTxPlan)
	case *sqlparser.Begin, *sqlparser.Commit, *sqlparser.Rollback, *sqlparser.Savepoint, *sqlparser.SRollback, *sqlparser.Release:
		// Empty by design. Not executed by a plan
		return nil, nil
	case *sqlparser.Show:
		return buildShowPlan(query, stmt, reservedVars, vschema)
	case *sqlparser.LockTables:
		return buildRoutePlan(stmt, reservedVars, vschema, buildLockPlan)
	case *sqlparser.UnlockTables:
		return buildRoutePlan(stmt, reservedVars, vschema, buildUnlockPlan)
	case *sqlparser.Flush:
		return buildFlushPlan(stmt, vschema)
	case *sqlparser.CallProc:
		return buildCallProcPlan(stmt, vschema)
	case *sqlparser.Stream:
		return buildStreamPlan(stmt, vschema)
	case *sqlparser.VStream:
		return buildVStreamPlan(stmt, vschema)
	case *sqlparser.CommentOnly:
		// There is only a comment in the input.
		// This is essentially a No-op
		return engine.NewRowsPrimitive(nil, nil), nil
	}

	return nil, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "BUG: unexpected statement type: %T", stmt)
}

func buildDBDDLPlan(stmt sqlparser.Statement, _ *sqlparser.ReservedVars, vschema plancontext.VSchema) (engine.Primitive, error) {
	dbDDLstmt := stmt.(sqlparser.DBDDLStatement)
	ksName := dbDDLstmt.GetDatabaseName()
	if ksName == "" {
		ks, err := vschema.DefaultKeyspace()
		if err != nil {
			return nil, err
		}
		ksName = ks.Name
	}
	ksExists := vschema.KeyspaceExists(ksName)

	switch dbDDL := dbDDLstmt.(type) {
	case *sqlparser.DropDatabase:
		if dbDDL.IfExists && !ksExists {
			return engine.NewRowsPrimitive(make([][]sqltypes.Value, 0), make([]*querypb.Field, 0)), nil
		}
		if !ksExists {
			return nil, vterrors.NewErrorf(vtrpcpb.Code_NOT_FOUND, vterrors.DbDropExists, "Can't drop database '%s'; database doesn't exists", ksName)
		}
		return engine.NewDBDDL(ksName, false, queryTimeout(dbDDL.Comments.Directives())), nil
	case *sqlparser.AlterDatabase:
		if !ksExists {
			return nil, vterrors.NewErrorf(vtrpcpb.Code_NOT_FOUND, vterrors.BadDb, "Can't alter database '%s'; unknown database", ksName)
		}
		return nil, vterrors.New(vtrpcpb.Code_UNIMPLEMENTED, "alter database is not supported")
	case *sqlparser.CreateDatabase:
		if dbDDL.IfNotExists && ksExists {
			return engine.NewRowsPrimitive(make([][]sqltypes.Value, 0), make([]*querypb.Field, 0)), nil
		}
		if !dbDDL.IfNotExists && ksExists {
			return nil, vterrors.NewErrorf(vtrpcpb.Code_ALREADY_EXISTS, vterrors.DbCreateExists, "Can't create database '%s'; database exists", ksName)
		}
		return engine.NewDBDDL(ksName, true, queryTimeout(dbDDL.Comments.Directives())), nil
	}
	return nil, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "[BUG] database ddl not recognized: %s", sqlparser.String(dbDDLstmt))
}

func buildSetTxPlan(_ sqlparser.Statement, _ *sqlparser.ReservedVars, _ plancontext.VSchema) (engine.Primitive, error) {
	// TODO: This is a NOP, modeled off of tx_isolation and tx_read_only.
	// It's incredibly dangerous that it's a NOP, this will be fixed when it will be implemented.
	// This is currently the refactor of existing setup.
	return engine.NewRowsPrimitive(nil, nil), nil
}

func buildLoadPlan(query string, vschema plancontext.VSchema) (engine.Primitive, error) {
	keyspace, err := vschema.DefaultKeyspace()
	if err != nil {
		return nil, err
	}

	destination := vschema.Destination()
	if destination == nil {
		if err := vschema.ErrorIfShardedF(keyspace, "LOAD", "LOAD is not supported on sharded database"); err != nil {
			return nil, err
		}
		destination = key.DestinationAnyShard{}
	}

	return &engine.Send{
		Keyspace:          keyspace,
		TargetDestination: destination,
		Query:             query,
		IsDML:             true,
		SingleShardOnly:   true,
	}, nil
}

func buildVSchemaDDLPlan(stmt *sqlparser.AlterVschema, vschema plancontext.VSchema) (engine.Primitive, error) {
	_, keyspace, _, err := vschema.TargetDestination(stmt.Table.Qualifier.String())
	if err != nil {
		return nil, err
	}
	return &engine.AlterVSchema{
		Keyspace:        keyspace,
		AlterVschemaDDL: stmt,
	}, nil
}

func buildFlushPlan(stmt *sqlparser.Flush, vschema plancontext.VSchema) (engine.Primitive, error) {
	if len(stmt.TableNames) == 0 {
		return buildFlushOptions(stmt, vschema)
	}
	return buildFlushTables(stmt, vschema)
}

func buildFlushOptions(stmt *sqlparser.Flush, vschema plancontext.VSchema) (engine.Primitive, error) {
	dest, keyspace, _, err := vschema.TargetDestination("")
	if err != nil {
		return nil, err
	}
	if dest == nil {
		dest = key.DestinationAllShards{}
	}
	return &engine.Send{
		Keyspace:          keyspace,
		TargetDestination: dest,
		Query:             sqlparser.String(stmt),
		IsDML:             false,
		SingleShardOnly:   false,
	}, nil
}

func buildFlushTables(stmt *sqlparser.Flush, vschema plancontext.VSchema) (engine.Primitive, error) {
	type sendDest struct {
		ks   *vindexes.Keyspace
		dest key.Destination
	}

	dest := vschema.Destination()
	if dest == nil {
		dest = key.DestinationAllShards{}
	}

	tablesMap := make(map[sendDest]sqlparser.TableNames)
	var keys []sendDest
	for i, tab := range stmt.TableNames {
		var ksTab *vindexes.Keyspace
		var table *vindexes.Table
		var err error

		table, _, _, _, _, err = vschema.FindTableOrVindex(tab)
		if err != nil {
			return nil, err
		}
		if table == nil {
			return nil, vindexes.NotFoundError{TableName: tab.Name.String()}
		}

		ksTab = table.Keyspace
		stmt.TableNames[i] = sqlparser.TableName{
			Name: table.Name,
		}

		key := sendDest{ksTab, dest}
		tables, isAvail := tablesMap[key]
		if !isAvail {
			keys = append(keys, key)
		}
		tables = append(tables, stmt.TableNames[i]) // = append(tables.TableNames, stmt.TableNames[i])
		tablesMap[key] = tables
	}

	if len(tablesMap) == 1 {
		for sendDest, tables := range tablesMap {
			return &engine.Send{
				Keyspace:          sendDest.ks,
				TargetDestination: sendDest.dest,
				Query:             sqlparser.String(newFlushStmt(stmt, tables)),
			}, nil
		}
	}

	sort.Slice(keys, func(i, j int) bool {
		return keys[i].ks.Name < keys[j].ks.Name
	})

	var sources []engine.Primitive
	for _, sendDest := range keys {
		plan := &engine.Send{
			Keyspace:          sendDest.ks,
			TargetDestination: sendDest.dest,
			Query:             sqlparser.String(newFlushStmt(stmt, tablesMap[sendDest])),
		}
		sources = append(sources, plan)
	}
	return engine.NewConcatenate(sources, nil), nil
}

func newFlushStmt(stmt *sqlparser.Flush, tables sqlparser.TableNames) *sqlparser.Flush {
	return &sqlparser.Flush{
		IsLocal:    stmt.IsLocal,
		TableNames: tables,
		WithLock:   stmt.WithLock,
		ForExport:  stmt.ForExport,
	}
}
