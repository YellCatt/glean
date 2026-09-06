package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// AppConfig 应用配置，对应 config.yaml
type AppConfig struct {
	Mail MailConfig `yaml:"mail"`
}

// appConfig 全局生效的配置，在 main 解析完命令行参数后加载
var appConfig *AppConfig

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
`

// DefaultAppConfig 内置默认配置
func DefaultAppConfig() *AppConfig {
	return &AppConfig{Mail: DefaultMailConfig()}
}

// generateConfigFile 生成一份带注释的默认配置文件
func generateConfigFile(path string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}

	m := DefaultMailConfig()
	content := fmt.Sprintf(configTemplate,
		m.SMTPHost, m.SMTPPort, m.FromEmail, m.AuthCode, m.ToEmail,
		// YAML 单引号字符串内的单引号需写成两个单引号
		strings.ReplaceAll(m.SubjectPrefix, "'", "''"), m.TLSSkipVerify)

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
	return cfg, nil
}
