package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// apiURL 首页统计接口（累计寻源次数 / 累计触达次数）
	apiURL = "https://api.insigmind.com/Api/WebLogin/GetIndexStats"
	// demandAPIURL 需求列表接口（取 data.totalCount 作为需求总量）
	demandAPIURL = "https://api.insigmind.com/Api/WebDemand/SearchDemands"
)

// 字段中文名映射
var fieldNames = map[string]string{
	"searchCount":  "累计寻源次数",
	"contactCount": "累计触达次数",
	"totalCount":   "需求总量",
}

// API 返回结构（首页统计）
type apiResponse struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    struct {
		SearchCount  int64 `json:"searchCount"`
		ContactCount int64 `json:"contactCount"`
	} `json:"data"`
}

// demandRequest 需求列表接口的查询条件（固定参数，按发布时间倒序取第一页）
type demandRequest struct {
	PageSize             int    `json:"pageSize"`
	Total                int    `json:"total"`
	PageNumber           int    `json:"pageNumber"`
	DemandUrban          string `json:"demandUrban"`
	DemandExpirationDate string `json:"demandExpirationDate"`
	NewDate              string `json:"newDate"`
	SortFieldName        string `json:"sortFieldName"`
	SortDirection        string `json:"sortDirection"`
	DemandStatus         int    `json:"demandStatus"`
}

// demandResponse 需求列表接口返回结构（只关心 data.totalCount）
type demandResponse struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    struct {
		DemandList      []json.RawMessage `json:"demandList"`
		TotalCount      int64             `json:"totalCount"`
		AIResponseCount int64             `json:"aiResponseCount"`
	} `json:"data"`
}

// 一条历史记录
type record struct {
	Timestamp string `json:"timestamp"`
	Search    int64  `json:"searchCount"`
	Contact   int64  `json:"contactCount"`
	Total     int64  `json:"totalCount"` // 需求总量（本次新增，旧历史文件无此字段时解析为 0）
}

// reportHour 每天生成报告的整点小时（按东八区计）
const reportHour = 5

// version 程序版本号 = 构建时间（东八区 YYYY-MM-DD_HH-MM-SS），由 GitHub Actions 在构建时注入：
//
//	go build -ldflags "-X main.version=2026-09-02_14-30-05" -o glean main.go
//
// 之所以用 var 而非 const，正是为了让 -X 能在链接期覆盖它。
// 本地直接 go build 未注入时，保持默认值 "dev"。
var version = "dev"

// versionKey 日志中打印版本号时使用的固定关键字。
// 日志行格式统一为 "... version=<构建时间>"，便于在 logs/ 或 glean.log 中 grep "version=" 检索。
const versionKey = "version"

// logVersion 以固定关键字打印版本号（同时写入控制台与当天日志文件）
func logVersion() {
	log.Printf("[glean] %s=%s (构建时间, 东八区)", versionKey, version)
}

// chinaLoc 程序统一使用的时区：东八区（Asia/Shanghai / UTC+8）
// 优先从系统时区数据库加载；若运行环境无 tzdata（如精简路由器固件），
// 则退化为固定的 +8 小时偏移，保证行为一致。
var chinaLoc *time.Location

func initChinaLoc() {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil && loc != nil {
		chinaLoc = loc
	} else {
		chinaLoc = time.FixedZone("CST", 8*60*60)
	}
	time.Local = chinaLoc
}

// nowCST 返回当前东八区时间（程序内所有"当前时间"均必须走此函数）
func nowCST() time.Time {
	if chinaLoc == nil {
		initChinaLoc()
	}
	return time.Now().In(chinaLoc)
}

// parseInCST 按东八区解析时间字符串（避免 time.Parse 默认落到 UTC 导致日期偏移）
func parseInCST(layout, value string) (time.Time, error) {
	if chinaLoc == nil {
		initChinaLoc()
	}
	return time.ParseInLocation(layout, value, chinaLoc)
}

var (
	historyFile   string
	logFile       string
	reportsDir    string
	logsDir       string
	sqlDir        string
	debugEnabled  bool
	collectCount  int    // 本次进程内已完成的采集次数
	lastReportDay string // 本次进程内已生成报告的"昨天"日期(YYYY-MM-DD)，避免重复生成

	currentLogPath string // 当前正在写入的日志文件路径，跨天后重新初始化时补打版本号
)

// initDebug 在 flag.Parse 之前扫描命令行，使 init 阶段的日志也能受 -debug 控制
func initDebug() {
	debugEnabled = true
	for _, a := range os.Args[1:] {
		if a == "-debug=false" || a == "--debug=false" {
			debugEnabled = false
			return
		}
	}
}

// cstLogWriter 接管日志的时间戳输出。
// Go 标准库 log 包的前缀时间固定使用**系统本地时区**（如 UTC 机器上会打印 UTC 时间），
// 与本程序"统一东八区"的约定不一致。这里关掉 log 自带的时间戳（log.SetFlags(0)），
// 改由本 Writer 在每条记录前补一个东八区时间戳，使控制台与日志文件的时间口径完全一致。
type cstLogWriter struct{ w io.Writer }

func (c cstLogWriter) Write(p []byte) (int, error) {
	body := strings.TrimRight(string(p), "\r\n")
	if body == "" {
		return len(p), nil // 空行原样放过，不补时间戳
	}
	if _, err := io.WriteString(c.w, nowCST().Format("2006/01/02 15:04:05")+" "+body+"\n"); err != nil {
		return 0, err
	}
	return len(p), nil // 必须返回原始长度，否则 log 包会误判为写入失败
}

// setLogOutput 设置日志输出目标，并统一套上东八区时间戳
func setLogOutput(w io.Writer) {
	log.SetOutput(cstLogWriter{w: w})
}

// debugf 输出调试日志（前缀 [DEBUG] + 毫秒时间），仅在 -debug 开启时打印
func debugf(format string, args ...any) {
	if !debugEnabled {
		return
	}
	log.Printf("[DEBUG] ["+nowCST().Format("15:04:05.000")+"] "+format, args...)
}

// truncate 截断长字符串，避免调试日志被超长响应体撑爆
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("...(已截断, 共%d字节)", len(s))
}

func init() {
	initDebug()
	initChinaLoc() // 必须先于任何时间处理，确保日志/采集/报告全部使用东八区
	log.SetFlags(0)             // 时间戳改由 cstLogWriter 以东八区输出
	setLogOutput(os.Stdout)     // 避免 setupLogger 之前的日志落到 stderr
	dir, err := os.Getwd()
	if err != nil {
		debugf("获取当前工作目录失败: %v", err)
		dir = "."
	}
	historyFile = filepath.Join(dir, "stats_history.json")
	sqlDir = filepath.Join(dir, "sql")
	logFile = filepath.Join(sqlDir, "stats_log.csv")
	reportsDir = filepath.Join(dir, "reports")
	logsDir = filepath.Join(dir, "logs")
	debugf("工作目录: %s", dir)
	if err := os.MkdirAll(reportsDir, 0755); err != nil {
		log.Printf("警告: 无法创建报告目录 %s: %v", reportsDir, err)
	}
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		log.Printf("警告: 无法创建日志目录 %s: %v", logsDir, err)
	}
	if err := os.MkdirAll(sqlDir, 0755); err != nil {
		log.Printf("警告: 无法创建数据目录 %s: %v", sqlDir, err)
	}
	debugf("路径配置: 历史=%s | CSV=%s | 报告目录=%s | 日志目录=%s | 数据目录=%s",
		historyFile, logFile, reportsDir, logsDir, sqlDir)
	setupLogger()
}

// setupLogger 将日志同时输出到 stdout 和 ./logs/glean_YYYY-MM-DD.log
// 每次（跨天）切换到新日志文件时，都会先打印一行版本号，便于确认实际运行的构建
func setupLogger() {
	today := nowCST().Format("2006-01-02")
	logPath := filepath.Join(logsDir, "glean_"+today+".log")
	if logPath == currentLogPath {
		return // 仍是同一天的日志文件，无需重复打开
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		setLogOutput(os.Stdout)
		log.Printf("警告: 无法打开日志文件 %s: %v，仅输出到控制台", logPath, err)
		return
	}
	setLogOutput(io.MultiWriter(os.Stdout, f))
	currentLogPath = logPath
	debugf("日志文件已就绪: %s", logPath)
	logVersion()
}

// newHTTPClient 统一构造 HTTP 客户端：15s 超时 + 跳过 TLS 证书校验
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // 跳过 TLS 证书校验
		},
	}
}

// fetchStats 请求首页统计接口并解析累计寻源/累计触达
func fetchStats() (*record, error) {
	debugf("准备请求接口: GET %s (超时=15s, 跳过TLS校验=true)", apiURL)
	client := newHTTPClient()
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		debugf("构建请求失败: %v", err)
		return nil, fmt.Errorf("构建请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "Apifox/1.0.0 (https://apifox.com)")
	debugf("请求头: User-Agent=%q, Host=%q", req.Header.Get("User-Agent"), req.URL.Host)

	start := nowCST()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		debugf("请求失败 (耗时 %v): %v", elapsed, err)
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	debugf("HTTP 响应: 状态码=%d 状态=%q 耗时=%v", resp.StatusCode, resp.Status, elapsed)
	debugf("响应头: Content-Type=%q Content-Length=%d",
		resp.Header.Get("Content-Type"), resp.ContentLength)
	if resp.StatusCode != http.StatusOK {
		debugf("警告: 状态码非 200 (%d)，仍尝试解析响应体", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		debugf("读取响应体失败: %v", err)
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	debugf("响应体: %d 字节, 内容=%s", len(body), truncate(strings.TrimSpace(string(body)), 1000))

	var ar apiResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		debugf("JSON 解析失败: %v", err)
		return nil, fmt.Errorf("解析 JSON 失败: %w", err)
	}
	debugf("解析结果: success=%v code=%q message=%q searchCount=%d contactCount=%d",
		ar.Success, ar.Code, ar.Message, ar.Data.SearchCount, ar.Data.ContactCount)
	if !ar.Success {
		debugf("接口返回失败标记: message=%q code=%q", ar.Message, ar.Code)
		return nil, fmt.Errorf("接口返回失败: %s", ar.Message)
	}

	rec := &record{
		Timestamp: nowCST().Format("2006-01-02 15:04:05"),
		Search:    ar.Data.SearchCount,
		Contact:   ar.Data.ContactCount,
	}
	debugf("构造记录: 时间=%s 累计寻源=%d 累计触达=%d", rec.Timestamp, rec.Search, rec.Contact)
	return rec, nil
}

// fetchDemandTotal 请求需求列表接口，返回需求总量 data.totalCount
func fetchDemandTotal() (int64, error) {
	payload := demandRequest{
		PageSize:             10,
		Total:                0,
		PageNumber:           1,
		DemandUrban:          "",
		DemandExpirationDate: "",
		NewDate:              "",
		SortFieldName:        "AuditProperties.CreateAt",
		SortDirection:        "Descending",
		DemandStatus:         1,
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		debugf("需求接口请求体序列化失败: %v", err)
		return 0, fmt.Errorf("序列化请求体失败: %w", err)
	}
	debugf("准备请求接口: POST %s (超时=15s, 跳过TLS校验=true)", demandAPIURL)
	debugf("需求接口请求体: %s", string(bodyBytes))

	client := newHTTPClient()
	req, err := http.NewRequest("POST", demandAPIURL, bytes.NewReader(bodyBytes))
	if err != nil {
		debugf("构建需求接口请求失败: %v", err)
		return 0, fmt.Errorf("构建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Apifox/1.0.0 (https://apifox.com)")

	start := nowCST()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		debugf("需求接口请求失败 (耗时 %v): %v", elapsed, err)
		return 0, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	debugf("需求接口 HTTP 响应: 状态码=%d 状态=%q 耗时=%v", resp.StatusCode, resp.Status, elapsed)
	debugf("需求接口响应头: Content-Type=%q Content-Length=%d",
		resp.Header.Get("Content-Type"), resp.ContentLength)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		debugf("读取需求接口响应体失败: %v", err)
		return 0, fmt.Errorf("读取响应失败: %w", err)
	}
	debugf("需求接口响应体: %d 字节, 内容=%s", len(body), truncate(strings.TrimSpace(string(body)), 1000))

	var dr demandResponse
	if err := json.Unmarshal(body, &dr); err != nil {
		debugf("需求接口 JSON 解析失败: %v", err)
		return 0, fmt.Errorf("解析 JSON 失败: %w", err)
	}
	debugf("需求接口解析结果: success=%v code=%q message=%q totalCount=%d (列表%d条, aiResponseCount=%d)",
		dr.Success, dr.Code, dr.Message, dr.Data.TotalCount, len(dr.Data.DemandList), dr.Data.AIResponseCount)
	if !dr.Success {
		debugf("需求接口返回失败标记: message=%q code=%q", dr.Message, dr.Code)
		return 0, fmt.Errorf("接口返回失败: %s", dr.Message)
	}
	return dr.Data.TotalCount, nil
}

// loadHistory 读取历史记录
func loadHistory() []record {
	debugf("读取历史文件: %s", historyFile)
	data, err := os.ReadFile(historyFile)
	if err != nil {
		debugf("历史文件不可用，按空历史处理: %v", err)
		return nil
	}
	debugf("历史文件读取成功: %d 字节", len(data))
	var hist []record
	if err := json.Unmarshal(data, &hist); err != nil {
		debugf("历史文件 JSON 解析失败，按空历史处理: %v", err)
		return nil
	}
	debugf("历史记录条数: %d", len(hist))
	if len(hist) > 0 {
		debugf("历史区间: %s ~ %s | 首条(寻源=%d, 触达=%d, 需求=%d) | 末条(寻源=%d, 触达=%d, 需求=%d)",
			hist[0].Timestamp, hist[len(hist)-1].Timestamp,
			hist[0].Search, hist[0].Contact, hist[0].Total,
			hist[len(hist)-1].Search, hist[len(hist)-1].Contact, hist[len(hist)-1].Total)
	}
	return hist
}

// saveHistory 保存历史记录
func saveHistory(hist []record) error {
	data, err := json.MarshalIndent(hist, "", "  ")
	if err != nil {
		debugf("历史记录序列化失败: %v", err)
		return err
	}
	debugf("写入历史文件: %s, 共 %d 条, %d 字节", historyFile, len(hist), len(data))
	if err := os.WriteFile(historyFile, data, 0644); err != nil {
		debugf("历史文件写入失败: %v", err)
		return err
	}
	debugf("历史文件写入成功")
	return nil
}

// csvHeader CSV 列定义（新增指标时在此追加，ensureCSVHeader 会自动升级旧文件）
var csvHeader = []string{
	"采集时间",
	"累计寻源次数", "当小时寻源增量",
	"累计触达次数", "当小时触达增量",
	"需求总量", "当小时需求增量",
}

// ensureCSVHeader 保证 CSV 表头包含全部列。
// 旧版本写入的 CSV 缺少"需求总量"两列，这里把首行替换为新表头、并为已有行补空值，
// 避免新写入的数据与表头列错位（文件为空或已是最新时不做任何处理）。
func ensureCSVHeader() error {
	f, err := os.Open(logFile)
	if err != nil {
		debugf("CSV 不存在，跳过表头升级: %v", err)
		return nil
	}
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // 旧文件列数少于新表头，关闭列数一致性校验
	rows, err := r.ReadAll()
	f.Close()
	if err != nil {
		debugf("读取 CSV 失败，跳过表头升级: %v", err)
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	if len(rows[0]) >= len(csvHeader) {
		return nil // 表头已是最新
	}
	debugf("升级 CSV 表头: %d 列 -> %d 列, 共 %d 行", len(rows[0]), len(csvHeader), len(rows))
	rows[0] = append([]string{}, csvHeader...)
	for i := 1; i < len(rows); i++ {
		for len(rows[i]) < len(csvHeader) {
			rows[i] = append(rows[i], "")
		}
	}
	out, err := os.Create(logFile)
	if err != nil {
		debugf("重写 CSV 失败: %v", err)
		return err
	}
	w := csv.NewWriter(out)
	if err := w.WriteAll(rows); err != nil {
		out.Close()
		debugf("重写 CSV 写入失败: %v", err)
		return err
	}
	w.Flush()
	if err := w.Error(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	debugf("CSV 表头升级完成")
	return nil
}

// appendLog 将本次采集及与上一小时的增量写入 CSV
func appendLog(cur record, prev *record) error {
	if err := ensureCSVHeader(); err != nil {
		debugf("CSV 表头升级失败: %v", err)
	}
	needHeader := false
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		needHeader = true
	}
	debugf("写入 CSV: %s (写表头=%v)", logFile, needHeader)

	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		debugf("打开 CSV 失败: %v", err)
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	if needHeader {
		if err := w.Write(csvHeader); err != nil {
			debugf("写入 CSV 表头失败: %v", err)
			return err
		}
		debugf("已写入 CSV 表头")
	}

	var searchDelta, contactDelta, totalDelta string
	if prev != nil {
		searchDelta = fmt.Sprintf("%d", cur.Search-prev.Search)
		contactDelta = fmt.Sprintf("%d", cur.Contact-prev.Contact)
		totalDelta = fmt.Sprintf("%d", cur.Total-prev.Total)
	} else {
		debugf("无上一条记录，增量列留空")
	}

	row := []string{
		cur.Timestamp,
		fmt.Sprintf("%d", cur.Search), searchDelta,
		fmt.Sprintf("%d", cur.Contact), contactDelta,
		fmt.Sprintf("%d", cur.Total), totalDelta,
	}
	debugf("CSV 行内容: %v", row)
	if err := w.Write(row); err != nil {
		debugf("写入 CSV 行失败: %v", err)
		return err
	}
	w.Flush()
	if err := w.Error(); err != nil {
		debugf("CSV 缓冲区刷新失败: %v", err)
		return err
	}
	debugf("CSV 写入完成")
	return nil
}

// collectOnce 执行一次采集
func collectOnce() {
	start := nowCST()
	setupLogger() // 确保日志文件按当天切分
	log.Printf("===== 开始第 %d 次采集 =====", collectCount+1)
	hist := loadHistory()
	var prev *record
	if len(hist) > 0 {
		p := hist[len(hist)-1]
		prev = &p
		debugf("上一条记录: 时间=%s 寻源=%d 触达=%d 需求=%d",
			p.Timestamp, p.Search, p.Contact, p.Total)
	} else {
		debugf("无历史记录，本次为首次采集")
	}

	cur, err := fetchStats()
	if err != nil {
		log.Printf("采集失败: %v", err)
		debugf("本次采集失败，跳过后续处理 (耗时 %v)", time.Since(start))
		return
	}

	// 需求总量：单独采集，失败不致命（沿用上一次的值，增量为 0）
	if dt, err := fetchDemandTotal(); err != nil {
		log.Printf("采集需求总量失败: %v（本次沿用上次值）", err)
		if prev != nil {
			cur.Total = prev.Total
		}
	} else {
		cur.Total = dt
		if prev != nil && dt < prev.Total {
			log.Printf("警告: 需求总量回退 %d -> %d，本次增量按 0 计", prev.Total, dt)
		}
	}

	hist = append(hist, *cur)
	debugf("历史记录数: %d -> %d", len(hist)-1, len(hist))
	if err := saveHistory(hist); err != nil {
		log.Printf("保存历史失败: %v", err)
	}
	if err := appendLog(*cur, prev); err != nil {
		log.Printf("写入日志失败: %v", err)
	}

	log.Printf("采集成功 [%s] 累计寻源=%d 累计触达=%d 需求总量=%d",
		cur.Timestamp, cur.Search, cur.Contact, cur.Total)
	if prev != nil {
		sd := cur.Search - prev.Search
		cd := cur.Contact - prev.Contact
		td := cur.Total - prev.Total
		log.Printf("  较上一小时增量: 寻源 %+d, 触达 %+d, 需求 %+d", sd, cd, td)
		if sd < 0 || cd < 0 || td < 0 {
			debugf("警告: 检测到负增量 (寻源=%d, 触达=%d, 需求=%d)，可能是接口数据被重置", sd, cd, td)
		}
	}
	collectCount++
	debugf("本次采集总耗时: %v", time.Since(start))
}

// barChart 生成条形图：value 相对 max 按比例填充 █，其余用 ░
func barChart(value, max int64, width int) string {
	filled := 0
	if max > 0 {
		filled = int(float64(value) / float64(max) * float64(width))
		if value > 0 && filled == 0 {
			filled = 1
		}
	}
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// metricCount 统计指标数量（寻源 / 触达 / 需求总量）
const metricCount = 3

// aggByPeriod 按周期聚合增量：match 决定记录是否计入，keyFunc 决定桶（小时/日期/月份）。
// 每个桶保存三个指标的增量：[0]=寻源 [1]=触达 [2]=需求总量
func aggByPeriod(hist []record, match func(record) bool, keyFunc func(record) string) map[string][metricCount]int64 {
	out := make(map[string][metricCount]int64)
	matched, clamped := 0, 0
	debugf("开始按周期聚合: 总记录=%d 条", len(hist))
	for i := 1; i < len(hist); i++ {
		r := hist[i]
		if !match(r) {
			continue
		}
		matched++
		s := r.Search - hist[i-1].Search
		c := r.Contact - hist[i-1].Contact
		t := r.Total - hist[i-1].Total
		if hist[i-1].Total == 0 && r.Total > 0 {
			// 上一条没有需求数据（旧历史文件或上次采集失败），无基准可比，增量记 0
			t = 0
		}
		if s < 0 {
			s = 0
			clamped++
		}
		if c < 0 {
			c = 0
			clamped++
		}
		if t < 0 {
			t = 0
			clamped++
		}
		k := keyFunc(r)
		v := out[k]
		v[0] += s
		v[1] += c
		v[2] += t
		out[k] = v
	}
	debugf("聚合完成: 命中=%d 条, 负增量钳制=%d 次, 桶数=%d", matched, clamped, len(out))
	return out
}

// renderBarSection 渲染一个指标的条形图区块（idx=0 寻源, idx=1 触达, idx=2 需求总量）。
// 即使该指标全时段都无增量，也照常画出整条空条形图（全为 ░、数值 0），便于肉眼核对"确实没变化"。
func renderBarSection(w *bufio.Writer, title, unit string, deltas map[string][metricCount]int64, keys, labels []string, idx int) {
	fmt.Fprintf(w, "【%s】(%s)\n", title, unit)
	max := int64(0)
	for _, k := range keys {
		if v := deltas[k][idx]; v > max {
			max = v
		}
	}
	for i, k := range keys {
		v := deltas[k][idx]
		flag := ""
		if v > 0 && v == max {
			flag = " ⚠️"
		}
		fmt.Fprintf(w, "  %s %s %10d%s\n", labels[i], barChart(v, max, 20), v, flag)
	}
	fmt.Fprintf(w, "\n")
}

// dayBaselineAndEnd 定位某天的"起始基准"与"期末"两条记录：
//   - 起始基准 = 该日 00:00 之前最后一条记录（正常情况下是前一日 23:00 的记录），
//     因此"期末 - 基准"覆盖的正是该日 0:00 ~ 23:59 的全部增量；
//   - 期末     = 该日最后一条记录（正常情况下是 23:00 的记录）。
//
// 若该日之前无任何历史数据，则退化为以该日首条记录为基准（增量自第 2 条起算）。
func dayBaselineAndEnd(hist []record, dayStr string) (base, end record, estimated, ok bool) {
	var lastBefore, firstOfDay *record
	for i := range hist {
		ts := hist[i].Timestamp
		if len(ts) < 10 {
			continue
		}
		switch d := ts[:10]; {
		case d == dayStr:
			if firstOfDay == nil {
				r := hist[i]
				firstOfDay = &r
			}
			end = hist[i]
		case d < dayStr:
			if lastBefore == nil || ts > lastBefore.Timestamp {
				r := hist[i]
				lastBefore = &r
			}
		}
	}
	if firstOfDay == nil {
		return record{}, record{}, false, false
	}
	if lastBefore != nil {
		base = *lastBefore
	} else {
		base = *firstOfDay
		estimated = true
	}
	return base, end, estimated, true
}

// buildDailySummary 生成日报的"当日汇总"区块，并一并返回当日总增量（寻源/触达/需求）
func buildDailySummary(hist []record, dayStr string) (summary string, ds, dc, dt int64, ok bool) {
	base, end, estimated, ok := dayBaselineAndEnd(hist, dayStr)
	if !ok {
		debugf("无法计算 %s 的当日汇总: 无该日记录", dayStr)
		return "", 0, 0, 0, false
	}
	ds = end.Search - base.Search
	dc = end.Contact - base.Contact
	dt = end.Total - base.Total
	debugf("当日汇总 [%s]: 基准=%s(寻源=%d 触达=%d 需求=%d) 期末=%s(寻源=%d 触达=%d 需求=%d) 原始增量 寻源=%+d 触达=%+d 需求=%+d (基准缺失=%v)",
		dayStr, base.Timestamp, base.Search, base.Contact, base.Total,
		end.Timestamp, end.Search, end.Contact, end.Total, ds, dc, dt, estimated)

	var b strings.Builder
	fmt.Fprintf(&b, "【当日汇总】(统计窗口 %s 00:00 ~ 23:59)\n", dayStr)
	fmt.Fprintf(&b, "  起始基准 : %s  寻源=%d  触达=%d  需求=%d\n",
		base.Timestamp, base.Search, base.Contact, base.Total)
	fmt.Fprintf(&b, "  期末值   : %s  寻源=%d  触达=%d  需求=%d\n",
		end.Timestamp, end.Search, end.Contact, end.Total)
	if estimated {
		fmt.Fprintf(&b, "  说明     : 无 %s 之前的采集数据，增量自当日首条记录起算\n", dayStr)
	}
	if ds < 0 || dc < 0 || dt < 0 {
		fmt.Fprintf(&b, "  警告     : 累计值回退(寻源 %+d, 触达 %+d, 需求 %+d)，疑似接口数据重置，增量按 0 计\n", ds, dc, dt)
		if ds < 0 {
			ds = 0
		}
		if dc < 0 {
			dc = 0
		}
		if dt < 0 {
			dt = 0
		}
	}
	fmt.Fprintf(&b, "  当日增量 : 寻源 %+d  触达 %+d  需求 %+d\n\n", ds, dc, dt)
	return b.String(), ds, dc, dt, true
}

// sumDeltas 汇总所有桶的增量（用于周/月/年报的"本期合计"）
func sumDeltas(deltas map[string][metricCount]int64, keys []string) (int64, int64, int64) {
	var s, c, t int64
	for _, k := range keys {
		s += deltas[k][0]
		c += deltas[k][1]
		t += deltas[k][2]
	}
	return s, c, t
}

// buildPeriodSummary 生成周/月/年报的"本期汇总"区块；avgLabel 为均值口径（日均/月均），
// 传空字符串则不输出均值行
func buildPeriodSummary(deltas map[string][metricCount]int64, keys []string, avgLabel string) string {
	s, c, t := sumDeltas(deltas, keys)
	n := int64(len(keys))
	var b strings.Builder
	fmt.Fprintf(&b, "【本期汇总】\n")
	fmt.Fprintf(&b, "  周期数   : %d\n", n)
	fmt.Fprintf(&b, "  合计增量 : 寻源 %+d  触达 %+d  需求 %+d\n", s, c, t)
	if avgLabel != "" && n > 0 {
		fmt.Fprintf(&b, "  %s增量 : 寻源 %+d  触达 %+d  需求 %+d\n", avgLabel, s/n, c/n, t/n)
	}
	fmt.Fprintf(&b, "\n")
	return b.String()
}

// renderReport 渲染通用报告并写入 ./reports；summary 为可选汇总区块（传空则不输出）。
// 返回渲染出的完整报告文本，便于调用方（如启动报告）直接打印到日志
func renderReport(title, fileName string, deltas map[string][metricCount]int64, keys, labels []string, summary string) (string, error) {
	var b strings.Builder
	w := bufio.NewWriter(&b)
	fmt.Fprintf(w, "==================================================\n")
	fmt.Fprintf(w, "  Insigmind %s\n", title)
	fmt.Fprintf(w, "==================================================\n\n")

	active := 0
	for _, k := range keys {
		if deltas[k][0] > 0 || deltas[k][1] > 0 || deltas[k][2] > 0 {
			active++
		}
	}
	fmt.Fprintf(w, "覆盖周期数(有增量): %d / %d\n\n", active, len(keys))
	debugf("渲染报告: 标题=%q 周期数=%d 有增量周期=%d", title, len(keys), active)
	if summary != "" {
		fmt.Fprint(w, summary)
	}

	renderBarSection(w, "累计寻源次数", "次", deltas, keys, labels, 0)
	renderBarSection(w, "累计触达次数", "次", deltas, keys, labels, 1)
	renderBarSection(w, "需求总量(totalCount)", "条", deltas, keys, labels, 2)
	w.Flush()

	content := b.String()
	outPath := filepath.Join(reportsDir, fileName)
	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		return "", fmt.Errorf("创建报告子目录失败: %w", err)
	}
	debugf("写入报告文件: %s (%d 字节)", outPath, len(content))
	if err := os.WriteFile(outPath, []byte(content), 0644); err != nil {
		debugf("报告文件写入失败: %s: %v", outPath, err)
		return "", err
	}
	debugf("报告文件写入成功: %s", outPath)
	return content, nil
}

// generateDailyReport 生成某天的日报（按小时）
func generateDailyReport(hist []record, targetDay string) (string, error) {
	day, err := parseInCST("2006-01-02", targetDay)
	if err != nil {
		return "", err
	}
	dayStr := day.Format("2006-01-02")
	debugf("生成日报: 目标日期=%s (星期%s)", dayStr, day.Weekday())
	hasData := false
	dayCount := 0
	for _, r := range hist {
		if len(r.Timestamp) >= 10 && r.Timestamp[:10] == dayStr {
			hasData = true
			dayCount++
		}
	}
	if !hasData {
		debugf("无 %s 的数据，跳过日报生成", targetDay)
		return "", fmt.Errorf("无 %s 的数据", targetDay)
	}
	debugf("%s 命中记录: %d 条", dayStr, dayCount)

	deltas := aggByPeriod(hist, func(r record) bool {
		return len(r.Timestamp) >= 10 && r.Timestamp[:10] == dayStr
	}, func(r record) string { return r.Timestamp[11:13] })

	keys := make([]string, 24)
	labels := make([]string, 24)
	for h := 0; h < 24; h++ {
		keys[h] = fmt.Sprintf("%02d", h)
		labels[h] = fmt.Sprintf("%s %02d:00", day.Format("01-02"), h)
	}
	summary, ds, dc, dt, ok := buildDailySummary(hist, dayStr)
	if ok {
		log.Printf("日报 [%s] 当日总增量: 寻源 %+d, 触达 %+d, 需求 %+d", dayStr, ds, dc, dt)
	}
	return renderReport(fmt.Sprintf("日活跃度报告  %s", dayStr), "daily/daily_"+targetDay+"_report.txt", deltas, keys, labels, summary)
}

// generateWeeklyReport 生成 ref 所在周的周报（周一~周日，按天）
func generateWeeklyReport(hist []record, ref time.Time) (string, error) {
	monday := ref.AddDate(0, 0, -int((int(ref.Weekday())+6)%7))
	sunday := monday.AddDate(0, 0, 6)
	startStr := monday.Format("2006-01-02")
	endStr := sunday.Format("2006-01-02")
	debugf("生成周报: 参考日=%s -> 周期 %s ~ %s", ref.Format("2006-01-02"), startStr, endStr)

	deltas := aggByPeriod(hist, func(r record) bool {
		return len(r.Timestamp) >= 10 && r.Timestamp[:10] >= startStr && r.Timestamp[:10] <= endStr
	}, func(r record) string { return r.Timestamp[:10] })

	weekdayNames := []string{"周一", "周二", "周三", "周四", "周五", "周六", "周日"}
	keys := make([]string, 7)
	labels := make([]string, 7)
	for i := 0; i < 7; i++ {
		d := monday.AddDate(0, 0, i)
		keys[i] = d.Format("2006-01-02")
		labels[i] = d.Format("01-02") + " " + weekdayNames[i]
	}
	return renderReport(
		fmt.Sprintf("周活跃度报告  %s ~ %s", startStr, endStr),
		fmt.Sprintf("week/week_%s_report.txt", startStr),
		deltas, keys, labels, buildPeriodSummary(deltas, keys, "日均"),
	)
}

// generateMonthlyReport 生成 ref 所在月份的月报（按天）
func generateMonthlyReport(hist []record, ref time.Time) (string, error) {
	ym := ref.Format("2006-01")
	debugf("生成月报: 目标月份=%s (参考日=%s)", ym, ref.Format("2006-01-02"))
	deltas := aggByPeriod(hist, func(r record) bool {
		return len(r.Timestamp) >= 7 && r.Timestamp[:7] == ym
	}, func(r record) string { return r.Timestamp[:10] })

	days := time.Date(ref.Year(), ref.Month()+1, 0, 0, 0, 0, 0, chinaLoc).Day()
	keys := make([]string, days)
	labels := make([]string, days)
	for i := 0; i < days; i++ {
		d := time.Date(ref.Year(), ref.Month(), i+1, 0, 0, 0, 0, chinaLoc)
		keys[i] = d.Format("2006-01-02")
		labels[i] = d.Format("01-02")
	}
	return renderReport(
		fmt.Sprintf("月活跃度报告  %s", ym),
		fmt.Sprintf("month/month_%s_report.txt", ym),
		deltas, keys, labels, buildPeriodSummary(deltas, keys, "日均"),
	)
}

// generateYearlyReport 生成 ref 所在年份的年报（按月）
func generateYearlyReport(hist []record, ref time.Time) (string, error) {
	y := ref.Format("2006")
	debugf("生成年报: 目标年份=%s (参考日=%s)", y, ref.Format("2006-01-02"))
	deltas := aggByPeriod(hist, func(r record) bool {
		return len(r.Timestamp) >= 4 && r.Timestamp[:4] == y
	}, func(r record) string { return r.Timestamp[:7] })

	keys := make([]string, 12)
	labels := make([]string, 12)
	for i := 0; i < 12; i++ {
		d := time.Date(ref.Year(), time.Month(i+1), 1, 0, 0, 0, 0, chinaLoc)
		keys[i] = d.Format("2006-01")
		labels[i] = d.Format("2006-01")
	}
	return renderReport(
		fmt.Sprintf("年活跃度报告  %s", y),
		fmt.Sprintf("year/year_%s_report.txt", y),
		deltas, keys, labels, buildPeriodSummary(deltas, keys, "月均"),
	)
}

// autoGeneratePeriodicReports 检测跨周/跨月/跨年，自动补生成对应报告
func autoGeneratePeriodicReports(hist []record, prevDay string) {
	day, err := parseInCST("2006-01-02", prevDay)
	if err != nil {
		debugf("周期检测跳过: 日期解析失败 %q: %v", prevDay, err)
		return
	}
	isSunday := day.Weekday() == time.Sunday
	isMonthEnd := day.AddDate(0, 0, 1).Month() != day.Month()
	isYearEnd := day.Month() == time.December && day.Day() == 31
	debugf("周期检测 [%s]: 星期%s | 是否周末=%v 是否月末=%v 是否年末=%v",
		prevDay, day.Weekday(), isSunday, isMonthEnd, isYearEnd)

	if isSunday {
		if _, err := generateWeeklyReport(hist, day); err != nil {
			log.Printf("生成周报失败: %v", err)
		} else {
			log.Printf("已生成周报: %s 所在周", prevDay)
		}
	} else {
		debugf("非周日，不生成周报")
	}
	if isMonthEnd {
		if _, err := generateMonthlyReport(hist, day); err != nil {
			log.Printf("生成月报失败: %v", err)
		} else {
			log.Printf("已生成月报: %s", day.Format("2006-01"))
		}
	} else {
		debugf("非月末，不生成月报")
	}
	if isYearEnd {
		if _, err := generateYearlyReport(hist, day); err != nil {
			log.Printf("生成年报失败: %v", err)
		} else {
			log.Printf("已生成年报: %s", day.Format("2006"))
		}
	} else {
		debugf("非年末，不生成年报")
	}
}

// generateReportsForYesterday 为"昨天"生成日报；若昨天为周日/月末/年末，
// 则同时补生成周报/月报/年报（每天早上 5:00 调用）
func generateReportsForYesterday() {
	yesterday := nowCST().AddDate(0, 0, -1).Format("2006-01-02")
	if lastReportDay == yesterday {
		debugf("今日 %s 的报告已在本次进程中生成过，跳过", yesterday)
		return
	}
	lastReportDay = yesterday
	log.Printf("===== 开始生成 %s 的报告 =====", yesterday)
	hist := loadHistory()
	if _, err := generateDailyReport(hist, yesterday); err != nil {
		log.Printf("生成日报失败 [%s]: %v", yesterday, err)
	} else {
		log.Printf("已生成日报: %s", yesterday)
	}
	// 前一天为周日/月末/年末时，补生成周报/月报/年报
	autoGeneratePeriodicReports(hist, yesterday)
}

// startupRefDay 确定启动报告的参考日期：
//   - 历史中有"昨天"的数据时，以昨天为准（与每日 5:00 的自动报告口径一致）；
//   - 否则（如设备久未联网、或程序首次运行）退化为最后一条记录所在日期，保证仍能出一份报告。
func startupRefDay(hist []record) string {
	yesterday := nowCST().AddDate(0, 0, -1).Format("2006-01-02")
	for _, r := range hist {
		if len(r.Timestamp) >= 10 && r.Timestamp[:10] == yesterday {
			return yesterday
		}
	}
	last := ""
	for i := len(hist) - 1; i >= 0; i-- {
		if len(hist[i].Timestamp) >= 10 {
			last = hist[i].Timestamp[:10]
			break
		}
	}
	if last == "" {
		return yesterday
	}
	debugf("启动报告: 昨天(%s)无数据，退化为最后有数据的日期 %s", yesterday, last)
	return last
}

// printReport 将报告完整内容逐行打印到日志（同时进入控制台与当天日志文件），
// 这样在 glean.log / logs/ 中就能直接看到报告，无需再打开 reports/ 下的文件
func printReport(kind, content string) {
	if content == "" {
		return
	}
	log.Printf("---------- %s 开始 ----------", kind)
	for _, line := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		log.Printf("%s", line)
	}
	log.Printf("---------- %s 结束 ----------", kind)
}

// generateStartupReports 启动后一次性生成日报/周报/月报/年报各一份，
// 文件落到 ./reports，同时把完整内容打印到日志
func generateStartupReports() {
	log.Printf("===== 开始生成启动报告（日报/周报/月报/年报） =====")
	hist := loadHistory()
	if len(hist) == 0 {
		log.Printf("启动报告跳过: 暂无历史数据")
		return
	}

	refStr := startupRefDay(hist)
	ref, err := parseInCST("2006-01-02", refStr)
	if err != nil {
		log.Printf("启动报告跳过: 参考日期解析失败 %q: %v", refStr, err)
		return
	}
	debugf("启动报告参考日期: %s (昨天=%s)", refStr, nowCST().AddDate(0, 0, -1).Format("2006-01-02"))

	items := []struct {
		kind string
		fn   func() (string, error)
	}{
		{"日报", func() (string, error) { return generateDailyReport(hist, refStr) }},
		{"周报", func() (string, error) { return generateWeeklyReport(hist, ref) }},
		{"月报", func() (string, error) { return generateMonthlyReport(hist, ref) }},
		{"年报", func() (string, error) { return generateYearlyReport(hist, ref) }},
	}
	for _, it := range items {
		content, err := it.fn()
		if err != nil {
			log.Printf("生成启动%s失败: %v", it.kind, err)
			continue
		}
		log.Printf("已生成启动%s (参考日 %s)", it.kind, refStr)
		printReport(it.kind, content)
	}

	// 日报已基于 refStr 生成过，避免 5:00 时段的调度对同一天重复生成
	if refStr == nowCST().AddDate(0, 0, -1).Format("2006-01-02") {
		lastReportDay = refStr
	}
	log.Printf("===== 启动报告生成完毕 =====")
}

// runHourly 每小时对齐到整点执行一次（所有时刻判定均基于东八区）
func runHourly() {
	// 启动时刻若正处于 5 点时段（东八区 5:00~5:59），先补生成昨日报告，避免错过
	if nowCST().Hour() == reportHour {
		generateReportsForYesterday()
	}

	// 先对齐到下一个整点
	now := nowCST()
	next := now.Add(time.Hour).Truncate(time.Hour)
	wait := time.Until(next)
	log.Printf("下次执行时间: %s", next.Format("2006-01-02 15:04:05"))
	debugf("对齐整点: 当前=%s 下次=%s 等待=%v",
		now.Format("2006-01-02 15:04:05"), next.Format("2006-01-02 15:04:05"), wait)
	time.Sleep(wait)

	collectOnce()
	// 对齐到整点后若恰为东八区 5 点（如 4:59 启动），补生成昨日报告
	if nowCST().Hour() == reportHour {
		generateReportsForYesterday()
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	debugf("已进入每小时定时循环 (间隔=1h)")
	for t := range ticker.C {
		tc := t.In(chinaLoc) // ticker 返回本地时间，显式转为东八区
		debugf("定时器触发: %s", tc.Format("2006-01-02 15:04:05"))
		collectOnce()
		// 每天早上东八区 5:00 整点，为前一天生成日报及周期报告
		if tc.Hour() == reportHour {
			generateReportsForYesterday()
		}
	}
}

func main() {
	once := flag.Bool("once", false, "只采集一次后退出（用于测试或配合外部计划任务）")
	reportDay := flag.String("report", "", "手动生成指定日期(YYYY-MM-DD)的日报，基于本地历史数据")
	weekDay := flag.String("week", "", "手动生成指定日期所在周的周报 (YYYY-MM-DD)")
	monthStr := flag.String("month", "", "手动生成指定月份的月报 (YYYY-MM)")
	yearStr := flag.String("year", "", "手动生成指定年份的年报 (YYYY)")
	showVersion := flag.Bool("version", false, "打印程序版本号后退出")
	startupReport := flag.Bool("startup-report", true, "启动时立即生成日报/周报/月报/年报各一份，并打印到日志")
	debugFlag := flag.Bool("debug", true, "开启调试日志（打印请求/响应/文件读写等详细信息）")
	flag.Parse()
	debugEnabled = *debugFlag

	log.SetFlags(0) // 时间戳由 cstLogWriter 以东八区输出，避免 log 包使用系统本地时区
	if *showVersion {
		logVersion()
		return
	}
	log.Printf("启动 insigmind 统计采集")
	logVersion()
	debugf("调试模式已开启")
	debugf("%s: %s", versionKey, version)
	debugf("命令行参数: once=%v report=%q week=%q month=%q year=%q startup-report=%v debug=%v",
		*once, *reportDay, *weekDay, *monthStr, *yearStr, *startupReport, *debugFlag)
	debugf("运行时间: %s", nowCST().Format("2006-01-02 15:04:05"))
	debugf("接口地址: %s", apiURL)

	if *reportDay != "" {
		debugf("模式: 手动生成日报 (%s)", *reportDay)
		hist := loadHistory()
		content, err := generateDailyReport(hist, *reportDay)
		if err != nil {
			log.Fatalf("生成报告失败: %v", err)
		}
		log.Printf("已生成日报: %s", *reportDay)
		printReport("日报", content)
		return
	}

	if *weekDay != "" {
		debugf("模式: 手动生成周报 (%s)", *weekDay)
		t, err := parseInCST("2006-01-02", *weekDay)
		if err != nil {
			log.Fatalf("日期格式错误: %v", err)
		}
		hist := loadHistory()
		content, err := generateWeeklyReport(hist, t)
		if err != nil {
			log.Fatalf("生成周报失败: %v", err)
		}
		log.Printf("已生成周报: %s 所在周", *weekDay)
		printReport("周报", content)
		return
	}

	if *monthStr != "" {
		debugf("模式: 手动生成月报 (%s)", *monthStr)
		t, err := parseInCST("2006-01", *monthStr)
		if err != nil {
			log.Fatalf("月份格式错误: %v", err)
		}
		hist := loadHistory()
		content, err := generateMonthlyReport(hist, t)
		if err != nil {
			log.Fatalf("生成月报失败: %v", err)
		}
		log.Printf("已生成月报: %s", *monthStr)
		printReport("月报", content)
		return
	}

	if *yearStr != "" {
		debugf("模式: 手动生成年报 (%s)", *yearStr)
		t, err := parseInCST("2006", *yearStr)
		if err != nil {
			log.Fatalf("年份格式错误: %v", err)
		}
		hist := loadHistory()
		content, err := generateYearlyReport(hist, t)
		if err != nil {
			log.Fatalf("生成年报失败: %v", err)
		}
		log.Printf("已生成年报: %s", *yearStr)
		printReport("年报", content)
		return
	}

	if *once {
		debugf("模式: 单次采集")
		collectOnce()
		debugf("单次采集结束，程序退出")
		return
	}

	// 立即执行一次，然后进入每小时循环
	debugf("模式: 常驻运行（立即采集一次 + 每小时整点）")
	collectOnce()
	// 启动即出报告：日报/周报/月报/年报各一份（先采集，保证报告含最新数据）
	if *startupReport {
		generateStartupReports()
	} else {
		debugf("已通过 -startup-report=false 关闭启动报告")
	}
	runHourly()
}