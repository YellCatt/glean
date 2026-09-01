# Insigmind 活跃度统计

每小时访问首页统计接口，获取并统计 **累计寻源次数(searchCount)** 和 **累计触达次数(contactCount)**，
对比上一小时计算增量（活跃度），并自动生成每日报告。

## 接口
- 方法: GET
- URL: https://api.insigmind.com/Api/WebLogin/GetIndexStats

## 构建与运行
```bash
go build -o stats.exe main.go
stats.exe            # 常驻：立即采集一次，之后每小时整点自动采集
stats.exe --once     # 仅采集一次后退出（适合配合系统计划任务）
```

## 每日报告
程序自动在 `./reports` 目录下生成日报告 `YYYY-MM-DD_report.txt`。
- 触发时机：检测到跨天（当天首条记录与上一条记录日期不同）时，自动为**上一天**生成报告。
- 报告内容：当日采集点数、活跃小时数、寻源/触达的日初累计/日末累计/当日新增、峰值小时。
- 手动补生成：`stats.exe -report=2026-09-01`（基于本地 `stats_history.json` 历史数据）。

## 数据文件
- `stats_history.json` : 每次采集的原始累计值列表
- `stats_log.csv`      : 增量统计（UTF-8 BOM，Excel 可直接打开）
  - 累计寻源次数 / 当小时寻源增量
  - 累计触达次数 / 当小时触达增量
- `reports/*.txt`      : 每日活跃度报告
