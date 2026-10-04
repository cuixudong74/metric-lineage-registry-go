package registry

import (
	"fmt"
	"strings"
)

// Expr 是派生指标口径表达式的 AST。
// 支持数字常量、指标引用、括号、+ - * / 以及一元负号，例如：
//
//	(gmv - refund) / orders
//	0.01 * users
//
// 指标引用名规则与指标标识一致：[A-Za-z_][A-Za-z0-9_]*。
type Expr struct {
	node node
	src  string
}

// AST 节点定义。
type (
	numberNode struct{ value float64 }
	refNode    struct{ name string }
	binaryNode struct {
		op          byte
		left, right node
	}
	unaryNode struct {
		op   byte
		expr node
	}
)

type node interface{ astTag() }

func (*numberNode) astTag() {}
func (*refNode) astTag()    {}
func (*binaryNode) astTag() {}
func (*unaryNode) astTag()  {}

// parser 是递归下降解析器。
type parser struct {
	src string
	pos int
}

// ParseExpr 解析口径表达式。空串、非法 token、括号不匹配或解析后有剩余字符都会报错。
func ParseExpr(s string) (*Expr, error) {
	if strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("%w: empty expression", ErrInvalidDefinition)
	}
	p := &parser{src: s}
	p.skipSpace()
	n, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos != len(p.src) {
		return nil, fmt.Errorf("%w: unexpected trailing character %q at %d",
			ErrInvalidDefinition, string(p.src[p.pos]), p.pos)
	}
	return &Expr{node: n, src: s}, nil
}

func (p *parser) skipSpace() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t' || p.src[p.pos] == '\n' || p.src[p.pos] == '\r') {
		p.pos++
	}
}

// expr = term { ('+'|'-') term }
func (p *parser) parseExpr() (node, error) {
	left, err := p.parseTerm()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.src) || (p.src[p.pos] != '+' && p.src[p.pos] != '-') {
			return left, nil
		}
		op := p.src[p.pos]
		p.pos++
		right, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: op, left: left, right: right}
	}
}

// term = factor { ('*'|'/') factor }
func (p *parser) parseTerm() (node, error) {
	left, err := p.parseFactor()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.src) || (p.src[p.pos] != '*' && p.src[p.pos] != '/') {
			return left, nil
		}
		op := p.src[p.pos]
		p.pos++
		right, err := p.parseFactor()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: op, left: left, right: right}
	}
}

// factor = ['-'] ( number | ref | '(' expr ')' )
func (p *parser) parseFactor() (node, error) {
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == '-' {
		p.pos++
		inner, err := p.parseFactor()
		if err != nil {
			return nil, err
		}
		return &unaryNode{op: '-', expr: inner}, nil
	}
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("%w: unexpected end of expression", ErrInvalidDefinition)
	}
	switch c := p.src[p.pos]; {
	case c == '(':
		p.pos++
		n, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		if p.pos >= len(p.src) || p.src[p.pos] != ')' {
			return nil, fmt.Errorf("%w: missing closing parenthesis", ErrInvalidDefinition)
		}
		p.pos++
		return n, nil
	case isDigit(c) || c == '.':
		return p.parseNumber()
	case isIdentStart(c):
		return p.parseRef()
	default:
		return nil, fmt.Errorf("%w: unexpected character %q at %d",
			ErrInvalidDefinition, string(c), p.pos)
	}
}

func (p *parser) parseNumber() (node, error) {
	start := p.pos
	dot := false
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if isDigit(c) {
			p.pos++
		} else if c == '.' && !dot {
			dot = true
			p.pos++
		} else {
			break
		}
	}
	tok := p.src[start:p.pos]
	if tok == "." || tok == "" {
		return nil, fmt.Errorf("%w: invalid number at %d", ErrInvalidDefinition, start)
	}
	var v float64
	if _, err := fmt.Sscanf(tok, "%g", &v); err != nil {
		return nil, fmt.Errorf("%w: invalid number %q", ErrInvalidDefinition, tok)
	}
	return &numberNode{value: v}, nil
}

func (p *parser) parseRef() (node, error) {
	start := p.pos
	p.pos++
	for p.pos < len(p.src) && isIdentPart(p.src[p.pos]) {
		p.pos++
	}
	name := p.src[start:p.pos]
	return &refNode{name: name}, nil
}

func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool { return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isIdentPart(c byte) bool  { return isIdentStart(c) || isDigit(c) }

// String 返回表达式原始文本。
func (e *Expr) String() string {
	if e == nil {
		return ""
	}
	return e.src
}

// Refs 返回表达式引用的全部指标标识（去重、按首次出现顺序）。
func (e *Expr) Refs() []string {
	if e == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	var walk func(n node)
	walk = func(n node) {
		switch t := n.(type) {
		case *refNode:
			if _, ok := seen[t.name]; !ok {
				seen[t.name] = struct{}{}
				out = append(out, t.name)
			}
		case *binaryNode:
			walk(t.left)
			walk(t.right)
		case *unaryNode:
			walk(t.expr)
		case *numberNode:
		}
	}
	walk(e.node)
	return out
}

// evalResult 保存一次求值的数值与其量纲类别。
type evalResult struct {
	value     float64
	dimension string
}

// eval 以"各输入已归一到量纲基准单位"的值求值；缺失输入、除零等返回错误。
// 量纲规则见 inferUnit。
func (e *Expr) eval(inputs map[string]evalResult) (evalResult, error) {
	if e == nil {
		return evalResult{}, fmt.Errorf("%w: nil expression", ErrInvalidDefinition)
	}
	var eval func(n node) (evalResult, error)
	eval = func(n node) (evalResult, error) {
		switch t := n.(type) {
		case *numberNode:
			return evalResult{value: t.value}, nil
		case *refNode:
			v, ok := inputs[t.name]
			if !ok {
				return evalResult{}, fmt.Errorf("%w: %q has no value for this dimension key", ErrUnknownInput, t.name)
			}
			return v, nil
		case *unaryNode:
			v, err := eval(t.expr)
			if err != nil {
				return evalResult{}, err
			}
			u, err := inferUnit("u-", []unitResult{{dimension: v.dimension}})
			if err != nil {
				return evalResult{}, err
			}
			return evalResult{value: -v.value, dimension: u.dimension}, nil
		case *binaryNode:
			l, err := eval(t.left)
			if err != nil {
				return evalResult{}, err
			}
			rr, err := eval(t.right)
			if err != nil {
				return evalResult{}, err
			}
			u, err := inferUnit(string(t.op),
				[]unitResult{{dimension: l.dimension}, {dimension: rr.dimension}})
			if err != nil {
				return evalResult{}, err
			}
			val, err := applyOp(t.op, l.value, rr.value)
			if err != nil {
				return evalResult{}, err
			}
			return evalResult{value: val, dimension: u.dimension}, nil
		default:
			return evalResult{}, fmt.Errorf("%w: unknown ast node", ErrInvalidDefinition)
		}
	}
	return eval(e.node)
}

// inferDim 在只有各引用指标量纲类别（refDims）、没有真实数据的情况下推断结果量纲。
// 用于注册时做单位兼容性与"声明单位量纲 vs 表达式量纲"的静态校验。
func (e *Expr) inferDim(refDims map[string]string) (string, error) {
	if e == nil {
		return "", fmt.Errorf("%w: nil expression", ErrInvalidDefinition)
	}
	var walk func(n node) (string, error)
	walk = func(n node) (string, error) {
		switch t := n.(type) {
		case *numberNode:
			return "", nil
		case *refNode:
			d, ok := refDims[t.name]
			if !ok {
				return "", fmt.Errorf("%w: %q has no resolved dimension", ErrUnknownInput, t.name)
			}
			return d, nil
		case *unaryNode:
			d, err := walk(t.expr)
			if err != nil {
				return "", err
			}
			u, err := inferUnit("u-", []unitResult{{dimension: d}})
			return u.dimension, err
		case *binaryNode:
			l, err := walk(t.left)
			if err != nil {
				return "", err
			}
			rr, err := walk(t.right)
			if err != nil {
				return "", err
			}
			u, err := inferUnit(string(t.op),
				[]unitResult{{dimension: l}, {dimension: rr}})
			return u.dimension, err
		default:
			return "", fmt.Errorf("%w: unknown ast node", ErrInvalidDefinition)
		}
	}
	return walk(e.node)
}

func applyOp(op byte, a, b float64) (float64, error) {
	switch op {
	case '+':
		return a + b, nil
	case '-':
		return a - b, nil
	case '*':
		return a * b, nil
	case '/':
		if b == 0 {
			return 0, fmt.Errorf("%w: division by zero", ErrInvalidDefinition)
		}
		return a / b, nil
	default:
		return 0, fmt.Errorf("%w: unknown operator %q", ErrInvalidDefinition, string(op))
	}
}
