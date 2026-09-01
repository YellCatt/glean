# Insigmind 活跃度统计

每小时访问首页统计接口，获取并统计 **累计寻源次数(searchCount)** 和 **累计触达次数(contactCount)**，
对比上一小时计算增量（活跃度），并自动生成每日报告。

## 接口
- 方法: GET
- URL: https://api.insigmind.com/Api/WebLogin/GetIndexStats

## 时区
程序内部**统一使用东八区（Asia/Shanghai / UTC+8）**，与运行环境所在系统时区无关：
- 采集时间戳、日志文件名（按天切分）、每天 5:00 的报告调度、周/月/年周期判定，均以东八区为准
- 优先读取系统时区数据库中的 `Asia/Shanghai`；若运行环境无 tzdata（如精简路由器固件），
  自动退化为固定 `+08:00` 偏移，两种情况下行为一致
- 因此部署在 UTC 或其他时区的机器上时，无需额外调整

## 构建与运行
```bash
go build -o stats.exe main.go
stats.exe            # 常驻：立即采集一次，之后每小时整点自动采集
stats.exe --once     # 仅采集一次后退出（适合配合系统计划任务）
stats.exe -debug     # 开启调试日志（可与其他参数组合）
stats.exe -version   # 打印版本号后退出
```

### 版本号
程序启动时会在日志中打印版本号（同时写入 `logs/` 与控制台）：
```
2026/09/02 06:10:00 启动 insigmind 统计采集 v1.2.3
```
默认版本号为 `dev`。发布时通过 ldflags 注入：
```bash
go build -ldflags "-X main.version=1.2.3" -o stats.exe main.go
```
守护脚本热更新后，可从 `glean.log` 中的该行确认实际运行的是哪个版本。

### 调试日志（-debug）
开启后额外打印 `[DEBUG]` 前缀的详细日志（同时写入控制台和 `logs/`），覆盖：
- 启动阶段：工作目录、各文件路径、命令行参数、运行模式
- 请求阶段：请求 URL/头、HTTP 状态码、响应头、耗时、响应体原文（超 1000 字节自动截断）、JSON 解析结果
- 数据阶段：历史文件读取/解析/条数/区间、CSV 是否写表头及行内容、写入字节数
- 报告阶段：周期范围、聚合命中条数、负增量钳制次数、周期检测结果（是否周末/月末/年末）、报告文件路径与大小
- 调度阶段：整点对齐等待时长、定时器触发时间、每次采集总耗时

关闭 `-debug` 时这些日志完全不输出，不影响正常日志与性能。

## 报告（写入 ./reports 目录）
四种报告均使用字符条形图展示每小时/每天/每月的增量活跃度（最大值带 ⚠️ 标记）：

| 报告 | 文件名 | 粒度 | 自动触发时机 |
|------|--------|------|--------------|
| 日报 | `YYYY-MM-DD_report.txt` | 每小时 | 每天早上 **5:00** 自动为**前一天**生成 |
| 周报 | `week_YYYY-MM-DD_report.txt`（周一日期） | 每天（周一~周日） | 每周一 **5:00** 自动生成上周（前一天为周日时） |
| 月报 | `month_YYYY-MM_report.txt` | 每天 | 每月 1 日 **5:00** 自动生成上月（前一天为月末时） |
| 年报 | `year_YYYY_report.txt` | 每月 | 每年 1/1 **5:00** 自动生成去年（前一天为 12/31 时） |

> 表中所有时刻均为**东八区时间**。

手动补生成（基于本地 `stats_history.json` 历史数据）：
```bash
stats.exe -report=2026-09-01        # 日报
stats.exe -week=2026-09-03          # 周报（该日期所在周）
stats.exe -month=2026-08            # 月报
stats.exe -year=2026                # 年报
```

报告示例（条形图部分）：
```
  【累计寻源次数】(次)
  08-31 00:00 ░░░░░░░░░░░░░░░░░░░░           0
  08-31 01:00 █████░░░░░░░░░░░░░░░         123
  08-31 19:00 ████████████████████        3300 ⚠️
```

## 数据文件
- `logs/glean_YYYY-MM-DD.log` : 运行日志（按天切分，同时输出到控制台）
- `stats_history.json` : 每次采集的原始累计值列表
- `stats_log.csv`      : 增量统计（UTF-8 BOM，Excel 可直接打开）
  - 累计寻源次数 / 当小时寻源增量
  - 累计触达次数 / 当小时触达增量
- `reports/*.txt`      : 每日/周/月/年活跃度报告

## 守护脚本部署（Linux / 路由器插件）
`glean.sh` 为常驻守护脚本，功能：启动前清理残留进程、等待网络就绪、从 GitHub 下载/热更新二进制、进程崩溃后指数退避重启（5s→300s）、PID 防重复启动。

部署目录：`/plugins/data/glean`（脚本会 `cd` 到此目录，程序的数据文件均生成于此）。

```bash
chmod +x glean.sh
./glean.sh            # 前台运行
nohup ./glean.sh > /dev/null 2>&1 &   # 后台运行
```

脚本内可调整的关键配置：
| 变量 | 默认值 | 说明 |
|------|--------|------|
| `PLUGIN_DIR` | `/plugins/data/glean` | 插件与数据目录 |
| `DOWNLOAD_URL` | `.../download/dev-latest/default.glean_linux_mipsle` | 下载地址 |
| `UPDATE_INTERVAL` | `14400`（4 小时） | 更新检查间隔 |
| `MAX_RETRY` | `20` | 下载最大重试次数 |

产物：`glean.log`（脚本日志 + 程序 stdout）、`glean.pid`、`logs/`、`reports/`、`stats_history.json`、`stats_log.csv`。

注意：脚本必须使用 **LF 换行**（若经 Windows 编辑需 `dos2unix glean.sh`）。
