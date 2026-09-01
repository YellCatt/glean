package main

import (
	"bufio"
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

const apiURL = "https://api.insigmind.com/Api/WebLogin/GetIndexStats"

// 字段中文名映射
var fieldNames = map[string]string{
	"searchCount":  "累计寻源次数",
	"contactCount": "累计触达次数",
}

// API 返回结构
type apiResponse struct {
	Success bool `json:"success"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    struct {
		SearchCount  int64 `json:"searchCount"`
		ContactCount int64 `json:"contactCount"`
	} `json:"data"`
}

// 一条历史记录
type record struct {
	Timestamp string `json:"timestamp"`
	Search    int64  `json:"searchCount"`
	Contact   int64  `json:"contactCount"`
}

var (
	historyFile string
	logFile     string
	reportsDir  string
	logsDir     string
)

func init() {
	dir, _ := os.Getwd()
	historyFile = filepath.Join(dir, "stats_history.json")
	logFile = filepath.Join(dir, "stats_log.csv")
	reportsDir = filepath.Join(dir, "reports")
	logsDir = filepath.Join(dir, "logs")
	_ = os.MkdirAll(reportsDir, 0755)
	_ = os.MkdirAll(logsDir, 0755)
	setupLogger()
}

// setupLogger 将日志同时输出到 stdout 和 ./logs/glean_YYYY-MM-DD.log
func setupLogger() {
	today := time.Now().Format("2006-01-02")
	logPath := filepath.Join(logsDir, "glean_"+today+".log")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.SetOutput(os.Stdout)
		log.Printf("警告: 无法打开日志文件 %s: %v，仅输出到控制台", logPath, err)
		return
	}
	log.SetOutput(io.MultiWriter(os.Stdout, f))
}

// fetchStats 请求接口并解析需要的字段
func fetchStats() (*record, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	var ar apiResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("解析 JSON 失败: %w", err)
	}
	if !ar.Success {
		return nil, fmt.Errorf("接口返回失败: %s", ar.Message)
	}

	return &record{
		Timestamp: time.Now().Format("2006-01-02 15:04:05"),
		Search:    ar.Data.SearchCount,
		Contact:   ar.Data.ContactCount,
	}, nil
}

// loadHistory 读取历史记录
func loadHistory() []record {
	data, err := os.ReadFile(historyFile)
	if err != nil {
		return nil
	}
	var hist []record
	if err := json.Unmarshal(data, &hist); err != nil {
		return nil
	}
	return hist
}

// saveHistory 保存历史记录
func saveHistory(hist []record) error {
	data, err := json.MarshalIndent(hist, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(historyFile, data, 0644)
}

// appendLog 将本次采集及与上一小时的增量写入 CSV
func appendLog(cur record, prev *record) error {
	needHeader := false
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		needHeader = true
	}

	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	if needHeader {
		if err := w.Write([]string{
			"采集时间",
			"累计寻源次数", "当小时寻源增量",
			"累计触达次数", "当小时触达增量",
		}); err != nil {
			return err
		}
	}

	var searchDelta, contactDelta string
	if prev != nil {
		searchDelta = fmt.Sprintf("%d", cur.Search-prev.Search)
		contactDelta = fmt.Sprintf("%d", cur.Contact-prev.Contact)
	}

	return w.Write([]string{
		cur.Timestamp,
		fmt.Sprintf("%d", cur.Search), searchDelta,
		fmt.Sprintf("%d", cur.Contact), contactDelta,
	})
}

// collectOnce 执行一次采集
func collectOnce() {
	hist := loadHistory()
	var prev *record
	if len(hist) > 0 {
		p := hist[len(hist)-1]
		prev = &p
	}

	cur, err := fetchStats()
	if err != nil {
		log.Printf("采集失败: %v", err)
		return
	}

	hist = append(hist, *cur)
	if err := saveHistory(hist); err != nil {
		log.Printf("保存历史失败: %v", err)
	}
	if err := appendLog(*cur, prev); err != nil {
		log.Printf("写入日志失败: %v", err)
	}

	log.Printf("采集成功 [%s] 累计寻源=%d 累计触达=%d", cur.Timestamp, cur.Search, cur.Contact)
	if prev != nil {
		log.Printf("  较上一小时增量: 寻源 +%d, 触达 +%d", cur.Search-prev.Search, cur.Contact-prev.Contact)
	}

	// 跨天检测：若当天日期与上一条记录不同，则为"上一天"生成日报告
	curDay := cur.Timestamp[:10]
	if prev != nil {
		prevDay := prev.Timestamp[:10]
		if prevDay != curDay {
			setupLogger() // 切换到新一天的日志文件
			if err := generateDailyReport(hist, prevDay); err != nil {
				log.Printf("生成日报告失败 [%s]: %v", prevDay, err)
			} else {
				log.Printf("已生成日报告: %s", prevDay)
			}
		}
	}
}

// generateDailyReport 基于历史中为 targetDay(YYYY-MM-DD) 的记录生成日报告
func generateDailyReport(hist []record, targetDay string) error {
	dayRecs := make([]record, 0)
	for _, r := range hist {
		if len(r.Timestamp) >= 10 && r.Timestamp[:10] == targetDay {
			dayRecs = append(dayRecs, r)
		}
	}
	if len(dayRecs) == 0 {
		return fmt.Errorf("无 %s 的数据", targetDay)
	}

	first := dayRecs[0]
	last := dayRecs[len(dayRecs)-1]

	// 按小时聚合增量
	var totalSearchDelta, totalContactDelta int64
	var activeHours int
	var peakSearchHour, peakContactHour string
	var peakSearchVal, peakContactVal int64 = -1, -1

	for i := 1; i < len(dayRecs); i++ {
		sd := dayRecs[i].Search - dayRecs[i-1].Search
		cd := dayRecs[i].Contact - dayRecs[i-1].Contact
		totalSearchDelta += sd
		totalContactDelta += cd
		if sd > 0 || cd > 0 {
			activeHours++
		}
		hr := dayRecs[i].Timestamp[11:13]
		if sd > peakSearchVal {
			peakSearchVal = sd
			peakSearchHour = hr
		}
		if cd > peakContactVal {
			peakContactVal = cd
			peakContactHour = hr
		}
	}

	var b strings.Builder
	w := bufio.NewWriter(&b)
	fmt.Fprintf(w, "==================================================\n")
	fmt.Fprintf(w, "       Insigmind 每日活跃度报告  %s\n", targetDay)
	fmt.Fprintf(w, "==================================================\n\n")
	fmt.Fprintf(w, "采集点数: %d 次\n", len(dayRecs))
	fmt.Fprintf(w, "活跃小时数(有增量): %d 小时\n\n", activeHours)

	fmt.Fprintf(w, "【累计寻源次数】\n")
	fmt.Fprintf(w, "  日初累计: %d\n", first.Search)
	fmt.Fprintf(w, "  日末累计: %d\n", last.Search)
	fmt.Fprintf(w, "  当日新增: %+d\n", totalSearchDelta)
	fmt.Fprintf(w, "  峰值小时: %s 时 (+%d)\n\n", peakSearchHour, peakSearchVal)

	fmt.Fprintf(w, "【累计触达次数】\n")
	fmt.Fprintf(w, "  日初累计: %d\n", first.Contact)
	fmt.Fprintf(w, "  日末累计: %d\n", last.Contact)
	fmt.Fprintf(w, "  当日新增: %+d\n", totalContactDelta)
	fmt.Fprintf(w, "  峰值小时: %s 时 (+%d)\n\n", peakContactHour, peakContactVal)

	fmt.Fprintf(w, "生成时间: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	w.Flush()

	reportPath := filepath.Join(reportsDir, targetDay+"_report.txt")
	return os.WriteFile(reportPath, []byte(b.String()), 0644)
}

// runHourly 每小时对齐到整点执行一次
func runHourly() {
	// 先对齐到下一个整点
	now := time.Now()
	next := now.Add(time.Hour).Truncate(time.Hour)
	log.Printf("下次执行时间: %s", next.Format("2006-01-02 15:04:05"))
	time.Sleep(time.Until(next))

	collectOnce()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		collectOnce()
	}
}

func main() {
	once := flag.Bool("once", false, "只采集一次后退出（用于测试或配合外部计划任务）")
	reportDay := flag.String("report", "", "手动生成指定日期(YYYY-MM-DD)的日报告，基于本地历史数据")
	flag.Parse()

	log.SetFlags(log.LstdFlags)
	log.Printf("启动 insigmind 统计采集")

	if *reportDay != "" {
		hist := loadHistory()
		if err := generateDailyReport(hist, *reportDay); err != nil {
			log.Fatalf("生成报告失败: %v", err)
		}
		log.Printf("已生成日报告: %s", *reportDay)
		return
	}

	if *once {
		collectOnce()
		return
	}

	// 立即执行一次，然后进入每小时循环
	collectOnce()
	runHourly()
}
