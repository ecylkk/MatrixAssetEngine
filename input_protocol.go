package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// errStateNotFound is a sentinel error returned by LoadPersistentState when
// the persistent state file does not yet exist (first ever run).
// Callers can use errors.Is(err, errStateNotFound) to distinguish "missing"
// (safe to start fresh) from "corrupted" (must abort — PATCH WR-03).
var errStateNotFound = errors.New("persistent state file not found")

// OperatorInput 是首席操作员输入协议的正式结构体
// 所有运行期参数必须通过此协议注入，禁止硬编码
type OperatorInput struct {
	SystemAwakeDate   string             `json:"System_Awake_Date"`   // UTC+8 格式：2026-05-23
	TargetInflowRMB   float64            `json:"Target_Inflow_RMB"`   // 当期注资
	SGOVBalanceRMB    float64            `json:"SGOV_Balance_RMB"`    // 水桶上期历史结余总市值
	CurrentWeights    map[string]float64 `json:"W_current"`           // 风险资产当前真实存量总市值占比快照
	TargetWeights     map[string]float64 `json:"W_target"`            // 50年战略不动摇理想比例
	HeavisideFloorRMB float64            `json:"Heaviside_Floor_RMB"` // 最小订单金额阈值（默认150）
}

// PersistentState 是系统持久化状态快照
// 每次运行结束后写入，下次运行时作为输入协议的一部分
type PersistentState struct {
	LastRunDate    string             `json:"last_run_date"`    // 上次运行日期
	SGOVBalanceRMB float64            `json:"sgov_balance_rmb"` // 最新水桶余额
	CurrentWeights map[string]float64 `json:"current_weights"`  // 最新权重快照
	ExecutionLog   []ExecutionRecord  `json:"execution_log"`    // 最近10次执行记录
}

// ExecutionRecord 记录单次执行的关键参数
type ExecutionRecord struct {
	Date            string             `json:"date"`
	DeltaMacro      float64            `json:"delta_macro"`
	MarketRegime    string             `json:"market_regime"`
	WSGOVPercent    float64            `json:"w_sgov_percent"`
	PhiPercent      float64            `json:"phi_percent"`
	TotalBullet     float64            `json:"total_bullet"`
	SGOVBalance     float64            `json:"sgov_balance"`
	Allocations     map[string]float64 `json:"allocations"`
}

// LoadOperatorInput 从 JSON 文件加载操作员输入协议
// 文件路径：./matrix_operator_input.json
func LoadOperatorInput(path string) (*OperatorInput, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read operator input file: %w", err)
	}

	var input OperatorInput
	if err := json.Unmarshal(data, &input); err != nil {
		return nil, fmt.Errorf("failed to parse operator input JSON: %w", err)
	}

	// 验证必填字段
	if input.SystemAwakeDate == "" {
		return nil, fmt.Errorf("System_Awake_Date is required")
	}
	if input.TargetInflowRMB < 0 {
		return nil, fmt.Errorf("Target_Inflow_RMB must be non-negative")
	}
	if input.SGOVBalanceRMB < 0 {
		return nil, fmt.Errorf("SGOV_Balance_RMB must be non-negative")
	}
	if len(input.TargetWeights) == 0 {
		return nil, fmt.Errorf("W_target must not be empty")
	}
	if len(input.CurrentWeights) == 0 {
		return nil, fmt.Errorf("W_current must not be empty")
	}

	// 设置默认值
	if input.HeavisideFloorRMB == 0 {
		input.HeavisideFloorRMB = 150.0
	}

	return &input, nil
}

// ParseAwakeDate 解析 UTC+8 日期字符串为 time.Time
func (input *OperatorInput) ParseAwakeDate() (time.Time, error) {
	loc := time.FixedZone("UTC+8", 8*60*60)
	t, err := time.ParseInLocation("2006-01-02", input.SystemAwakeDate, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid System_Awake_Date format (expected YYYY-MM-DD): %w", err)
	}
	return t, nil
}

// SavePersistentState 保存持久化状态到 JSON 文件
// 文件路径：./matrix_persistent_state.json
func SavePersistentState(path string, state *PersistentState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal persistent state: %w", err)
	}

	tmpFile, err := os.CreateTemp(".", ".persistent-state-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create persistent state temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	cleanup := func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}

	if _, err := tmpFile.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("failed to write persistent state temp file: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("failed to sync persistent state temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to close persistent state temp file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to atomically replace persistent state file: %w", err)
	}

	return nil
}

// LoadPersistentState 加载持久化状态
//
// PATCH WR-03: Returns a wrapped errStateNotFound when the file does not
// exist (first run — safe to continue with empty state).  Any other error
// (permission denied, corrupted JSON, …) is returned unwrapped so the caller
// can detect it via errors.Is and abort rather than silently resetting state,
// which would bypass the idempotency guard.
func LoadPersistentState(path string) (*PersistentState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// First ever run — surface sentinel so caller starts fresh safely.
			return nil, errStateNotFound
		}
		return nil, fmt.Errorf("failed to read persistent state file: %w", err)
	}

	var state PersistentState
	if err := json.Unmarshal(data, &state); err != nil {
		// JSON parse failure — file is corrupted, not missing.
		// Return error directly (NOT wrapped in errStateNotFound) so caller
		// treats this as fatal rather than a safe first-run reset.
		return nil, fmt.Errorf("failed to parse persistent state JSON: %w", err)
	}

	return &state, nil
}

// CreateDefaultOperatorInput 创建默认的操作员输入文件（首次运行辅助）
//
// PATCH WR-07: Uses the same atomic temp→sync→close→rename pattern as
// SavePersistentState.  Direct os.WriteFile would produce a half-written
// JSON file if the disk fills during the write, causing a fatal parse error
// on the next startup.
func CreateDefaultOperatorInput(path string) error {
	defaultInput := OperatorInput{
		SystemAwakeDate:   time.Now().Format("2006-01-02"),
		TargetInflowRMB:   7000.00,
		SGOVBalanceRMB:    0.00,
		HeavisideFloorRMB: 150.00,
		CurrentWeights: map[string]float64{
			"SMH":  0.0,
			"QQQM": 0.0,
			"ORBX": 0.0,
			"URA":  0.0,
		},
		TargetWeights: map[string]float64{
			"SMH":  0.30,
			"QQQM": 0.40,
			"ORBX": 0.15,
			"URA":  0.15,
		},
	}

	data, err := json.MarshalIndent(defaultInput, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal default input: %w", err)
	}

	// Atomic write: temp file in the same directory so os.Rename is atomic.
	tmpFile, err := os.CreateTemp(".", ".operator-input-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file for operator input: %w", err)
	}
	tmpPath := tmpFile.Name()

	cleanup := func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}

	if _, err := tmpFile.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("failed to write operator input temp file: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("failed to sync operator input temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to close operator input temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to atomically replace operator input file: %w", err)
	}

	return nil
}
