/*
Copyright © 2020 Marvin

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
package oracle

import (
	"context"
	"fmt"
	"github.com/shopspring/decimal"
	"github.com/thinkeridea/go-extend/exstrings"
	"github.com/wentaojin/transferdb/common"
)

func (o *Oracle) GetOracleCurrentSnapshotSCN() (uint64, error) {
	// 获取当前 SCN 号
	_, res, err := Query(o.Ctx, o.OracleDB, "select min(current_scn) CURRENT_SCN from gv$database")
	var globalSCN uint64
	if err != nil {
		return globalSCN, err
	}
	globalSCN, err = common.StrconvUintBitSize(res[0]["CURRENT_SCN"], 64)
	if err != nil {
		return globalSCN, fmt.Errorf("get oracle current snapshot scn %s utils.StrconvUintBitSize failed: %v", res[0]["CURRENT_SCN"], err)
	}
	return globalSCN, nil
}

func (o *Oracle) StartOracleChunkCreateTask(taskName string) error {
	querySQL := common.StringsBuilder(`SELECT COUNT(1) COUNT FROM user_parallel_execute_chunks WHERE TASK_NAME='`, taskName, `'`)
	_, res, err := Query(o.Ctx, o.OracleDB, querySQL)
	if err != nil {
		return err
	}
	if res[0]["COUNT"] != "0" {
		if err = o.CloseOracleChunkTask(taskName); err != nil {
			return err
		}
	}

	ctx, _ := context.WithCancel(o.Ctx)
	createSQL := common.StringsBuilder(`BEGIN
  DBMS_PARALLEL_EXECUTE.CREATE_TASK (task_name => '`, taskName, `');
END;`)
	_, err = o.OracleDB.ExecContext(ctx, createSQL)
	if err != nil {
		return fmt.Errorf("oracle DBMS_PARALLEL_EXECUTE create task failed: %v, sql: %v", err, createSQL)
	}
	return nil
}

func (o *Oracle) StartOracleCreateChunkByRowID(taskName, schemaName, tableName string, chunkSize string) error {
	ctx, _ := context.WithCancel(o.Ctx)

	chunkSQL := common.StringsBuilder(`BEGIN
  DBMS_PARALLEL_EXECUTE.CREATE_CHUNKS_BY_ROWID (task_name   => '`, taskName, `',
                                               table_owner => '`, schemaName, `',
                                               table_name  => '`, tableName, `',
                                               by_row      => TRUE,
                                               chunk_size  => `, chunkSize, `);
END;`)
	_, err := o.OracleDB.ExecContext(ctx, chunkSQL)
	if err != nil {
		return fmt.Errorf("oracle DBMS_PARALLEL_EXECUTE create_chunks_by_rowid task failed: %v, sql: %v", err, chunkSQL)
	}
	return nil
}

func (o *Oracle) GetOracleTableChunksByRowID(taskName string) ([]map[string]string, error) {
	querySQL := common.StringsBuilder(`SELECT 'ROWID BETWEEN ''' || start_rowid || ''' AND ''' || end_rowid || '''' CMD FROM user_parallel_execute_chunks WHERE  task_name = '`, taskName, `' ORDER BY chunk_id`)

	_, res, err := Query(o.Ctx, o.OracleDB, querySQL)
	if err != nil {
		return res, err
	}
	return res, nil
}

func (o *Oracle) CloseOracleChunkTask(taskName string) error {
	ctx, _ := context.WithCancel(context.Background())

	clearSQL := common.StringsBuilder(`BEGIN
  DBMS_PARALLEL_EXECUTE.DROP_TASK ('`, taskName, `');
END;`)

	_, err := o.OracleDB.ExecContext(ctx, clearSQL)
	if err != nil {
		return fmt.Errorf("oracle DBMS_PARALLEL_EXECUTE drop task failed: %v, sql: %v", err, clearSQL)
	}

	return nil
}

// 获取表字段名以及行数据 -> 用于 FULL/ALL
func (o *Oracle) GetOracleTableRowsData(querySQL string, insertBatchSize int) ([]string, []string, error) {
	var (
		err          error
		rowsResult   []string
		rowsTMP      []string
		batchResults []string
		cols         []string
	)
	rows, err := o.OracleDB.QueryContext(o.Ctx, querySQL)
	if err != nil {
		return []string{}, batchResults, err
	}
	defer rows.Close()

	tmpCols, err := rows.Columns()
	if err != nil {
		return cols, batchResults, err
	}

	// 字段名关键字反引号处理
	for _, col := range tmpCols {
		cols = append(cols, common.StringsBuilder("`", col, "`"))
	}

	// 用于判断字段值是数字还是字符
	var (
		columnTypes   []string
		databaseTypes []string
	)
	colTypes, err := rows.ColumnTypes()
	if err != nil {
		return cols, batchResults, err
	}

	for _, ct := range colTypes {
		// 数据库字段类型 DatabaseTypeName() 映射 go 类型 ScanType()
		columnTypes = append(columnTypes, ct.ScanType().String())
		databaseTypes = append(databaseTypes, ct.DatabaseTypeName())
	}

	// 数据 Scan
	columns := len(cols)
	rawResult := make([][]byte, columns)
	dest := make([]interface{}, columns)
	for i := range rawResult {
		dest[i] = &rawResult[i]
	}

	// 表行数读取
	for rows.Next() {
		err = rows.Scan(dest...)
		if err != nil {
			return cols, batchResults, err
		}

		for i, raw := range rawResult {
			// 注意 Oracle/Mysql NULL VS 空字符串区别
			// Oracle 空字符串与 NULL 归于一类，统一 NULL 处理 （is null 可以查询 NULL 以及空字符串值，空字符串查询无法查询到空字符串值）
			// Mysql 空字符串与 NULL 非一类，NULL 是 NULL，空字符串是空字符串（is null 只查询 NULL 值，空字符串查询只查询到空字符串值）
			// 按照 Oracle 特性来，转换同步统一转换成 NULL 即可，但需要注意业务逻辑中空字符串得写入，需要变更
			// Oracle/Mysql 对于 'NULL' 统一字符 NULL 处理，查询出来转成 NULL,所以需要判断处理
			if raw == nil {
				rowsResult = append(rowsResult, fmt.Sprintf("%v", `NULL`))
			} else if string(raw) == "" {
				rowsResult = append(rowsResult, fmt.Sprintf("%v", `NULL`))
			} else {
				switch columnTypes[i] {
				case "int64":
					r, err := common.StrconvIntBitSize(string(raw), 64)
					if err != nil {
						return cols, batchResults, err
					}
					rowsResult = append(rowsResult, fmt.Sprintf("%v", r))
				case "uint64":
					r, err := common.StrconvUintBitSize(string(raw), 64)
					if err != nil {
						return cols, batchResults, err
					}
					rowsResult = append(rowsResult, fmt.Sprintf("%v", r))
				case "float32":
					r, err := common.StrconvFloatBitSize(string(raw), 32)
					if err != nil {
						return cols, batchResults, err
					}
					rowsResult = append(rowsResult, fmt.Sprintf("%v", r))
				case "float64":
					r, err := common.StrconvFloatBitSize(string(raw), 64)
					if err != nil {
						return cols, batchResults, err
					}
					rowsResult = append(rowsResult, fmt.Sprintf("%v", r))
				case "rune":
					r, err := common.StrconvRune(string(raw))
					if err != nil {
						return cols, batchResults, err
					}
					rowsResult = append(rowsResult, fmt.Sprintf("%v", r))
				case "godror.Number":
					r, err := decimal.NewFromString(string(raw))
					if err != nil {
						return cols, rowsResult, err
					}
					if r.IsInteger() {
						si, err := common.StrconvIntBitSize(string(raw), 64)
						if err != nil {
							return cols, rowsResult, err
						}
						rowsResult = append(rowsResult, fmt.Sprintf("%v", si))
					} else {
						rf, err := common.StrconvFloatBitSize(string(raw), 64)
						if err != nil {
							return cols, rowsResult, err
						}
						rowsResult = append(rowsResult, fmt.Sprintf("%v", rf))
					}
				default:
					// 特殊字符
					rowsResult = append(rowsResult, fmt.Sprintf("'%v'", common.SpecialLettersUsingMySQL(raw)))
				}
			}
		}

		rowsTMP = append(rowsTMP, common.StringsBuilder("(", exstrings.Join(rowsResult, ","), ")"))

		// 数组清空
		rowsResult = rowsResult[0:0]

		// batch 批次
		if len(rowsTMP) == insertBatchSize {
			batchResults = append(batchResults, exstrings.Join(rowsTMP, ","))
			// 数组清空
			rowsTMP = rowsTMP[0:0]
		}
	}

	if err = rows.Err(); err != nil {
		return cols, batchResults, err
	}

	// 非 batch 批次
	if len(rowsTMP) > 0 {
		batchResults = append(batchResults, exstrings.Join(rowsTMP, ","))
	}

	return cols, batchResults, nil
}
