package main

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

// mailTimeout SMTP 连接与交互的超时时间，避免网络异常时长时间挂起
const mailTimeout = 15 * time.Second

// MailConfig 邮件（SMTP）发送配置，对应 config.yaml 的 mail 段
type MailConfig struct {
	SMTPHost      string `yaml:"smtp_host"`      // SMTP 服务器地址
	SMTPPort      int    `yaml:"smtp_port"`      // SMTP 端口（465=隐式 TLS，其余如 587=STARTTLS）
	FromEmail     string `yaml:"from"`           // 发件邮箱
	AuthCode      string `yaml:"auth_code"`      // 邮箱授权码（非登录密码）
	ToEmail       string `yaml:"to"`             // 收件邮箱，多个用 , 或 ; 分隔
	SubjectPrefix string `yaml:"subject_prefix"` // 邮件主题前缀，如 "[glean]"，留空则不加前缀
	TLSSkipVerify bool   `yaml:"skip_verify"`    // 是否跳过 TLS 证书校验（部分网络环境下证书链不完整时需要）
}

// DefaultMailConfig 内置默认邮件配置（config.yaml 与环境变量均未配置时使用）
func DefaultMailConfig() MailConfig {
	return MailConfig{
		SMTPHost:      "smtp.qq.com",
		SMTPPort:      465,
		FromEmail:     "768305875@qq.com",
		AuthCode:      "gpfruabgjebubdad",
		ToEmail:       "768305875@qq.com",
		SubjectPrefix: "[glean]",
		TLSSkipVerify: true,
	}
}

// WithPrefix 给主题加上配置的前缀（前缀为空时原样返回）
func (m *MailConfig) WithPrefix(subject string) string {
	if p := strings.TrimSpace(m.SubjectPrefix); p != "" {
		return p + " " + subject
	}
	return subject
}

// fillDefaults 用内置默认值补齐未配置的字段
func (m *MailConfig) fillDefaults() {
	d := DefaultMailConfig()
	if m.SMTPHost == "" {
		m.SMTPHost = d.SMTPHost
	}
	if m.SMTPPort == 0 {
		m.SMTPPort = d.SMTPPort
	}
	if m.FromEmail == "" {
		m.FromEmail = d.FromEmail
	}
	if m.AuthCode == "" {
		m.AuthCode = d.AuthCode
	}
	if m.ToEmail == "" {
		m.ToEmail = d.ToEmail
	}
}

// applyEnv 用环境变量覆盖配置（优先级高于 config.yaml）
func (m *MailConfig) applyEnv() {
	if v := os.Getenv("GLEAN_SMTP_HOST"); v != "" {
		m.SMTPHost = v
	}
	if v := os.Getenv("GLEAN_SMTP_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			m.SMTPPort = p
		}
	}
	if v := os.Getenv("GLEAN_MAIL_FROM"); v != "" {
		m.FromEmail = v
	}
	if v := os.Getenv("GLEAN_MAIL_AUTH"); v != "" {
		m.AuthCode = v
	}
	if v := os.Getenv("GLEAN_MAIL_TO"); v != "" {
		m.ToEmail = v
	}
	if v := os.Getenv("GLEAN_MAIL_SUBJECT_PREFIX"); v != "" {
		m.SubjectPrefix = v
	}
	if v := os.Getenv("GLEAN_MAIL_SKIP_VERIFY"); v != "" {
		m.TLSSkipVerify = v == "1" || strings.EqualFold(v, "true")
	}
}

// loadMailConfig 取得最终生效的邮件配置，优先级：环境变量 > config.yaml > 内置默认值
func loadMailConfig() *MailConfig {
	m := DefaultMailConfig()
	if appConfig != nil {
		m = appConfig.Mail
	}
	m.applyEnv()
	m.fillDefaults()
	return &m
}

// splitRecipients 拆分收件人（支持 , 和 ; 分隔），并去掉空项
func splitRecipients(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';'
	}) {
		if addr := strings.TrimSpace(part); addr != "" {
			out = append(out, addr)
		}
	}
	return out
}

// encodeHeader 对含非 ASCII 字符的邮件头（如中文主题）做 RFC2047 编码，避免乱码
func encodeHeader(s string) string {
	need := false
	for _, r := range s {
		if r > 127 {
			need = true
			break
		}
	}
	if !need {
		return s
	}
	return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
}

// buildMessage 组装一封 UTF-8 纯文本邮件
func buildMessage(from string, to []string, subject, body string, t time.Time) []byte {
	return []byte(fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\n"+
			"Content-Type: text/plain; charset=UTF-8\r\n"+
			"Content-Transfer-Encoding: 8bit\r\n\r\n%s",
		from, strings.Join(to, ", "), encodeHeader(subject), t.Format(time.RFC1123Z), body))
}

// sendWithClient 复用已建立的 SMTP 连接完成认证与投递
func sendWithClient(c *smtp.Client, auth smtp.Auth, from string, to []string, msg []byte) error {
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, addr := range to {
		if err := c.Rcpt(addr); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// sendMailTLS 通过隐式 TLS（如 465 端口）连接 SMTP 并发送邮件
func sendMailTLS(addr string, auth smtp.Auth, from string, to []string, msg []byte, skipVerify bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}

	tlsConfig := &tls.Config{
		InsecureSkipVerify: skipVerify,
		ServerName:         host,
	}

	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: mailTimeout}, "tcp", addr, tlsConfig)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(mailTimeout)); err != nil {
		return err
	}

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer c.Close()

	return sendWithClient(c, auth, from, to, msg)
}

// sendMailStartTLS 先明文连接再 STARTTLS 升级后发送邮件（如 587 端口）
func sendMailStartTLS(addr string, auth smtp.Auth, from string, to []string, msg []byte, skipVerify bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}

	conn, err := net.DialTimeout("tcp", addr, mailTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(mailTimeout)); err != nil {
		return err
	}

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer c.Close()

	if err := c.Hello("localhost"); err != nil {
		return err
	}
	if err := c.StartTLS(&tls.Config{
		InsecureSkipVerify: skipVerify,
		ServerName:         host,
	}); err != nil {
		return err
	}

	return sendWithClient(c, auth, from, to, msg)
}

// SendMail 按配置发送一封纯文本邮件（主题与正文按 UTF-8 处理）
func SendMail(subject, body string) error {
	cfg := loadMailConfig()
	return SendMailWithConfig(cfg, subject, body)
}

// SendMailWithConfig 使用指定配置发送邮件
func SendMailWithConfig(cfg *MailConfig, subject, body string) error {
	if cfg == nil || cfg.SMTPHost == "" || cfg.SMTPPort == 0 || cfg.FromEmail == "" {
		return fmt.Errorf("邮件配置不完整，无法发送")
	}

	to := splitRecipients(cfg.ToEmail)
	if len(to) == 0 {
		return fmt.Errorf("未配置收件人邮箱")
	}

	addr := net.JoinHostPort(cfg.SMTPHost, strconv.Itoa(cfg.SMTPPort))
	auth := smtp.PlainAuth("", cfg.FromEmail, cfg.AuthCode, cfg.SMTPHost)
	msg := buildMessage(cfg.FromEmail, to, subject, body, nowCST())

	debugf("准备发送邮件: 服务器=%s 发件人=%s 收件人=%v 主题=%q 正文=%d 字节 跳过TLS校验=%v",
		addr, cfg.FromEmail, to, subject, len(body), cfg.TLSSkipVerify)

	var err error
	if cfg.SMTPPort == 465 {
		err = sendMailTLS(addr, auth, cfg.FromEmail, to, msg, cfg.TLSSkipVerify)
	} else {
		err = sendMailStartTLS(addr, auth, cfg.FromEmail, to, msg, cfg.TLSSkipVerify)
	}
	if err != nil {
		debugf("邮件发送失败: %v", err)
		return fmt.Errorf("发送邮件失败: %w", err)
	}
	debugf("邮件发送成功: %d 字节", len(msg))
	return nil
}
