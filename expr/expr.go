// Package expr implements the small arithmetic expression language used by
// derived metrics: number literals, input references a/b/..., the four
// arithmetic operators and parentheses.
//
// Grammar (lowest to highest precedence):
//
//	expr   := term (('+' | '-') term)*
//	term   := unary (('*' | '/') unary)*
//	unary  := '-' unary | factor
//	factor := number | ident | '(' expr ')'
package expr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Node is one AST node.
type Node interface {
	node()
}

// NumberNode is a numeric literal.
type NumberNode struct{ Value float64 }

// VarNode is an input slot reference such as "a".
type VarNode struct{ Name string }

// BinaryNode is an arithmetic operation: Op is one of + - * /.
type BinaryNode struct {
	Op       string
	Lhs, Rhs Node
}

// UnaryNode is a unary operation (only "-" is supported).
type UnaryNode struct {
	Op      string
	Operand Node
}

func (*NumberNode) node() {}
func (*VarNode) node()    {}
func (*BinaryNode) node() {}
func (*UnaryNode) node()  {}

// Expr is a parsed expression.
type Expr struct {
	src  string
	root Node
}

// Errors returned by Parse / Eval.
var (
	ErrExpr  = errors.New("expression error")
	ErrEmpty = fmt.Errorf("%w: empty expression", ErrExpr)
)

type lexer struct {
	src string
	pos int
}

type tokenKind int

const (
	tkNum tokenKind = iota
	tkIdent
	tkOp
	tkLParen
	tkRParen
)

type token struct {
	kind tokenKind
	text string
}

func (l *lexer) tokens() ([]token, error) {
	var out []token
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			l.pos++
		case c == '(':
			out = append(out, token{tkLParen, "("})
			l.pos++
		case c == ')':
			out = append(out, token{tkRParen, ")"})
			l.pos++
		case c == '+' || c == '-' || c == '*' || c == '/':
			out = append(out, token{tkOp, string(c)})
			l.pos++
		case c >= '0' && c <= '9' || c == '.':
			start := l.pos
			dot := false
			for l.pos < len(l.src) {
				ch := l.src[l.pos]
				if ch >= '0' && ch <= '9' {
					l.pos++
				} else if ch == '.' && !dot {
					dot = true
					l.pos++
				} else {
					break
				}
			}
			out = append(out, token{tkNum, l.src[start:l.pos]})
		case isIdentStart(c):
			start := l.pos
			l.pos++
			for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
				l.pos++
			}
			out = append(out, token{tkIdent, l.src[start:l.pos]})
		default:
			return nil, fmt.Errorf("%w: unexpected character %q", ErrExpr, string(c))
		}
	}
	return out, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || c >= '0' && c <= '9'
}

type parser struct {
	tokens []token
	pos    int
}

func (p *parser) peek() *token {
	if p.pos >= len(p.tokens) {
		return nil
	}
	return &p.tokens[p.pos]
}

func (p *parser) next() *token {
	t := p.peek()
	if t != nil {
		p.pos++
	}
	return t
}

func (p *parser) parseExpr() (Node, error) {
	lhs, err := p.parseTerm()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t == nil || t.kind != tkOp || t.text != "+" && t.text != "-" {
			return lhs, nil
		}
		p.next()
		rhs, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		lhs = &BinaryNode{Op: t.text, Lhs: lhs, Rhs: rhs}
	}
}

func (p *parser) parseTerm() (Node, error) {
	lhs, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t == nil || t.kind != tkOp || t.text != "*" && t.text != "/" {
			return lhs, nil
		}
		p.next()
		rhs, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		lhs = &BinaryNode{Op: t.text, Lhs: lhs, Rhs: rhs}
	}
}

func (p *parser) parseUnary() (Node, error) {
	t := p.peek()
	if t != nil && t.kind == tkOp && t.text == "-" {
		p.next()
		operand, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &UnaryNode{Op: "-", Operand: operand}, nil
	}
	return p.parseFactor()
}

func (p *parser) parseFactor() (Node, error) {
	t := p.next()
	if t == nil {
		return nil, fmt.Errorf("%w: unexpected end of expression", ErrExpr)
	}
	switch t.kind {
	case tkNum:
		v, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: bad number %q", ErrExpr, t.text)
		}
		return &NumberNode{Value: v}, nil
	case tkIdent:
		return &VarNode{Name: t.text}, nil
	case tkLParen:
		n, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		cl := p.next()
		if cl == nil || cl.kind != tkRParen {
			return nil, fmt.Errorf("%w: missing closing parenthesis", ErrExpr)
		}
		return n, nil
	default:
		return nil, fmt.Errorf("%w: unexpected token %q", ErrExpr, t.text)
	}
}

// Parse parses an expression source string.
func Parse(src string) (*Expr, error) {
	if strings.TrimSpace(src) == "" {
		return nil, ErrEmpty
	}
	l := &lexer{src: src}
	toks, err := l.tokens()
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: toks}
	root, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(toks) {
		return nil, fmt.Errorf("%w: trailing tokens", ErrExpr)
	}
	return &Expr{src: src, root: root}, nil
}

// Root exposes the AST (used for unit inference).
func (e *Expr) Root() Node { return e.root }

// String returns the normalized source with single spaces around operators.
func (e *Expr) String() string { return printNode(e.root) }

func printNode(n Node) string {
	switch x := n.(type) {
	case *NumberNode:
		return strconv.FormatFloat(x.Value, 'g', -1, 64)
	case *VarNode:
		return x.Name
	case *UnaryNode:
		return "-" + printNode(x.Operand)
	case *BinaryNode:
		return printNode(x.Lhs) + " " + x.Op + " " + printNode(x.Rhs)
	default:
		return ""
	}
}

// Inputs returns the referenced input slot names (e.g. "a", "b") in
// first-use order, without duplicates.
func (e *Expr) Inputs() []string {
	seen := map[string]bool{}
	var out []string
	var walk func(Node)
	walk = func(n Node) {
		switch x := n.(type) {
		case *VarNode:
			if !seen[x.Name] {
				seen[x.Name] = true
				out = append(out, x.Name)
			}
		case *BinaryNode:
			walk(x.Lhs)
			walk(x.Rhs)
		case *UnaryNode:
			walk(x.Operand)
		}
	}
	walk(e.root)
	return out
}

// Eval evaluates the expression given input slot values.
func (e *Expr) Eval(inputs map[string]float64) (float64, error) {
	var eval func(Node) (float64, error)
	eval = func(n Node) (float64, error) {
		switch x := n.(type) {
		case *NumberNode:
			return x.Value, nil
		case *VarNode:
			v, ok := inputs[x.Name]
			if !ok {
				return 0, fmt.Errorf("%w: missing input %q", ErrExpr, x.Name)
			}
			return v, nil
		case *UnaryNode:
			v, err := eval(x.Operand)
			if err != nil {
				return 0, err
			}
			return -v, nil
		case *BinaryNode:
			l, err := eval(x.Lhs)
			if err != nil {
				return 0, err
			}
			r, err := eval(x.Rhs)
			if err != nil {
				return 0, err
			}
			switch x.Op {
			case "+":
				return l + r, nil
			case "-":
				return l - r, nil
			case "*":
				return l * r, nil
			case "/":
				if r == 0 {
					return 0, fmt.Errorf("%w: division by zero", ErrExpr)
				}
				return l / r, nil
			default:
				return 0, fmt.Errorf("%w: unknown op %q", ErrExpr, x.Op)
			}
		default:
			return 0, fmt.Errorf("%w: unknown node", ErrExpr)
		}
	}
	return eval(e.root)
}
