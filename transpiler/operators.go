// This file contains functions transpiling some general operator expressions.
// See binary.go and unary.go.

package transpiler

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/printer"
	"go/token"
	"html/template"
	"strings"

	goast "go/ast"

	"github.com/elliotchance/c2go/ast"
	"github.com/elliotchance/c2go/program"
	"github.com/elliotchance/c2go/types"
	"github.com/elliotchance/c2go/util"
)

// transpileConditionalOperator transpiles a conditional (also known as a
// ternary) operator:
//
//     a ? b : c
//
// We cannot simply convert these to an "if" statement because they by inside
// another expression.
//
// Since Go does not support the ternary operator or inline "if" statements we
// use a closure to work the same way.
//
// It is also important to note that C only evaulates the "b" or "c" condition
// based on the result of "a" (from the above example).
func transpileConditionalOperator(n *ast.ConditionalOperator, p *program.Program) (
	_ *goast.CallExpr, theType string, preStmts []goast.Stmt, postStmts []goast.Stmt, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("Cannot transpile ConditionalOperator : err = %v", err)
		}
	}()

	// a - condition
	a, aType, newPre, newPost, err := transpileToExpr(n.Children()[0], p, false)
	if err != nil {
		return
	}
	preStmts, postStmts = combinePreAndPostStmts(preStmts, postStmts, newPre, newPost)

	// null in C is zero
	if aType == types.NullPointer {
		a = &goast.BasicLit{
			Kind:  token.INT,
			Value: "0",
		}
		aType = "int"
	}

	a, err = types.CastExpr(p, a, aType, "bool")
	if err != nil {
		return
	}

	// b - body
	b, bType, newPre, newPost, err := transpileToExpr(n.Children()[1], p, false)
	if err != nil {
		return
	}
	// Theorephly, length is must be zero
	if len(newPre) > 0 || len(newPost) > 0 {
		p.AddMessage(p.GenerateWarningMessage(
			fmt.Errorf("length of pre or post in body must be zero. {%d,%d}", len(newPre), len(newPost)), n))
	}
	preStmts, postStmts = combinePreAndPostStmts(preStmts, postStmts, newPre, newPost)

	if n.Type != "void" {
		b, err = types.CastExpr(p, b, bType, n.Type)
		if err != nil {
			return
		}
		bType = n.Type
	}

	// c - else body
	c, cType, newPre, newPost, err := transpileToExpr(n.Children()[2], p, false)
	if err != nil {
		return nil, "", nil, nil, err
	}
	preStmts, postStmts = combinePreAndPostStmts(preStmts, postStmts, newPre, newPost)

	if n.Type != "void" {
		c, err = types.CastExpr(p, c, cType, n.Type)
		if err != nil {
			return
		}
		cType = n.Type
	}

	// rightType - generate return type
	var returnType string
	if n.Type != "void" {
		returnType, err = types.ResolveType(p, n.Type)
		if err != nil {
			return
		}
	}

	var bod, els goast.BlockStmt

	bod.Lbrace = 1
	if bType != types.ToVoid {
		if n.Type != "void" {
			bod.List = []goast.Stmt{
				&goast.ReturnStmt{
					Results: []goast.Expr{b},
				},
			}
		} else {
			bod.List = []goast.Stmt{&goast.ExprStmt{b}}
		}
	}

	els.Lbrace = 1
	if cType != types.ToVoid {
		if n.Type != "void" {
			els.List = []goast.Stmt{
				&goast.ReturnStmt{
					Results: []goast.Expr{c},
				},
			}
		} else {
			els.List = []goast.Stmt{&goast.ExprStmt{c}}
		}
	}

	return util.NewFuncClosure(
		returnType,
		&goast.IfStmt{
			Cond: a,
			Body: &bod,
			Else: &els,
		},
	), n.Type, preStmts, postStmts, nil
}

// transpileParenExpr transpiles an expression that is wrapped in parentheses.
// There is a special case where "(0)" is treated as a NULL (since that's what
// the macro expands to). We have to return the type as "null" since we don't
// know at this point what the NULL expression will be used in conjunction with.
func transpileParenExpr(n *ast.ParenExpr, p *program.Program) (
	r *goast.ParenExpr, exprType string, preStmts []goast.Stmt, postStmts []goast.Stmt, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("Cannot transpile ParenExpr. err = %v", err)
			p.AddMessage(p.GenerateWarningMessage(err, n))
		}
	}()

	expr, exprType, preStmts, postStmts, err := transpileToExpr(n.Children()[0], p, false)
	if err != nil {
		return
	}
	if expr == nil {
		err = fmt.Errorf("Expr is nil")
		return
	}

	if exprType == types.NullPointer {
		r = &goast.ParenExpr{X: expr}
		return
	}

	if !types.IsFunction(exprType) && exprType != "void" &&
		exprType != types.ToVoid {
		expr, err = types.CastExpr(p, expr, exprType, n.Type)
		if err != nil {
			return
		}
		exprType = n.Type
	}

	r = &goast.ParenExpr{X: expr}

	return
}

// pointerArithmetic - operations between 'int' and pointer
// Example C code : ptr += i
// ptr = (*(*[1]int)(unsafe.Pointer(uintptr(unsafe.Pointer(&ptr[0])) + (i)*unsafe.Sizeof(ptr[0]))))[:]
// , where i  - left
//        '+' - operator
//      'ptr' - right
//      'int' - leftType transpiled in Go type
// Note:
// 1) rigthType MUST be 'int'
// 2) pointerArithmetic - implemented ONLY right part of formula
// 3) right is MUST be positive value, because impossible multiply uintptr to (-1)
func pointerArithmetic(p *program.Program,
	left goast.Expr, leftType string,
	right goast.Expr, rightType string,
	operator token.Token) (
	_ goast.Expr, _ string, preStmts []goast.Stmt, postStmts []goast.Stmt, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("Cannot transpile pointerArithmetic. err = %v", err)
		}
	}()
	if !types.IsCInteger(rightType) {
		err = fmt.Errorf("right type is not C integer type : '%s'", rightType)
		return
	}
	if !types.IsPointer(leftType) {
		err = fmt.Errorf("left type is not a pointer : '%s'", leftType)
		return
	}

	resolvedLeftType, err := types.ResolveType(p, leftType)
	if err != nil {
		return
	}

	type pA struct {
		Name      string // name of variable: 'ptr'
		Type      string // type of variable: 'int','double'
		Condition string // condition : '-1' ,'(-1+2-2)'
		Operator  string // operator : '+', '-'
	}

	var s pA

	{
		var buf bytes.Buffer
		_ = printer.Fprint(&buf, token.NewFileSet(), left)
		s.Name = buf.String()
	}
	{
		var buf bytes.Buffer
		_ = printer.Fprint(&buf, token.NewFileSet(), right)
		s.Condition = buf.String()
	}
	s.Type = resolvedLeftType[2:]

	s.Operator = "+"
	if operator == token.SUB {
		s.Operator = "-"
	}

	src := `package main
func main(){
	a := (*(*[1000000000]{{ .Type }})(unsafe.Pointer(uintptr(unsafe.Pointer(&{{ .Name }}[0])) {{ .Operator }} (uintptr)({{ .Condition }})*unsafe.Sizeof({{ .Name }}[0]))))[:]
}`
	tmpl := template.Must(template.New("").Parse(src))
	var source bytes.Buffer
	err = tmpl.Execute(&source, s)
	if err != nil {
		err = fmt.Errorf("Cannot execute template. err = %v", err)
		return
	}

	// Create the AST by parsing src.
	fset := token.NewFileSet() // positions are relative to fset
	body := strings.Replace(source.String(), "&#43;", "+", -1)
	body = strings.Replace(body, "&amp;", "&", -1)
	f, err := parser.ParseFile(fset, "", body, 0)
	if err != nil {
		err = fmt.Errorf("Cannot parse file. err = %v", err)
		return
	}

	p.AddImport("unsafe")

	return f.Decls[0].(*goast.FuncDecl).Body.List[0].(*goast.AssignStmt).Rhs[0],
		leftType, preStmts, postStmts, nil
}

func transpileCompoundAssignOperator(
	n *ast.CompoundAssignOperator, p *program.Program, exprIsStmt bool) (
	_ goast.Expr, _ string, preStmts []goast.Stmt, postStmts []goast.Stmt, err error) {

	defer func() {
		if err != nil {
			err = fmt.Errorf("Cannot transpileCompoundAssignOperator. err = %v", err)
		}
	}()

	operator := getTokenForOperator(n.Opcode)

	right, rightType, newPre, newPost, err := transpileToExpr(n.Children()[1], p, false)
	if err != nil {
		return nil, "", nil, nil, err
	}

	preStmts, postStmts = combinePreAndPostStmts(preStmts, postStmts, newPre, newPost)

	// Construct code for computing compound assign operation to an union field
	memberExpr, ok := n.Children()[0].(*ast.MemberExpr)
	if ok {
		ref := memberExpr.GetDeclRefExpr()
		if ref != nil {
			// Get operator by removing last char that is '=' (e.g.: += becomes +)
			binaryOperation := n.Opcode
			binaryOperation = binaryOperation[:(len(binaryOperation) - 1)]

			// TODO: Is this duplicate code in unary.go?
			union := p.GetStruct(ref.Type)
			if union != nil && union.IsUnion {
				attrType, err := types.ResolveType(p, ref.Type)
				if err != nil {
					p.AddMessage(p.GenerateWarningMessage(err, memberExpr))
				}

				// Method names
				getterName := getFunctionNameForUnionGetter(ref.Name, attrType, memberExpr.Name)
				setterName := getFunctionNameForUnionSetter(ref.Name, attrType, memberExpr.Name)

				// Call-Expression argument
				argLHS := util.NewCallExpr(getterName)
				argOp := getTokenForOperator(binaryOperation)
				argRHS := right
				argValue := util.NewBinaryExpr(argLHS, argOp, argRHS, "interface{}", exprIsStmt)

				// Make Go expression
				resExpr := util.NewCallExpr(setterName, argValue)

				return resExpr, "", preStmts, postStmts, nil
			}
		}
	}

	left, leftType, newPre, newPost, err := transpileToExpr(n.Children()[0], p, false)
	if err != nil {
		return nil, "", nil, nil, err
	}

	preStmts, postStmts = combinePreAndPostStmts(preStmts, postStmts, newPre, newPost)

	// Pointer arithmetic
	if types.IsPointer(n.Type) &&
		(operator == token.ADD_ASSIGN || operator == token.SUB_ASSIGN) {
		operator = convertToWithoutAssign(operator)
		v, vType, newPre, newPost, err := pointerArithmetic(p, left, leftType, right, rightType, operator)
		if err != nil {
			return nil, "", nil, nil, err
		}
		if v == nil {
			return nil, "", nil, nil, fmt.Errorf("Expr is nil")
		}
		preStmts, postStmts = combinePreAndPostStmts(preStmts, postStmts, newPre, newPost)
		v = &goast.BinaryExpr{
			X:  goast.NewIdent(getName(p, n.Children()[0])),
			Op: token.ASSIGN,
			Y:  v,
		}
		return v, vType, preStmts, postStmts, nil
	}

	// The right hand argument of the shift left or shift right operators
	// in Go must be unsigned integers. In C, shifting with a negative shift
	// count is undefined behaviour (so we should be able to ignore that case).
	// To handle this, cast the shift count to a uint64.
	if operator == token.SHL_ASSIGN || operator == token.SHR_ASSIGN {
		right, err = types.CastExpr(p, right, rightType, "unsigned long long")
		p.AddMessage(p.GenerateWarningOrErrorMessage(err, n, right == nil))
		if right == nil {
			right = util.NewNil()
		}
	}

	resolvedLeftType, err := types.ResolveType(p, leftType)
	if err != nil {
		p.AddMessage(p.GenerateWarningMessage(err, n))
	}

	if right == nil {
		err = fmt.Errorf("Right part is nil. err = %v", err)
		return nil, "", nil, nil, err
	}
	if left == nil {
		err = fmt.Errorf("Left part is nil. err = %v", err)
		return nil, "", nil, nil, err
	}

	return util.NewBinaryExpr(left, operator, right, resolvedLeftType, exprIsStmt),
		n.Type, preStmts, postStmts, nil
}

// getTokenForOperator returns the Go operator token for the provided C
// operator.
func getTokenForOperator(operator string) token.Token {
	switch operator {
	// Arithmetic
	case "--":
		return token.DEC
	case "++":
		return token.INC
	case "+":
		return token.ADD
	case "-":
		return token.SUB
	case "*":
		return token.MUL
	case "/":
		return token.QUO
	case "%":
		return token.REM

	// Assignment
	case "=":
		return token.ASSIGN
	case "+=":
		return token.ADD_ASSIGN
	case "-=":
		return token.SUB_ASSIGN
	case "*=":
		return token.MUL_ASSIGN
	case "/=":
		return token.QUO_ASSIGN
	case "%=":
		return token.REM_ASSIGN
	case "&=":
		return token.AND_ASSIGN
	case "|=":
		return token.OR_ASSIGN
	case "^=":
		return token.XOR_ASSIGN
	case "<<=":
		return token.SHL_ASSIGN
	case ">>=":
		return token.SHR_ASSIGN

	// Bitwise
	case "&":
		return token.AND
	case "|":
		return token.OR
	case "~":
		return token.XOR
	case ">>":
		return token.SHR
	case "<<":
		return token.SHL
	case "^":
		return token.XOR

	// Comparison
	case ">=":
		return token.GEQ
	case "<=":
		return token.LEQ
	case "<":
		return token.LSS
	case ">":
		return token.GTR
	case "!=":
		return token.NEQ
	case "==":
		return token.EQL

	// Logical
	case "!":
		return token.NOT
	case "&&":
		return token.LAND
	case "||":
		return token.LOR

	// Other
	case ",":
		return token.COMMA
	}

	panic(fmt.Sprintf("unknown operator: %s", operator))
}

func convertToWithoutAssign(operator token.Token) token.Token {
	switch operator {
	case token.ADD_ASSIGN: // "+="
		return token.ADD
	case token.SUB_ASSIGN: // "-="
		return token.SUB
	case token.MUL_ASSIGN: // "*="
		return token.MUL
	case token.QUO_ASSIGN: // "/="
		return token.QUO
	}
	panic(fmt.Sprintf("not support operator: %v", operator))
}

func atomicOperation(n ast.Node, p *program.Program) (
	expr goast.Expr, exprType string, preStmts, postStmts []goast.Stmt, err error) {

	expr, exprType, preStmts, postStmts, err = transpileToExpr(n, p, false)
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("Cannot create atomicOperation. err = %v", err)
		}
	}()

	switch v := n.(type) {
	case *ast.UnaryOperator:
		switch v.Operator {
		case "&", "*", "!":
			return
		}
		var varName string
		if vv, ok := v.Children()[0].(*ast.DeclRefExpr); !ok {
			break
		} else {
			varName = vv.Name
		}

		var exprResolveType string
		exprResolveType, err = types.ResolveType(p, v.Type)
		if err != nil {
			return
		}

		// operators: ++, --
		if v.IsPrefix {
			// Example:
			// UnaryOperator 0x3001768 <col:204, col:206> 'int' prefix '++'
			// `-DeclRefExpr 0x3001740 <col:206> 'int' lvalue Var 0x303e888 'current_test' 'int'
			expr = util.NewAnonymousFunction(append(preStmts, &goast.ExprStmt{expr}),
				nil,
				util.NewIdent(varName),
				exprResolveType)
			preStmts = nil
			break
		}
		// Example:
		// UnaryOperator 0x3001768 <col:204, col:206> 'int' postfix '++'
		// `-DeclRefExpr 0x3001740 <col:206> 'int' lvalue Var 0x303e888 'current_test' 'int'
		expr = util.NewAnonymousFunction(preStmts,
			[]goast.Stmt{&goast.ExprStmt{expr}},
			util.NewIdent(varName),
			exprResolveType)
		preStmts = nil

	case *ast.CompoundAssignOperator:
		// CompoundAssignOperator 0x32911c0 <col:18, col:28> 'int' '-=' ComputeLHSTy='int' ComputeResultTy='int'
		// |-DeclRefExpr 0x3291178 <col:18> 'int' lvalue Var 0x328df60 'iterator' 'int'
		// `-IntegerLiteral 0x32911a0 <col:28> 'int' 2
		var varName string
		if vv, ok := v.Children()[0].(*ast.DeclRefExpr); !ok {
			break
		} else {
			varName = vv.Name
		}

		var exprResolveType string
		exprResolveType, err = types.ResolveType(p, v.Type)
		if err != nil {
			return
		}

		expr = util.NewAnonymousFunction(append(preStmts, &goast.ExprStmt{expr}),
			nil,
			util.NewIdent(varName),
			exprResolveType)
		preStmts = nil

	case *ast.ParenExpr:
		// ParenExpr 0x3c42468 <col:18, col:40> 'int'
		return atomicOperation(v.Children()[0], p)

	case *ast.ImplicitCastExpr:
		if _, ok := v.Children()[0].(*ast.MemberExpr); ok {
			return
		}
		if _, ok := v.Children()[0].(*ast.IntegerLiteral); ok {
			return
		}

		expr, exprType, preStmts, postStmts, err = atomicOperation(v.Children()[0], p)
		if err != nil {
			return nil, "", nil, nil, err
		}
		if exprType == types.NullPointer {
			return
		}
		if !types.IsFunction(exprType) && v.Kind != ast.ImplicitCastExprArrayToPointerDecay {
			expr, err = types.CastExpr(p, expr, exprType, v.Type)
			if err != nil {
				return nil, "", nil, nil, err
			}
			exprType = v.Type
		}
		return

	case *ast.BinaryOperator:
		switch v.Operator {
		case ",":
			var varName string
			if vv, ok := v.Children()[1].(*ast.ImplicitCastExpr); ok {
				if vvv, ok := vv.Children()[0].(*ast.DeclRefExpr); ok {
					varName = vvv.Name
				}
			}
			if varName == "" {
				return
			}
			// `-BinaryOperator 0x3c42440 <col:19, col:32> 'int' ','
			//   |-BinaryOperator 0x3c423d8 <col:19, col:30> 'int' '='
			//   | |-DeclRefExpr 0x3c42390 <col:19> 'int' lvalue Var 0x3c3cf60 'iterator' 'int'
			//   | `-IntegerLiteral 0x3c423b8 <col:30> 'int' 0
			//   `-ImplicitCastExpr 0x3c42428 <col:32> 'int' <LValueToRValue>
			//     `-DeclRefExpr 0x3c42400 <col:32> 'int' lvalue Var 0x3c3cf60 'iterator' 'int'
			e, _, newPre, newPost, _ := transpileToExpr(v.Children()[0], p, false)
			body := combineStmts(&goast.ExprStmt{e}, newPre, newPost)

			e, exprType, _, _, _ = atomicOperation(v.Children()[1], p)

			var tt string
			if tt, err = types.ResolveType(p, exprType); err == nil {
				exprType = tt
			} else {
				p.AddMessage(p.GenerateWarningMessage(err, v))
				err = nil
			}

			expr = util.NewAnonymousFunction(body,
				nil,
				util.NewIdent(varName),
				exprType)
			preStmts = nil
			postStmts = nil
			exprType = v.Type
			return

		case "=":
			// Find ast.DeclRefExpr in Children[0]
			decl, ok := getDeclRefExpr(v.Children()[0])
			if !ok {
				return
			}
			// BinaryOperator 0x2a230c0 <col:8, col:13> 'int' '='
			// |-UnaryOperator 0x2a23080 <col:8, col:9> 'int' lvalue prefix '*'
			// | `-ImplicitCastExpr 0x2a23068 <col:9> 'int *' <LValueToRValue>
			// |   `-DeclRefExpr 0x2a23040 <col:9> 'int *' lvalue Var 0x2a22f20 'a' 'int *'
			// `-IntegerLiteral 0x2a230a0 <col:13> 'int' 42

			// VarDecl 0x328dc50 <col:3, col:29> col:13 used d 'int' cinit
			// `-BinaryOperator 0x328dd98 <col:17, col:29> 'int' '='
			//   |-DeclRefExpr 0x328dcb0 <col:17> 'int' lvalue Var 0x328dae8 'a' 'int'
			//   `-BinaryOperator 0x328dd70 <col:21, col:29> 'int' '='
			//     |-DeclRefExpr 0x328dcd8 <col:21> 'int' lvalue Var 0x328db60 'b' 'int'
			//     `-BinaryOperator 0x328dd48 <col:25, col:29> 'int' '='
			//       |-DeclRefExpr 0x328dd00 <col:25> 'int' lvalue Var 0x328dbd8 'c' 'int'
			//       `-IntegerLiteral 0x328dd28 <col:29> 'int' 42

			varName := decl.Name

			var exprResolveType string
			exprResolveType, err = types.ResolveType(p, v.Type)
			if err != nil {
				return
			}

			e, _, newPre, newPost, _ := transpileToExpr(v, p, false)
			body := combineStmts(&goast.ExprStmt{e}, newPre, newPost)

			expr, exprType, _, _, _ = atomicOperation(v.Children()[0], p)

			preStmts = nil
			postStmts = nil

			var returnValue goast.Expr = util.NewIdent(varName)
			if types.IsPointer(decl.Type) && !types.IsPointer(v.Type) {
				returnValue = &goast.IndexExpr{
					X: returnValue,
					Index: &goast.BasicLit{
						Kind:  token.INT,
						Value: "0",
					},
				}
			}

			expr = util.NewAnonymousFunction(body,
				nil,
				returnValue,
				exprResolveType)
			expr = &goast.ParenExpr{
				X:      expr,
				Lparen: 1,
			}
		}

	}

	return
}

// getDeclRefExpr - find ast DeclRefExpr
// Examples of input ast trees:
// UnaryOperator 0x2a23080 <col:8, col:9> 'int' lvalue prefix '*'
// `-ImplicitCastExpr 0x2a23068 <col:9> 'int *' <LValueToRValue>
//   `-DeclRefExpr 0x2a23040 <col:9> 'int *' lvalue Var 0x2a22f20 'a' 'int *'
//
// DeclRefExpr 0x328dd00 <col:25> 'int' lvalue Var 0x328dbd8 'c' 'int'
func getDeclRefExpr(n ast.Node) (*ast.DeclRefExpr, bool) {
	switch v := n.(type) {
	case *ast.DeclRefExpr:
		return v, true
	case *ast.ImplicitCastExpr:
		return getDeclRefExpr(n.Children()[0])
	case *ast.UnaryOperator:
		return getDeclRefExpr(n.Children()[0])
	}
	return nil, false
}
