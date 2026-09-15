package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type AlertState struct {
	LastSuccessTime string `json:"last_success_time"`
	Consecutive404  int    `json:"consecutive_404"`
	AlertSent       bool   `json:"alert_sent"`
	LastAlertTime   string `json:"last_alert_time"`
}

var alertStateFile string

func initAlertState() {
	alertStateFile = filepath.Join(workDir, "alert_state.json")
}

func loadAlertState() AlertState {
	data, err := os.ReadFile(alertStateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			debugf("读取告警状态文件失败，使用空状态: %v", err)
		}
		return AlertState{}
	}
	var s AlertState
	if err := json.Unmarshal(data, &s); err != nil {
		debugf("告警状态文件解析失败，使用空状态: %v", err)
		return AlertState{}
	}
	return s
}

func saveAlertState(s AlertState) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		debugf("告警状态序列化失败: %v", err)
		return
	}
	if err := os.WriteFile(alertStateFile, data, 0644); err != nil {
		debugf("告警状态写入失败: %v", err)
		return
	}
	debugf("告警状态已保存: consecutive_404=%d alert_sent=%v", s.Consecutive404, s.AlertSent)
}

func alertEnabled() bool {
	if appConfig != nil {
		return appConfig.Alert.Enabled
	}
	return DefaultAlertConfig().Enabled
}

func alertThreshold() int {
	if appConfig != nil && appConfig.Alert.Consecutive404Threshold > 0 {
		return appConfig.Alert.Consecutive404Threshold
	}
	return DefaultAlertConfig().Consecutive404Threshold
}

func alertNotifyRecovery() bool {
	if appConfig != nil {
		return appConfig.Alert.NotifyRecovery
	}
	return DefaultAlertConfig().NotifyRecovery
}

const statusCodeNotFound = 404
const statusCodeRequestFailed = 0

func checkAndUpdateAlert(mainStatusCode int, mainErr error) {
	initAlertState()
	state := loadAlertState()
	threshold := alertThreshold()

	var was404 bool
	if mainErr == nil && mainStatusCode == statusCodeNotFound {
		was404 = true
	} else if mainErr != nil {
		errStr := mainErr.Error()
		if mainStatusCode == statusCodeNotFound {
			was404 = true
		} else if mainStatusCode == statusCodeRequestFailed {
			debugf("主接口请求完全失败 (status=%d): %v，不计入 404 连续计数", mainStatusCode, mainErr)
		} else {
			debugf("主接口返回错误 (status=%d, err=%s): %v，不计入 404 连续计数", mainStatusCode, errStr, mainErr)
		}
	}

	if was404 {
		state.Consecutive404++
		debugf("主接口返回 HTTP 404，连续 404 次数: %d / %d", state.Consecutive404, threshold)

		if state.Consecutive404 >= threshold && !state.AlertSent {
			subject := loadMailConfig().WithPrefix(fmt.Sprintf("⚠ 接口连续 %d 次返回 404（约 %.1f 天）", state.Consecutive404, float64(state.Consecutive404)/24.0))
			body := fmt.Sprintf(
				"监测到接口持续返回 HTTP 404，已连续 %d 次（约 %.1f 天，按每小时一次采集计算）。\n\n"+
					"接口地址: %s\n"+
					"触发阈值: 连续 %d 次（约 %.1f 天）\n"+
					"告警时刻: %s\n\n"+
					"请及时排查接口是否仍正常运行。\n",
				state.Consecutive404, float64(state.Consecutive404)/24.0,
				apiURL,
				threshold, float64(threshold)/24.0,
				nowCST().Format("2006-01-02 15:04:05"))
			log.Printf("⚠ 接口告警: 连续 %d 次返回 HTTP 404，发送告警邮件...", state.Consecutive404)
			if err := SendMail(subject, body); err != nil {
				log.Printf("告警邮件发送失败: %v", err)
			} else {
				log.Printf("告警邮件已发送")
			}
			state.AlertSent = true
			state.LastAlertTime = nowCST().Format("2006-01-02 15:04:05")
		}
	} else if mainErr == nil {
		if state.AlertSent && alertNotifyRecovery() {
			subject := loadMailConfig().WithPrefix(fmt.Sprintf("✓ 接口恢复正常（此前连续 %d 次 404）", state.Consecutive404))
			body := fmt.Sprintf(
				"接口已恢复正常访问。\n\n"+
					"此前连续 %d 次返回 HTTP 404，现已恢复。\n"+
					"恢复时刻: %s\n",
				state.Consecutive404,
				nowCST().Format("2006-01-02 15:04:05"))
			log.Printf("✓ 接口恢复正常，发送恢复通知邮件...")
			if err := SendMail(subject, body); err != nil {
				log.Printf("恢复通知邮件发送失败: %v", err)
			} else {
				log.Printf("恢复通知邮件已发送")
			}
		}
		if state.Consecutive404 > 0 {
			debugf("主接口恢复正常，重置连续 404 计数 %d -> 0", state.Consecutive404)
		}
		state.Consecutive404 = 0
		state.AlertSent = false
		state.LastSuccessTime = nowCST().Format("2006-01-02 15:04:05")
	}

	saveAlertState(state)
}