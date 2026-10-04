package registry

import "errors"

// 预定义错误，覆盖注册、血缘、单位维度校验等场景。
var (
	ErrNotFound             = errors.New("metric not found")
	ErrAlreadyExists        = errors.New("metric definition is identical to the current version")
	ErrInvalidDefinition    = errors.New("invalid metric definition")
	ErrCycleDetected        = errors.New("cycle detected in lineage graph")
	ErrIncompatibleUnit     = errors.New("incompatible unit in derivation")
	ErrDimensionMismatch    = errors.New("dimension mismatch in derivation")
	ErrDependentStillActive = errors.New("cannot retire metric: downstream derived metrics still depend on it")
	ErrUnknownInput         = errors.New("derivation references unknown input metric")
	ErrStore                = errors.New("persistence store error")
)
