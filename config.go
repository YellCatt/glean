package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// AppConfig 应用配置，对应 config.yaml
type AppConfig struct {
	Mail   MailConfig   `yaml:"mail"`
	Report ReportConfig `yaml:"report"`
}

// DefaultReportHour 每天生成定时报告的默认小时（东八区），配置缺失或非法时回退到此值
const DefaultReportHour = 5

// ClockTime 一天中的时刻（东八区），支持 "05:00" / "5:30" / 5 等多种写法
type ClockTime struct {
	Hour   int
	Minute int
}

// String 输出 "HH:MM" 形式
func (c ClockTime) String() string {
	return fmt.Sprintf("%02d:%02d", c.Hour, c.Minute)
}

// UnmarshalYAML 自定义解析：既接受 "05:30" 这类字符串，也接受 5 这类整数（视为 5:00）。
// 这样即使写成 time: 5 也不会导致整个配置文件解析失败。
func (c *ClockTime) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err == nil {
		h, m, ok := parseClock(s)
		if !ok {
			return fmt.Errorf("无法解析时间 %q，应形如 05:30（时 0-23，分 0-59）", s)
		}
		c.Hour, c.Minute = h, m
		return nil
	}
	var i int
	if err := value.Decode(&i); err == nil {
		if i < 0 || i > 23 {
			return fmt.Errorf("小时 %d 超出范围 0-23", i)
		}
		c.Hour, c.Minute = i, 0
		return nil
	}
	return fmt.Errorf("时间应形如 05:30（字符串）或 0-23 的整数")
}

// parseClock 解析 "HH:MM" / "H:MM" / "HH" / "H" / "HH:MM:SS"，非法返回 ok=false
func parseClock(s string) (hour, minute int, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, false
	}
	parts := strings.Split(s, ":")
	if len(parts) < 1 || len(parts) > 3 {
		return 0, 0, false
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || h < 0 || h > 23 {
		return 0, 0, false
	}
	if len(parts) == 1 {
		return h, 0, true
	}
	m, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// ReportConfig 报告相关配置
type ReportConfig struct {
	// StartupMail 启动时生成的日报/周报/月报/年报是否同时发送邮件，默认 false（只落文件与日志）
	StartupMail bool `yaml:"startup_mail"`
	// PeriodicMail 每天定时（report.time 时刻）生成的报告是否发送邮件，默认 true。
	// 用指针是为了区分"未配置"与"显式写 false"，避免配置段为空时被误关
	PeriodicMail *bool `yaml:"periodic_mail"`
	// Time 每天生成定时报告的时刻（东八区 HH:MM），默认 05:00，支持分钟（如 05:30）
	Time *ClockTime `yaml:"time"`
	// Hour 已废弃：旧配置的整数小时（如 hour: 5），仅在 time 未配置时兜底使用
	Hour *int `yaml:"hour"`
}

// appConfig 全局生效的配置，在 main 解析完命令行参数后加载
var appConfig *AppConfig

// DefaultReportConfig 内置默认报告配置：启动报告默认不发邮件、定时报告默认发邮件、默认 05:00 生成
func DefaultReportConfig() *ReportConfig {
	periodicMail := true
	return &ReportConfig{
		StartupMail:  false,
		PeriodicMail: &periodicMail,
		Time:         &ClockTime{Hour: DefaultReportHour, Minute: 0},
	}
}

// fillDefaults 补齐未配置（或 report 段为空）的字段；兼容旧配置的 hour 字段
func (r *ReportConfig) fillDefaults() {
	d := DefaultReportConfig()
	if r.PeriodicMail == nil {
		r.PeriodicMail = d.PeriodicMail
	}
	if r.Time == nil {
		if r.Hour != nil && *r.Hour >= 0 && *r.Hour <= 23 {
			r.Time = &ClockTime{Hour: *r.Hour} // 旧配置 hour: 5 等价于 05:00
		} else {
			r.Time = d.Time
		}
	}
}

// Clock 取生效的报告时刻（东八区 时/分），配置越界时回退默认值
func (r *ReportConfig) Clock() (hour, minute int) {
	if r.Time == nil {
		return DefaultReportHour, 0
	}
	h, m := r.Time.Hour, r.Time.Minute
	if h < 0 || h > 23 {
		h, m = DefaultReportHour, 0
	}
	if m < 0 || m > 59 {
		m = 0
	}
	return h, m
}

// TimeString 返回 "HH:MM" 形式的报告时刻（用于日志与配置文件模板）
func (r *ReportConfig) TimeString() string {
	h, m := r.Clock()
	return fmt.Sprintf("%02d:%02d", h, m)
}

// PeriodicMailValue 取"每天定时报告是否发送邮件"，未配置时为 true
func (r *ReportConfig) PeriodicMailValue() bool {
	if r.PeriodicMail == nil {
		return true
	}
	return *r.PeriodicMail
}

// configTemplate 自动生成配置文件时使用的模板，占位符由内置默认值填充
const configTemplate = `# glean 配置文件（首次运行自动生成，可按需修改，重启后生效）
# 字段留空则回退到内置默认值；环境变量优先级高于本文件
# 可用 -config=路径 指定其他配置文件

mail:
  # SMTP 服务器地址与端口（465=隐式 TLS，587=STARTTLS）
  smtp_host: %s
  smtp_port: %d
  # 发件邮箱与授权码（授权码不是登录密码）
  from: %s
  auth_code: %s
  # 收件邮箱，多个用 , 或 ; 分隔
  to: %s
  # 邮件主题前缀，最终主题形如 "[glean] 日报 2026-09-05"，留空则不加前缀
  # 注意：用 [ ] 开头时整段必须用引号包住，否则 YAML 会当成数组导致解析失败
  subject_prefix: '%s'
  # 是否跳过 TLS 证书校验
  skip_verify: %t

report:
  # 每天生成定时报告的时刻（东八区 HH:MM），默认 05:00，支持分钟
  # 例：改成 '08:30' 表示每天东八区 8:30 生成前一天的报告
  # 旧字段 hour: 5 仍兼容（等价于 05:00），建议统一用 time
  time: '%s'
  # 每天定时生成的报告是否发送邮件，默认 true（发送）
  # 改成 false 则只写 reports/ 文件并打印到日志，不发邮件
  periodic_mail: %t
  # 程序启动时立即生成的日报/周报/月报/年报，是否同时发送邮件
  # false（默认）：只写 reports/ 文件并打印到日志，不发邮件
  # true        ：启动即发送 4 封报告邮件
  startup_mail: %t
`

// DefaultAppConfig 内置默认配置
func DefaultAppConfig() *AppConfig {
	return &AppConfig{Mail: DefaultMailConfig(), Report: *DefaultReportConfig()}
}

// generateConfigFile 生成一份带注释的默认配置文件
func generateConfigFile(path string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}

	m := DefaultMailConfig()
	r := DefaultReportConfig()
	content := fmt.Sprintf(configTemplate,
		m.SMTPHost, m.SMTPPort, m.FromEmail, m.AuthCode, m.ToEmail,
		// YAML 单引号字符串内的单引号需写成两个单引号
		strings.ReplaceAll(m.SubjectPrefix, "'", "''"), m.TLSSkipVerify,
		r.TimeString(), r.PeriodicMailValue(), r.StartupMail)

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return nil
}

// LoadAppConfig 读取 YAML 配置文件：
//   - 文件不存在：自动生成一份默认配置，本次运行使用默认值
//   - 文件存在但字段缺失/为空：缺失字段回退到默认值
//   - 读取或解析失败：返回默认配置并附带错误，由调用方决定是否继续
//
// 返回的 error 只表示"未能使用文件配置"，调用方记录日志即可，不应中断运行。
func LoadAppConfig(path string) (*AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return DefaultAppConfig(), fmt.Errorf("读取配置文件失败，使用默认配置: %w", err)
		}
		if genErr := generateConfigFile(path); genErr != nil {
			return DefaultAppConfig(), fmt.Errorf("配置文件 %s 不存在且自动生成失败，使用默认配置: %w", path, genErr)
		}
		return DefaultAppConfig(), fmt.Errorf("配置文件不存在，已自动生成默认配置: %s", path)
	}

	// 空文件（如手动新建但没填内容）也按"没有配置"处理，直接补写一份默认配置
	if len(strings.TrimSpace(string(data))) == 0 {
		if genErr := generateConfigFile(path); genErr != nil {
			return DefaultAppConfig(), fmt.Errorf("配置文件 %s 为空且补写默认配置失败，使用默认配置: %w", path, genErr)
		}
		return DefaultAppConfig(), fmt.Errorf("配置文件 %s 为空，已补写默认配置", path)
	}

	cfg := DefaultAppConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		// 最常见的是 subject_prefix 写成 [glean]（YAML 会当成数组），这里给出针对性提示
		return DefaultAppConfig(), fmt.Errorf("解析配置文件 %s 失败，使用默认配置: %w"+
			"（提示：以 [ 开头的主题前缀请加引号，如 subject_prefix: '[glean]'）", path, err)
	}

	cfg.Mail.fillDefaults()
	cfg.Report.fillDefaults()
	return cfg, nil
}