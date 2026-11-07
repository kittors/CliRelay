package quota

import (
	"errors"
	"fmt"
	"math"
)

// Metric 是配额的度量维度。账号、Key、权限模板与按模型规则共用同一套度量，
// 面板与准入判定因此只有一种「矩阵」心智模型。
type Metric string

const (
	// MetricCost 是美元花费，口径与 usage 记账的 cost_total 一致。
	MetricCost Metric = "cost"
	// MetricRequests 是请求次数，口径与 usage 的 request_count 一致（含失败请求，
	// 与原有「每日请求限额」相同：一次被上游拒绝的调用同样占用了一次配额）。
	MetricRequests Metric = "requests"
	// MetricTokens 是 Token 总量，口径与 usage 的 total_tokens 一致。
	MetricTokens Metric = "tokens"
)

// OrderedMetrics 决定矩阵在校验错误、准入判定与面板中的固定顺序。
var OrderedMetrics = [...]Metric{MetricCost, MetricRequests, MetricTokens}

// OrderedWindows 是矩阵的全部窗口：四个周期窗口加累计。累计没有窗口可滚动，
// 但它和周期窗口一样是「某段时间内的用量上限」，放进同一矩阵才能统一表达
// 「Opus 累计最多 100 万 Token」这类规则。
var OrderedWindows = [...]Period{PeriodFiveHour, PeriodDay, PeriodWeek, PeriodMonth, PeriodLifetime}

var (
	ErrInvalidMetric     = errors.New("invalid quota metric")
	ErrInvalidQuotaValue = errors.New("invalid quota value")
)

// ValidMetric 报告 metric 是否是已知度量。
func ValidMetric(metric Metric) bool {
	switch metric {
	case MetricCost, MetricRequests, MetricTokens:
		return true
	default:
		return false
	}
}

// ValidWindow 报告 window 是否是矩阵窗口。
func ValidWindow(window Period) bool {
	switch window {
	case PeriodFiveHour, PeriodDay, PeriodWeek, PeriodMonth, PeriodLifetime:
		return true
	default:
		return false
	}
}

// WindowValues 是一个度量在五个窗口上的取值。作为限额时 0 表示不限制；
// 作为用量时就是该窗口内的用量。
type WindowValues struct {
	FiveHour float64 `json:"5h"`
	Day      float64 `json:"day"`
	Week     float64 `json:"week"`
	Month    float64 `json:"month"`
	Lifetime float64 `json:"lifetime"`
}

// Value 返回 window 上的取值，未知窗口返回 0。
func (w WindowValues) Value(window Period) float64 {
	switch window {
	case PeriodFiveHour:
		return w.FiveHour
	case PeriodDay:
		return w.Day
	case PeriodWeek:
		return w.Week
	case PeriodMonth:
		return w.Month
	case PeriodLifetime:
		return w.Lifetime
	default:
		return 0
	}
}

func (w *WindowValues) set(window Period, value float64) {
	switch window {
	case PeriodFiveHour:
		w.FiveHour = value
	case PeriodDay:
		w.Day = value
	case PeriodWeek:
		w.Week = value
	case PeriodMonth:
		w.Month = value
	case PeriodLifetime:
		w.Lifetime = value
	}
}

// Matrix 是「度量 × 窗口」的完整取值表。
type Matrix struct {
	Cost     WindowValues `json:"cost"`
	Requests WindowValues `json:"requests"`
	Tokens   WindowValues `json:"tokens"`
}

func (m *Matrix) row(metric Metric) *WindowValues {
	switch metric {
	case MetricCost:
		return &m.Cost
	case MetricRequests:
		return &m.Requests
	case MetricTokens:
		return &m.Tokens
	default:
		return nil
	}
}

// Value 返回某一格的取值，未知度量或窗口返回 0。
func (m Matrix) Value(metric Metric, window Period) float64 {
	row := m.row(metric)
	if row == nil {
		return 0
	}
	return row.Value(window)
}

// Set 写入某一格；未知度量或窗口被忽略。
func (m *Matrix) Set(metric Metric, window Period, value float64) {
	if row := m.row(metric); row != nil {
		row.set(window, value)
	}
}

// Cell 是矩阵中的一格。
type Cell struct {
	Metric Metric  `json:"metric"`
	Window Period  `json:"window"`
	Value  float64 `json:"value"`
}

// ConfiguredCells 按固定顺序返回所有大于 0 的格子，即真正生效的限额。
func (m Matrix) ConfiguredCells() []Cell {
	out := make([]Cell, 0, 4)
	for _, metric := range OrderedMetrics {
		for _, window := range OrderedWindows {
			if value := m.Value(metric, window); value > 0 {
				out = append(out, Cell{Metric: metric, Window: window, Value: value})
			}
		}
	}
	return out
}

// IsZero 报告矩阵是否没有任何生效限额。
func (m Matrix) IsZero() bool {
	for _, metric := range OrderedMetrics {
		for _, window := range OrderedWindows {
			if m.Value(metric, window) != 0 {
				return false
			}
		}
	}
	return true
}

// Add 返回逐格相加的结果，用于把多个模型的用量汇总到一条规则上。
func (m Matrix) Add(other Matrix) Matrix {
	out := m
	for _, metric := range OrderedMetrics {
		for _, window := range OrderedWindows {
			out.Set(metric, window, m.Value(metric, window)+other.Value(metric, window))
		}
	}
	return out
}

// NormalizeQuotaValue 规范化单格限额：负数、NaN、Inf 拒绝；0 表示不限制。
//
// 美元沿用原有「整美元」语义（正小数向上取整），与周期额度、累计额度的既有
// 输入行为一致，避免同一个弹窗里两种金额精度。请求次数与 Token 只能是整数，
// 小数同样向上取整：向下取整会让管理员填的上限悄悄变小。
func NormalizeQuotaValue(metric Metric, value float64) (float64, error) {
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("%w: %s must be a finite non-negative number", ErrInvalidQuotaValue, metric)
	}
	if value == 0 {
		return 0, nil
	}
	// 上限超过 2^53 时 float64 已无法精确表示整数，也远超任何真实用量。请求次数的
	// 每日/累计两格落在 INTEGER 列上，统一以 int32 为界，避免写库时溢出。
	limit := maxQuotaValue
	if metric == MetricRequests {
		limit = maxRequestQuotaValue
	}
	if value > limit {
		return 0, fmt.Errorf("%w: %s exceeds %.0f", ErrInvalidQuotaValue, metric, limit)
	}
	return math.Ceil(value), nil
}

const (
	maxQuotaValue        float64 = 1 << 53
	maxRequestQuotaValue float64 = math.MaxInt32
)

// NormalizeMatrix 逐格规范化，返回的错误带有出错的格子位置。
func NormalizeMatrix(m Matrix) (Matrix, error) {
	out := Matrix{}
	for _, metric := range OrderedMetrics {
		for _, window := range OrderedWindows {
			value, err := NormalizeQuotaValue(metric, m.Value(metric, window))
			if err != nil {
				return Matrix{}, fmt.Errorf("%s.%s: %w", metric, window, err)
			}
			out.Set(metric, window, value)
		}
	}
	return out, nil
}

// WindowValuesPatch 是一行的局部修改：nil 表示不修改，显式 0 表示清除。
type WindowValuesPatch struct {
	FiveHour *float64 `json:"5h,omitempty"`
	Day      *float64 `json:"day,omitempty"`
	Week     *float64 `json:"week,omitempty"`
	Month    *float64 `json:"month,omitempty"`
	Lifetime *float64 `json:"lifetime,omitempty"`
}

func (p *WindowValuesPatch) value(window Period) *float64 {
	if p == nil {
		return nil
	}
	switch window {
	case PeriodFiveHour:
		return p.FiveHour
	case PeriodDay:
		return p.Day
	case PeriodWeek:
		return p.Week
	case PeriodMonth:
		return p.Month
	case PeriodLifetime:
		return p.Lifetime
	default:
		return nil
	}
}

// MatrixPatch 是矩阵的局部修改，沿用 PATCH 语义：缺失的度量或窗口保持原值。
type MatrixPatch struct {
	Cost     *WindowValuesPatch `json:"cost,omitempty"`
	Requests *WindowValuesPatch `json:"requests,omitempty"`
	Tokens   *WindowValuesPatch `json:"tokens,omitempty"`
}

func (p *MatrixPatch) row(metric Metric) *WindowValuesPatch {
	if p == nil {
		return nil
	}
	switch metric {
	case MetricCost:
		return p.Cost
	case MetricRequests:
		return p.Requests
	case MetricTokens:
		return p.Tokens
	default:
		return nil
	}
}

// Get 返回补丁中某一格的值；nil 表示该格不修改。
func (p *MatrixPatch) Get(metric Metric, window Period) *float64 {
	return p.row(metric).value(window)
}

// IsEmpty 报告补丁是否不修改任何格子。
func (p *MatrixPatch) IsEmpty() bool {
	for _, metric := range OrderedMetrics {
		for _, window := range OrderedWindows {
			if p.Get(metric, window) != nil {
				return false
			}
		}
	}
	return true
}

// ApplyMatrixPatch 规范化补丁中的每一格并应用到 current 上。
func ApplyMatrixPatch(current Matrix, patch *MatrixPatch) (Matrix, error) {
	if patch == nil {
		return current, nil
	}
	out := current
	for _, metric := range OrderedMetrics {
		for _, window := range OrderedWindows {
			ptr := patch.Get(metric, window)
			if ptr == nil {
				continue
			}
			value, err := NormalizeQuotaValue(metric, *ptr)
			if err != nil {
				return Matrix{}, fmt.Errorf("%s.%s: %w", metric, window, err)
			}
			out.Set(metric, window, value)
		}
	}
	return out, nil
}

// MatrixFromPeriodLimits 把既有的美元周期额度投影到矩阵的 cost 行。
func MatrixFromPeriodLimits(limits PeriodSpendingLimits) Matrix {
	m := Matrix{}
	m.Cost.FiveHour = limits.FiveHour
	m.Cost.Day = limits.Day
	m.Cost.Week = limits.Week
	m.Cost.Month = limits.Month
	return m
}

// PeriodLimitsFromMatrix 取出矩阵 cost 行的四个周期窗口。
func PeriodLimitsFromMatrix(m Matrix) PeriodSpendingLimits {
	return PeriodSpendingLimits{FiveHour: m.Cost.FiveHour, Day: m.Cost.Day, Week: m.Cost.Week, Month: m.Cost.Month}
}

// QuotaLimitExceedsAccountError 表示 Key 的某一格子额度超过了账号同一格的额度。
// 美元的四个周期窗口仍由 LimitExceedsAccountError 表达（既有错误码与前端提示），
// 其余格子用本错误。
type QuotaLimitExceedsAccountError struct {
	Metric       Metric
	Window       Period
	KeyLimit     float64
	AccountLimit float64
}

func (e *QuotaLimitExceedsAccountError) Error() string {
	return fmt.Sprintf("Key %s %s quota %.0f exceeds account %s %s quota %.0f",
		e.Window, e.Metric, e.KeyLimit, e.Window, e.Metric, e.AccountLimit)
}

// ValidateKeyMatrixWithinAccount 校验 Key 的子额度矩阵不超过账号矩阵。
// 只有两边都设置了（> 0）才比较：账号不限而 Key 有限、或 Key 不限而账号有限都合法，
// 运行时两层各自检查。
func ValidateKeyMatrixWithinAccount(key, account Matrix) error {
	for _, metric := range OrderedMetrics {
		for _, window := range OrderedWindows {
			keyLimit, accountLimit := key.Value(metric, window), account.Value(metric, window)
			if keyLimit <= 0 || accountLimit <= 0 || keyLimit <= accountLimit {
				continue
			}
			if metric == MetricCost && window != PeriodLifetime {
				return &LimitExceedsAccountError{Period: window, KeyLimit: keyLimit, AccountLimit: accountLimit}
			}
			return &QuotaLimitExceedsAccountError{Metric: metric, Window: window, KeyLimit: keyLimit, AccountLimit: accountLimit}
		}
	}
	return nil
}

// CappedCell 记录一次因账号降额而被自动压低的 Key 格子。
type CappedCell struct {
	ID     string  `json:"id"`
	Metric Metric  `json:"metric"`
	Window Period  `json:"window"`
	From   float64 `json:"from"`
	To     float64 `json:"to"`
}

// CapMatrixToAccount 把 Key 矩阵中超过账号上限的格子压到账号上限，返回压低后的矩阵与
// 被调整的格子。Key 未设置（0）的格子保持 0：它本来就只受账号天花板约束。
func CapMatrixToAccount(keyID string, key, account Matrix) (Matrix, []CappedCell) {
	out := key
	capped := make([]CappedCell, 0)
	for _, metric := range OrderedMetrics {
		for _, window := range OrderedWindows {
			from, ceiling := key.Value(metric, window), account.Value(metric, window)
			if from <= 0 || ceiling <= 0 || from <= ceiling {
				continue
			}
			out.Set(metric, window, ceiling)
			capped = append(capped, CappedCell{ID: keyID, Metric: metric, Window: window, From: from, To: ceiling})
		}
	}
	return out, capped
}

// LegacyCappedKeys 把压低记录中美元周期窗口的部分转换成旧的 capped-keys 形态，
// 供既有响应字段与审计继续使用。
func LegacyCappedKeys(cells []CappedCell) []CappedKey {
	out := make([]CappedKey, 0, len(cells))
	for _, cell := range cells {
		if cell.Metric != MetricCost || cell.Window == PeriodLifetime {
			continue
		}
		out = append(out, CappedKey{ID: cell.ID, Period: cell.Window, From: cell.From, To: cell.To})
	}
	return out
}

// Set 在补丁中写入一格（值未规范化，ApplyMatrixPatch 时统一校验）。
func (p *MatrixPatch) Set(metric Metric, window Period, value float64) {
	if p == nil {
		return
	}
	var row **WindowValuesPatch
	switch metric {
	case MetricCost:
		row = &p.Cost
	case MetricRequests:
		row = &p.Requests
	case MetricTokens:
		row = &p.Tokens
	default:
		return
	}
	if *row == nil {
		*row = &WindowValuesPatch{}
	}
	v := value
	switch window {
	case PeriodFiveHour:
		(*row).FiveHour = &v
	case PeriodDay:
		(*row).Day = &v
	case PeriodWeek:
		(*row).Week = &v
	case PeriodMonth:
		(*row).Month = &v
	case PeriodLifetime:
		(*row).Lifetime = &v
	}
}

// Clone 深拷贝补丁，避免改写调用方传入的结构。
func (p *MatrixPatch) Clone() *MatrixPatch {
	out := &MatrixPatch{}
	if p == nil {
		return out
	}
	for _, metric := range OrderedMetrics {
		for _, window := range OrderedWindows {
			if v := p.Get(metric, window); v != nil {
				out.Set(metric, window, *v)
			}
		}
	}
	return out
}
