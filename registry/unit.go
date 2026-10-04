package registry

import (
	"fmt"
	"sort"
)

// Unit 表示度量单位。同一物理量纲（Dimension）下的单位通过仿射变换互相换算：
//
//	canonical = raw*Scale + Offset
//	raw       = (canonical - Offset)/Scale
//
// 例如华氏度到摄氏度、千元到元。Scale 必须为非零值；Offset 默认为 0。
// 不同 Dimension 的单位互不兼容，参与同一派生运算时会被拒绝。
type Unit struct {
	Name      string  `json:"name"`             // 单位唯一名，如 "CNY"、"kg"
	Dimension string  `json:"dimension"`        // 物理量纲类别，如 "currency"、"mass"、"ratio"
	Scale     float64 `json:"scale"`            // 相对该量纲基准单位的倍数
	Offset    float64 `json:"offset,omitempty"` // 相对该量纲基准单位的偏移
}

// Canonical 把以本单位表示的原始数值换算为量纲基准值。
func (u Unit) Canonical(raw float64) float64 {
	return raw*u.Scale + u.Offset
}

// FromCanonical 把量纲基准值还原为以本单位表示的原始数值。
func (u Unit) FromCanonical(canonical float64) float64 {
	return (canonical - u.Offset) / u.Scale
}

// Convert 把数值从 src 单位换算到 dst 单位。
// 两者 Dimension 必须相同，且 Scale 非零，否则返回 ErrIncompatibleUnit。
func Convert(raw float64, src, dst Unit) (float64, error) {
	if !compatibleUnit(src, dst) {
		return 0, fmt.Errorf("%w: %q(%s) -> %q(%s)",
			ErrIncompatibleUnit, src.Name, src.Dimension, dst.Name, dst.Dimension)
	}
	return dst.FromCanonical(src.Canonical(raw)), nil
}

// Compatible 判断两个单位是否可换算（量纲类别相同且换算系数有效）。
func Compatible(a, b Unit) bool { return compatibleUnit(a, b) }

func compatibleUnit(a, b Unit) bool {
	if a.Dimension == "" || b.Dimension == "" {
		return false
	}
	return a.Dimension == b.Dimension && a.Scale != 0 && b.Scale != 0
}

// RegisterUnit 注册一个可复用的单位模板（按 Name 唯一）。
// 重名但定义不同会被拒绝，避免换算规则被悄悄改变。
func (r *Registry) RegisterUnit(u Unit) error {
	if u.Name == "" {
		return fmt.Errorf("%w: unit name is empty", ErrInvalidDefinition)
	}
	if u.Dimension == "" {
		return fmt.Errorf("%w: unit %q dimension is empty", ErrInvalidDefinition, u.Name)
	}
	if u.Scale == 0 {
		return fmt.Errorf("%w: unit %q scale must be non-zero", ErrInvalidDefinition, u.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.units[u.Name]; ok && old != u {
		return fmt.Errorf("%w: unit %q already registered with a different definition",
			ErrAlreadyExists, u.Name)
	}
	r.units[u.Name] = u
	return nil
}

// defaultUnits 是开箱即用的单位集合，按明确的基准规则归一。
var defaultUnits = []Unit{
	{Name: "CNY", Dimension: "currency", Scale: 1, Offset: 0},
	{Name: "USD", Dimension: "currency", Scale: 7.1, Offset: 0}, // 1 USD = 7.1 CNY（基准）
	{Name: "kCNY", Dimension: "currency", Scale: 1000, Offset: 0},
	{Name: "ratio", Dimension: "ratio", Scale: 1, Offset: 0}, // 0~1 的比例
	{Name: "percent", Dimension: "ratio", Scale: 0.01, Offset: 0},
	{Name: "count", Dimension: "count", Scale: 1, Offset: 0}, // 个
	{Name: "thousand", Dimension: "count", Scale: 1000, Offset: 0},
	{Name: "s", Dimension: "duration", Scale: 1, Offset: 0}, // 秒，基准
	{Name: "ms", Dimension: "duration", Scale: 0.001, Offset: 0},
	{Name: "min", Dimension: "duration", Scale: 60, Offset: 0},
	{Name: "hour", Dimension: "duration", Scale: 3600, Offset: 0},
}

// unitResult 记录表达式子节点求值后携带的量纲类别。
type unitResult struct {
	dimension string
}

// inferUnit 按运算类型与输入量纲推断结果量纲，规则刻意保持显式、保守。
// "ratio" 是特殊的无量纲族（比例、百分比），常量量纲为 ""：
//   - 加减：带量纲的输入必须两两同量纲；常量不得与带量纲值直接加减；
//   - 乘法：至多一个物理量纲（非 ratio），ratio/常量透传；
//     无物理量纲时，出现过 ratio 则结果为 ratio，否则为 ""；
//   - 除法：两个物理量纲必须相同，相消后结果为 ratio；
//     仅一个物理量纲时结果取该量纲；其余情况 ratio 透传；
//   - 一元负号：透传输入量纲。
//
// 不支持复合物理量纲（如 currency*count），避免口径歧义，直接拒绝。
// 调用方在注册时再把推断结果与派生指标声明单位的 Dimension 比对。
func inferUnit(op string, inputs []unitResult) (unitResult, error) {
	switch op {
	case "u-":
		if len(inputs) != 1 {
			return unitResult{}, fmt.Errorf("%w: unary expects 1 input", ErrIncompatibleUnit)
		}
		return inputs[0], nil
	case "+", "-":
		dim := ""
		for _, in := range inputs {
			if in.dimension == "" {
				if dim != "" {
					return unitResult{}, fmt.Errorf("%w: cannot add/subtract dimensionless constant with %q",
						ErrIncompatibleUnit, dim)
				}
				continue
			}
			if dim == "" {
				dim = in.dimension
			} else if dim != in.dimension {
				return unitResult{}, fmt.Errorf("%w: %q vs %q in %q operation",
					ErrIncompatibleUnit, dim, in.dimension, op)
			}
		}
		return unitResult{dimension: dim}, nil
	case "*", "/":
		physical := ""
		physicalCount := 0
		hasRatio := false
		for _, in := range inputs {
			switch in.dimension {
			case "":
				// 常量，透传
			case "ratio":
				hasRatio = true
			default:
				if physical != "" {
					if op == "*" {
						return unitResult{}, fmt.Errorf(
							"%w: %q and %q cannot be multiplied (compound dimensions are not allowed)",
							ErrIncompatibleUnit, physical, in.dimension)
					}
					if physical != in.dimension {
						return unitResult{}, fmt.Errorf("%w: %q divided by %q (different dimensions)",
							ErrIncompatibleUnit, physical, in.dimension)
					}
				}
				physical = in.dimension
				physicalCount++
			}
		}
		switch {
		case op == "/" && physicalCount == 2:
			// 两个同物理量纲相除得到无量纲比率
			return unitResult{dimension: "ratio"}, nil
		case physical != "":
			return unitResult{dimension: physical}, nil
		case hasRatio:
			return unitResult{dimension: "ratio"}, nil
		default:
			return unitResult{dimension: ""}, nil
		}
	default:
		return unitResult{}, fmt.Errorf("%w: unknown operator %q", ErrIncompatibleUnit, op)
	}
}

// sortedUnitNames 返回已注册单位名（测试/文档用）。
func (r *Registry) sortedUnitNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.units))
	for n := range r.units {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
