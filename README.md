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
stats.exe -config=/path/to/config.yaml  # 指定配置文件（默认工作目录下的 config.yaml）
```

配置文件 `config.yaml` 用于设置邮箱等参数，详见下方「配置文件（config.yaml）」。

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

四种报告生成成功后，都会**自动把报告正文发到邮箱**（读取 `reports/` 下对应文件作为正文，
主题形如 `[glean] 日报 2026-09-05`、`[glean] 周报 2026-08-31 ~ 2026-09-06`、`[glean] 月报 2026-08`、`[glean] 年报 2026`）。
邮件发送失败只记日志，不影响报告文件本身；配置与测试见下方「邮件发送」。

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

## 配置文件（config.yaml）
启动时读取**工作目录下**的 `config.yaml`，可用 `-config` 指定其他路径（相对路径基于工作目录）：
```bash
stats.exe -config /plugins/data/glean/config.yaml
```
- 文件不存在：**自动生成**一份带注释的默认 `config.yaml`（内容即内置默认值），本次运行使用默认值，日志会提示生成路径
- 字段缺失或为空：仅该字段回退到默认值
- 读取/解析失败：整体回退到默认配置，不中断运行

当前配置的 `mail` 段（邮件设置）：
```yaml
mail:
  smtp_host: smtp.qq.com       # SMTP 服务器地址
  smtp_port: 465               # 端口：465=隐式 TLS，587=STARTTLS
  from: 768305875@qq.com       # 发件邮箱
  auth_code: xxxxxxxx          # 邮箱授权码（非登录密码）
  to: 768305875@qq.com         # 收件邮箱，多个用 , 或 ; 分隔
  subject_prefix: '[glean]'    # 邮件主题前缀，留空则不加前缀
  skip_verify: true            # 是否跳过 TLS 证书校验
```

主题由「前缀 + 报告名」组成，例如 `subject_prefix: '[glean]'` 时日报主题为 `[glean] 日报 2026-09-05`；
改成 `[Insigmind 统计]`、`生产环境` 等任意文字即可，留空则主题为 `日报 2026-09-05`。

优先级：**环境变量 > config.yaml > 内置默认值**，环境变量用于临时覆盖：

| 环境变量 | 对应字段 | 说明 |
|----------|----------|------|
| `GLEAN_SMTP_HOST` | `smtp_host` | SMTP 服务器 |
| `GLEAN_SMTP_PORT` | `smtp_port` | SMTP 端口 |
| `GLEAN_MAIL_FROM` | `from` | 发件邮箱 |
| `GLEAN_MAIL_AUTH` | `auth_code` | 授权码 |
| `GLEAN_MAIL_TO` | `to` | 收件邮箱 |
| `GLEAN_MAIL_SUBJECT_PREFIX` | `subject_prefix` | 邮件主题前缀 |
| `GLEAN_MAIL_SKIP_VERIFY` | `skip_verify` | `1`/`true` 表示跳过证书校验 |

> 部署到路由器时，需把 `config.yaml` 一起放到 `$PLUGIN_DIR`（`/plugins/data/glean`）下。
> 文件内含授权码，建议不要提交到公开仓库（可加入 `.gitignore`）。

## 邮件发送
`mail.go` 提供基于 `net/smtp` 的邮件发送能力（从 IPMailer 提取并适配本项目）：
- 465 端口：隐式 TLS 直连；其他端口（如 587）：先连接再 STARTTLS 升级
- 中文主题自动做 RFC2047 编码，正文为 UTF-8 纯文本，时间统一取东八区
- 连接/交互超时 15 秒，避免网络异常时挂起

代码内调用：
```go
SendMail("标题", "正文")                // 使用 config.yaml / 环境变量 / 默认值
SendMailWithConfig(cfg, "标题", "正文") // 使用自定义配置
```

验证配置：
```bash
stats.exe -mail            # 发送一封测试邮件后退出
stats.exe -mail -debug     # 附带 SMTP 交互调试日志
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
