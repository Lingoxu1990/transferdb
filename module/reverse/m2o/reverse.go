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
package m2o

import (
	"context"
	"fmt"
	"github.com/wentaojin/transferdb/common"
	"github.com/wentaojin/transferdb/config"
	"github.com/wentaojin/transferdb/database/meta"
	"github.com/wentaojin/transferdb/database/mysql"
	"github.com/wentaojin/transferdb/database/oracle"
	"github.com/wentaojin/transferdb/module/reverse"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"path/filepath"
	"time"
)

type Reverse struct {
	ctx    context.Context
	cfg    *config.Config
	mysql  *mysql.MySQL
	oracle *oracle.Oracle
	metaDB *meta.Meta
}

func NewReverse(ctx context.Context, cfg *config.Config) (*Reverse, error) {
	oracleDB, err := oracle.NewOracleDBEngine(ctx, cfg.OracleConfig)
	if err != nil {
		return nil, err
	}
	mysqlDB, err := mysql.NewMySQLDBEngine(ctx, cfg.MySQLConfig)
	if err != nil {
		return nil, err
	}
	metaDB, err := meta.NewMetaDBEngine(ctx, cfg.MySQLConfig, cfg.AppConfig.SlowlogThreshold)
	if err != nil {
		return nil, err
	}
	return &Reverse{
		ctx:    ctx,
		cfg:    cfg,
		mysql:  mysqlDB,
		oracle: oracleDB,
		metaDB: metaDB,
	}, nil
}

func (r *Reverse) Reverse() error {
	startTime := time.Now()
	zap.L().Info("reverse table mysql to oracle start",
		zap.String("schema", r.cfg.MySQLConfig.SchemaName))

	// 获取配置文件待同步表列表
	exporters, viewTables, err := filterCFGTable(r.cfg, r.mysql)
	if err != nil {
		return err
	}

	if (len(exporters) + len(viewTables)) == 0 {
		zap.L().Warn("there are no table objects in the mysql schema",
			zap.String("schema", r.cfg.MySQLConfig.SchemaName))
		return nil
	}

	// 判断 error_log_detail 是否存在错误记录，是否可进行 reverse
	errTotals, err := meta.NewErrorLogDetailModel(r.metaDB).CountsErrorLogBySchema(r.ctx, &meta.ErrorLogDetail{
		DBTypeS:     r.cfg.DBTypeS,
		DBTypeT:     r.cfg.DBTypeT,
		SchemaNameS: common.StringUPPER(r.cfg.MySQLConfig.SchemaName),
		TaskMode:    r.cfg.TaskMode,
	})
	if errTotals > 0 || err != nil {
		return fmt.Errorf("reverse schema [%s] table mode [%s] task failed: %v, table [error_log_detail] exist failed error, please clear and rerunning", r.cfg.MySQLConfig.SchemaName, r.cfg.TaskMode, err)
	}

	// 环境信息
	oracleDBVersion, err := r.oracle.GetOracleDBVersion()
	if err != nil {
		return fmt.Errorf("get oracle db version falied: %v", err)
	}

	// Oracle 12.2 版本及以上，column collation extended 模式检查
	isExtended := false

	if common.VersionOrdinal(oracleDBVersion) >= common.VersionOrdinal(common.OracleTableColumnCollationDBVersion) {
		isExtended, err = r.oracle.GetOracleExtendedMode()
		if err != nil {
			return fmt.Errorf("get oracle version [%s] extended mode failed: %v", oracleDBVersion, err)
		}
	}

	reverseTaskTables, errCompatibility, tableCharSetMap, tableCollationMap, err := PreCheckCompatibility(r.cfg, r.mysql, exporters, oracleDBVersion, isExtended)
	if err != nil {
		return err
	}

	// 获取规则
	ruleTime := time.Now()
	tableNameRuleMap, tableColumnRuleMap, tableDefaultRuleMap, err := IChanger(&Change{
		Ctx:              r.ctx,
		DBTypeS:          r.cfg.DBTypeS,
		DBTypeT:          r.cfg.DBTypeT,
		SourceSchemaName: common.StringUPPER(r.cfg.MySQLConfig.SchemaName),
		TargetSchemaName: common.StringUPPER(r.cfg.OracleConfig.SchemaName),
		SourceTables:     reverseTaskTables,
		Threads:          r.cfg.ReverseConfig.ReverseThreads,
		MySQL:            r.mysql,
		MetaDB:           r.metaDB,
	})
	if err != nil {
		return err
	}
	zap.L().Warn("get all rules",
		zap.String("schema", r.cfg.MySQLConfig.SchemaName),
		zap.String("cost", time.Now().Sub(ruleTime).String()))

	tables, err := GenReverseTableTask(r, tableNameRuleMap, tableColumnRuleMap, tableDefaultRuleMap, reverseTaskTables, oracleDBVersion, isExtended, tableCharSetMap, tableCollationMap)
	if err != nil {
		return err
	}

	// file writer
	f, err := reverse.NewWriter(r.cfg, r.mysql, r.oracle)
	if err != nil {
		return err
	}

	// schema create
	err = GenCreateSchema(f,
		common.StringUPPER(r.cfg.MySQLConfig.SchemaName), common.StringUPPER(r.cfg.OracleConfig.SchemaName), r.cfg.ReverseConfig.DirectWrite)
	if err != nil {
		return err
	}

	// 表类型不兼容项输出
	err = GenCompatibilityTable(f, common.StringUPPER(r.cfg.MySQLConfig.SchemaName), errCompatibility, viewTables)
	if err != nil {
		return err
	}

	// 表转换
	g := &errgroup.Group{}
	g.SetLimit(r.cfg.ReverseConfig.ReverseThreads)

	for _, table := range tables {
		t := table
		g.Go(func() error {
			rule, err := IReader(t)
			if err != nil {
				if err = meta.NewErrorLogDetailModel(r.metaDB).CreateErrorLog(r.ctx, &meta.ErrorLogDetail{
					DBTypeS:     r.cfg.DBTypeS,
					DBTypeT:     r.cfg.DBTypeT,
					SchemaNameS: t.SourceSchemaName,
					TableNameS:  t.SourceTableName,
					SchemaNameT: t.TargetSchemaName,
					TableNameT:  t.TargetTableName,
					TaskMode:    r.cfg.TaskMode,
					TaskStatus:  "Failed",
					InfoDetail:  t.String(),
					ErrorDetail: err.Error(),
				}); err != nil {
					zap.L().Error("reverse table mysql to oracle failed",
						zap.String("schema", t.SourceSchemaName),
						zap.String("table", t.SourceTableName),
						zap.Error(
							fmt.Errorf("reader table task failed, detail see [error_log_detail], please rerunning")))

					return fmt.Errorf("reader table task failed, detail see [error_log_detail], please rerunning, error: %v", err)
				}
				return nil
			}
			ddl, err := IReverse(rule)
			if err != nil {
				if err = meta.NewErrorLogDetailModel(r.metaDB).CreateErrorLog(r.ctx, &meta.ErrorLogDetail{
					DBTypeS:     r.cfg.DBTypeS,
					DBTypeT:     r.cfg.DBTypeT,
					SchemaNameS: t.SourceSchemaName,
					TableNameS:  t.SourceTableName,
					SchemaNameT: t.TargetSchemaName,
					TableNameT:  t.TargetTableName,
					TaskMode:    r.cfg.TaskMode,
					TaskStatus:  "Failed",
					InfoDetail:  t.String(),
					ErrorDetail: err.Error(),
				}); err != nil {
					zap.L().Error("reverse table mysql to oracle failed",
						zap.String("schema", t.SourceSchemaName),
						zap.String("table", t.SourceTableName),
						zap.Error(
							fmt.Errorf("reverse table task failed, detail see [error_log_detail], please rerunning")))

					return fmt.Errorf("reverse table task failed, detail see [error_log_detail], please rerunning, error: %v", err)
				}
				return nil
			}
			err = IWriter(f, ddl)
			if err != nil {
				if err = meta.NewErrorLogDetailModel(r.metaDB).CreateErrorLog(r.ctx, &meta.ErrorLogDetail{
					DBTypeS:     r.cfg.DBTypeS,
					DBTypeT:     r.cfg.DBTypeT,
					SchemaNameS: t.SourceSchemaName,
					TableNameS:  t.SourceTableName,
					SchemaNameT: t.TargetSchemaName,
					TableNameT:  t.TargetTableName,
					TaskMode:    r.cfg.TaskMode,
					TaskStatus:  "Failed",
					InfoDetail:  t.String(),
					ErrorDetail: err.Error(),
				}); err != nil {
					zap.L().Error("reverse table mysql to oracle failed",
						zap.String("schema", t.SourceSchemaName),
						zap.String("table", t.SourceTableName),
						zap.Error(
							fmt.Errorf("writer table task failed, detail see [error_log_detail], please rerunning")))

					return fmt.Errorf("writer table task failed, detail see [error_log_detail], please rerunning, error: %v", err)
				}
				return nil
			}
			return nil
		})
	}

	if err = g.Wait(); err != nil {
		return err
	}

	err = f.Close()
	if err != nil {
		return err
	}

	errTotals, err = meta.NewErrorLogDetailModel(r.metaDB).CountsErrorLogBySchema(r.ctx, &meta.ErrorLogDetail{
		DBTypeS:     r.cfg.DBTypeS,
		DBTypeT:     r.cfg.DBTypeT,
		SchemaNameS: common.StringUPPER(r.cfg.MySQLConfig.SchemaName),
		TaskMode:    r.cfg.TaskMode,
	})
	if err != nil {
		return err
	}

	endTime := time.Now()
	if !r.cfg.ReverseConfig.DirectWrite {
		zap.L().Info("reverse", zap.String("create table and index output", filepath.Join(r.cfg.ReverseConfig.DDLReverseDir,
			fmt.Sprintf("reverse_%s.sql", r.cfg.MySQLConfig.SchemaName))))
	}
	zap.L().Info("compatibility", zap.String("maybe exist compatibility output", filepath.Join(r.cfg.ReverseConfig.DDLCompatibleDir,
		fmt.Sprintf("compatibility_%s.sql", r.cfg.MySQLConfig.SchemaName))))
	if errTotals == 0 {
		zap.L().Info("reverse table mysql to oracle finished",
			zap.Int("table totals", len(tables)),
			zap.Int("table success", len(tables)),
			zap.Int64("table failed", errTotals),
			zap.String("cost", endTime.Sub(startTime).String()))
	} else {
		zap.L().Warn("reverse table mysql to oracle finished",
			zap.Int("table totals", len(tables)),
			zap.Int("table success", len(tables)-int(errTotals)),
			zap.Int64("table failed", errTotals),
			zap.String("failed tips", "failed detail, please see table [error_log_detail]"),
			zap.String("cost", endTime.Sub(startTime).String()))
	}
	return nil
}
