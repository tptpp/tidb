// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package perfschema

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/meta/autoid"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/profile"
	"github.com/pingcap/tidb/util/stmtsummary"
)

const (
	tableNameEventsStatementsSummaryByDigest = "events_statements_summary_by_digest"
	tableNameTiDBProfileCPU                  = "tidb_profile_cpu"
	tableNameTiDBProfileMemory               = "tidb_profile_memory"
	tableNameTiDBProfileMutex                = "tidb_profile_mutex"
	tableNameTiDBProfileAllocs               = "tidb_profile_allocs"
	tableNameTiDBProfileBlock                = "tidb_profile_block"
	tableNameTiDBProfileGoroutines           = "tidb_profile_goroutines"
	tableNameTiKVProfileCPU                  = "tikv_profile_cpu"
	tableNamePDProfileCPU                    = "pd_profile_cpu"
	tableNamePDProfileMemory                 = "pd_profile_memory"
	tableNamePDProfileMutex                  = "pd_profile_mutex"
	tableNamePDProfileAllocs                 = "pd_profile_allocs"
	tableNamePDProfileBlock                  = "pd_profile_block"
	tableNamePDProfileGoroutines             = "pd_profile_goroutines"
)

// perfSchemaTable stands for the fake table all its data is in the memory.
type perfSchemaTable struct {
	infoschema.VirtualTable
	meta *model.TableInfo
	cols []*table.Column
}

var pluginTable = make(map[string]func(autoid.Allocator, *model.TableInfo) (table.Table, error))

// RegisterTable registers a new table into TiDB.
func RegisterTable(tableName, sql string,
	tableFromMeta func(autoid.Allocator, *model.TableInfo) (table.Table, error)) {
	perfSchemaTables = append(perfSchemaTables, sql)
	pluginTable[tableName] = tableFromMeta
}

func tableFromMeta(alloc autoid.Allocator, meta *model.TableInfo) (table.Table, error) {
	if f, ok := pluginTable[meta.Name.L]; ok {
		ret, err := f(alloc, meta)
		return ret, err
	}
	return createPerfSchemaTable(meta), nil
}

// createPerfSchemaTable creates all perfSchemaTables
func createPerfSchemaTable(meta *model.TableInfo) *perfSchemaTable {
	columns := make([]*table.Column, 0, len(meta.Columns))
	for _, colInfo := range meta.Columns {
		col := table.ToColumn(colInfo)
		columns = append(columns, col)
	}
	t := &perfSchemaTable{
		meta: meta,
		cols: columns,
	}
	return t
}

// Cols implements table.Table Type interface.
func (vt *perfSchemaTable) Cols() []*table.Column {
	return vt.cols
}

// WritableCols implements table.Table Type interface.
func (vt *perfSchemaTable) WritableCols() []*table.Column {
	return vt.cols
}

// GetID implements table.Table GetID interface.
func (vt *perfSchemaTable) GetPhysicalID() int64 {
	return vt.meta.ID
}

// Meta implements table.Table Type interface.
func (vt *perfSchemaTable) Meta() *model.TableInfo {
	return vt.meta
}

func (vt *perfSchemaTable) getRows(ctx sessionctx.Context, cols []*table.Column) (fullRows [][]types.Datum, err error) {
	switch vt.meta.Name.O {
	case tableNameEventsStatementsSummaryByDigest:
		fullRows = stmtsummary.StmtSummaryByDigestMap.ToDatum()
	case tableNameTiDBProfileCPU:
		fullRows, err = (&profile.Collector{}).ProfileGraph("cpu")
	case tableNameTiDBProfileMemory:
		fullRows, err = (&profile.Collector{}).ProfileGraph("heap")
	case tableNameTiDBProfileMutex:
		fullRows, err = (&profile.Collector{}).ProfileGraph("mutex")
	case tableNameTiDBProfileAllocs:
		fullRows, err = (&profile.Collector{}).ProfileGraph("allocs")
	case tableNameTiDBProfileBlock:
		fullRows, err = (&profile.Collector{}).ProfileGraph("block")
	case tableNameTiDBProfileGoroutines:
		fullRows, err = (&profile.Collector{}).ProfileGraph("goroutine")
	case tableNameTiKVProfileCPU:
		interval := fmt.Sprintf("%d", profile.CPUProfileInterval/time.Second)
		fullRows, err = dataForRemoteProfile(ctx, "tikv", "/debug/pprof/profile?seconds="+interval, false)
	case tableNamePDProfileCPU:
		interval := fmt.Sprintf("%d", profile.CPUProfileInterval/time.Second)
		fullRows, err = dataForRemoteProfile(ctx, "pd", "/pd/api/v1/debug/pprof/profile?seconds="+interval, false)
	case tableNamePDProfileMemory:
		fullRows, err = dataForRemoteProfile(ctx, "pd", "/pd/api/v1/debug/pprof/heap", false)
	case tableNamePDProfileMutex:
		fullRows, err = dataForRemoteProfile(ctx, "pd", "/pd/api/v1/debug/pprof/mutex", false)
	case tableNamePDProfileAllocs:
		fullRows, err = dataForRemoteProfile(ctx, "pd", "/pd/api/v1/debug/pprof/allocs", false)
	case tableNamePDProfileBlock:
		fullRows, err = dataForRemoteProfile(ctx, "pd", "/pd/api/v1/debug/pprof/block", false)
	case tableNamePDProfileGoroutines:
		fullRows, err = dataForRemoteProfile(ctx, "pd", "/pd/api/v1/debug/pprof/goroutine?debug=2", true)
	}
	if err != nil {
		return
	}
	if len(cols) == len(vt.cols) {
		return
	}
	rows := make([][]types.Datum, len(fullRows))
	for i, fullRow := range fullRows {
		row := make([]types.Datum, len(cols))
		for j, col := range cols {
			row[j] = fullRow[col.Offset]
		}
		rows[i] = row
	}
	return rows, nil
}

// IterRecords implements table.Table IterRecords interface.
func (vt *perfSchemaTable) IterRecords(ctx sessionctx.Context, startKey kv.Key, cols []*table.Column,
	fn table.RecordIterFunc) error {
	if len(startKey) != 0 {
		return table.ErrUnsupportedOp
	}
	rows, err := vt.getRows(ctx, cols)
	if err != nil {
		return err
	}
	for i, row := range rows {
		more, err := fn(int64(i), row, cols)
		if err != nil {
			return err
		}
		if !more {
			break
		}
	}
	return nil
}

func dataForRemoteProfile(ctx sessionctx.Context, nodeType, uri string, isGoroutine bool) ([][]types.Datum, error) {
	var (
		servers []infoschema.ServerInfo
		err     error
	)
	switch nodeType {
	case "tikv":
		servers, err = infoschema.GetTiKVServerInfo(ctx)
	case "pd":
		servers, err = infoschema.GetPDServerInfo(ctx)
	default:
		return nil, errors.Errorf("%s does not support profile remote component", nodeType)
	}
	failpoint.Inject("mockRemoteNodeStatusAddress", func(val failpoint.Value) {
		// The cluster topology is injected by `failpoint` expression and
		// there is no extra checks for it. (let the test fail if the expression invalid)
		if s := val.(string); len(s) > 0 {
			servers = servers[:0]
			for _, server := range strings.Split(s, ";") {
				parts := strings.Split(server, ",")
				if parts[0] != nodeType {
					continue
				}
				servers = append(servers, infoschema.ServerInfo{
					ServerType: parts[0],
					Address:    parts[1],
					StatusAddr: parts[2],
				})
			}
			// erase error
			err = nil
		}
	})
	if err != nil {
		return nil, errors.Trace(err)
	}

	type result struct {
		addr string
		rows [][]types.Datum
		err  error
	}

	wg := sync.WaitGroup{}
	ch := make(chan result, len(servers))
	for _, server := range servers {
		statusAddr := server.StatusAddr
		if len(statusAddr) == 0 {
			ctx.GetSessionVars().StmtCtx.AppendWarning(errors.Errorf("TiKV node %s does not contain status address", server.Address))
			continue
		}

		wg.Add(1)
		go func(address string) {
			util.WithRecovery(func() {
				defer wg.Done()
				url := fmt.Sprintf("http://%s%s", statusAddr, uri)
				req, err := http.NewRequest(http.MethodGet, url, nil)
				if err != nil {
					ch <- result{err: errors.Trace(err)}
					return
				}
				// Forbidden PD follower proxy
				req.Header.Add("PD-Allow-follower-handle", "true")
				// TiKV output svg format in default
				req.Header.Add("Content-Type", "application/protobuf")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					ch <- result{err: errors.Trace(err)}
					return
				}
				defer func() {
					terror.Log(resp.Body.Close())
				}()
				if resp.StatusCode != http.StatusOK {
					ch <- result{err: errors.Errorf("request %s failed: %s", url, resp.Status)}
					return
				}
				collector := profile.Collector{}
				var rows [][]types.Datum
				if isGoroutine {
					rows, err = collector.ParseGoroutines(resp.Body)
				} else {
					rows, err = collector.ProfileReaderToDatums(resp.Body)
				}
				if err != nil {
					ch <- result{err: errors.Trace(err)}
					return
				}
				ch <- result{addr: address, rows: rows}
			}, nil)
		}(statusAddr)
	}

	wg.Wait()
	close(ch)

	// Keep the original order to make the result more stable
	var results []result
	for result := range ch {
		if result.err != nil {
			ctx.GetSessionVars().StmtCtx.AppendWarning(result.err)
			continue
		}
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].addr < results[j].addr })
	var finalRows [][]types.Datum
	for _, result := range results {
		addr := types.NewStringDatum(result.addr)
		for _, row := range result.rows {
			// Insert the node address in front of rows
			finalRows = append(finalRows, append([]types.Datum{addr}, row...))
		}
	}
	return finalRows, nil
}
