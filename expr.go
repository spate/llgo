/*
Copyright (c) 2011, 2012 Andrew Wilkins <axwalk@gmail.com>

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies
of the Software, and to permit persons to whom the Software is furnished to do
so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package llgo

import (
	"fmt"
	"github.com/axw/gollvm/llvm"
	"github.com/axw/llgo/types"
	"go/ast"
	"go/token"
	"reflect"
	"sort"
	"strconv"
)

func isNilIdent(x ast.Expr) bool {
	ident, ok := x.(*ast.Ident)
	return ok && ident.Obj == types.Nil
}

// Binary logical operators are handled specially, outside of the Value
// type, because of the need to perform lazy evaluation.
//
// Binary logical operators are implemented using a Phi node, which takes
// on the appropriate value depending on which basic blocks branch to it.
func (c *compiler) compileLogicalOp(op token.Token, lhs Value, rhsFunc func() Value) Value {
	lhsBlock := c.builder.GetInsertBlock()
	resultBlock := llvm.AddBasicBlock(lhsBlock.Parent(), "")
	resultBlock.MoveAfter(lhsBlock)
	rhsBlock := llvm.InsertBasicBlock(resultBlock, "")
	falseBlock := llvm.InsertBasicBlock(resultBlock, "")

	if op == token.LOR {
		c.builder.CreateCondBr(lhs.LLVMValue(), resultBlock, rhsBlock)
	} else {
		c.builder.CreateCondBr(lhs.LLVMValue(), rhsBlock, falseBlock)
	}
	c.builder.SetInsertPointAtEnd(rhsBlock)
	rhs := rhsFunc()
	rhsBlock = c.builder.GetInsertBlock() // rhsFunc may create blocks
	c.builder.CreateCondBr(rhs.LLVMValue(), resultBlock, falseBlock)
	c.builder.SetInsertPointAtEnd(falseBlock)
	c.builder.CreateBr(resultBlock)
	c.builder.SetInsertPointAtEnd(resultBlock)

	result := c.builder.CreatePHI(llvm.Int1Type(), "")
	trueValue := llvm.ConstAllOnes(llvm.Int1Type())
	falseValue := llvm.ConstNull(llvm.Int1Type())
	var values []llvm.Value
	var blocks []llvm.BasicBlock
	if op == token.LOR {
		values = []llvm.Value{trueValue, trueValue, falseValue}
		blocks = []llvm.BasicBlock{lhsBlock, rhsBlock, falseBlock}
	} else {
		values = []llvm.Value{trueValue, falseValue}
		blocks = []llvm.BasicBlock{rhsBlock, falseBlock}
	}
	result.AddIncoming(values, blocks)
	return c.NewLLVMValue(result, types.Bool)
}

func (c *compiler) VisitBinaryExpr(expr *ast.BinaryExpr) Value {
	lhs := c.VisitExpr(expr.X)
	switch expr.Op {
	case token.LOR, token.LAND:
		if lhs, ok := lhs.(ConstValue); ok {
			lhsvalue := lhs.Const.Val.(bool)
			switch expr.Op {
			case token.LOR:
				if lhsvalue {
					return lhs
				}
			case token.LAND:
				if !lhsvalue {
					return lhs
				}
			}
			return c.VisitExpr(expr.Y)
		}
		return c.compileLogicalOp(expr.Op, lhs, func() Value { return c.VisitExpr(expr.Y) })
	case token.SHL, token.SHR:
		rhs := c.VisitExpr(expr.Y)
		if _, ok := lhs.(ConstValue); ok {
			typ := c.types.expr[expr]
			lhs = lhs.Convert(typ)
		}
		return lhs.BinaryOp(expr.Op, rhs)
	}
	return lhs.BinaryOp(expr.Op, c.VisitExpr(expr.Y))
}

func (c *compiler) VisitUnaryExpr(expr *ast.UnaryExpr) Value {
	value := c.VisitExpr(expr.X)
	return value.UnaryOp(expr.Op)
}

func (c *compiler) VisitCallExpr(expr *ast.CallExpr) Value {
	switch x := (expr.Fun).(type) {
	case *ast.Ident:
		switch x.String() {
		case "print":
			return c.VisitPrint(expr, false)
		case "println":
			return c.VisitPrint(expr, true)
		case "len":
			return c.VisitLen(expr)
		case "new":
			return c.VisitNew(expr)
		case "make":
			return c.VisitMake(expr)
		case "append":
			return c.VisitAppend(expr)
		case "delete":
			m := c.VisitExpr(expr.Args[0]).(*LLVMValue)
			key := c.VisitExpr(expr.Args[1])
			c.mapDelete(m, key)
			return nil
		case "panic":
			// TODO
			return nil
		}

	case *ast.SelectorExpr:
		// Handle unsafe functions specially.
		if pkgobj, ok := x.X.(*ast.Ident); ok && pkgobj.Obj.Data == types.Unsafe.Data {
			var value int
			switch x.Sel.Name {
			case "Alignof", "Offsetof":
				panic("unimplemented")
			case "Sizeof":
				argtype := c.types.expr[expr.Args[0]]
				value = c.sizeofType(argtype)
				value := c.NewConstValue(token.INT, strconv.Itoa(value))
				value.typ = types.Uintptr
				return value
			}
		}
	}
	lhs := c.VisitExpr(expr.Fun)

	// Is it a type conversion?
	if len(expr.Args) == 1 {
		if _, ok := lhs.(TypeValue); ok {
			typ := lhs.Type()
			value := c.VisitExpr(expr.Args[0])
			return value.Convert(typ)
		}
	}

	// Not a type conversion, so must be a function call.
	fn := lhs.(*LLVMValue)
	fn_type := types.Underlying(fn.Type()).(*types.Func)
	args := make([]llvm.Value, 0)
	if fn_type.Recv != nil {
		// Don't dereference the receiver here. It'll have been worked out in
		// the selector.
		receiver := fn.receiver
		args = append(args, receiver.LLVMValue())
	}
	if nparams := len(fn_type.Params); nparams > 0 {
		if fn_type.IsVariadic {
			nparams--
		}
		for i := 0; i < nparams; i++ {
			value := c.VisitExpr(expr.Args[i])
			param_type := fn_type.Params[i].Type.(types.Type)
			args = append(args, value.Convert(param_type).LLVMValue())
		}
		if fn_type.IsVariadic {
			param_type := fn_type.Params[nparams].Type.(*types.Slice).Elt
			varargs := make([]llvm.Value, 0)
			for i := nparams; i < len(expr.Args); i++ {
				value := c.VisitExpr(expr.Args[i])
				value = value.Convert(param_type)
				varargs = append(varargs, value.LLVMValue())
			}
			slice_value := c.makeLiteralSlice(varargs, param_type)
			args = append(args, slice_value)
		}
	}

	var result_type types.Type
	switch len(fn_type.Results) {
	case 0: // no-op
	case 1:
		result_type = fn_type.Results[0].Type.(types.Type)
	default:
		fields := make([]*ast.Object, len(fn_type.Results))
		for i, result := range fn_type.Results {
			fields[i] = result
		}
		result_type = &types.Struct{Fields: fields}
	}

	// After calling the function, we must bitcast to the computed LLVM
	// type. This is a no-op, and exists just to satisfy LLVM's type
	// comparisons.
	result := c.builder.CreateCall(fn.LLVMValue(), args, "")
	if len(fn_type.Results) == 1 {
		result = c.builder.CreateBitCast(result, c.types.ToLLVM(result_type), "")
	}
	return c.NewLLVMValue(result, result_type)
}

func isIntType(t types.Type) bool {
	for {
		switch x := t.(type) {
		case *types.Name:
			t = x.Underlying
		case *types.Basic:
			switch x.Kind {
			case types.UintKind, types.Uint8Kind, types.Uint16Kind,
				types.Uint32Kind, types.Uint64Kind, types.IntKind,
				types.Int8Kind, types.Int16Kind, types.Int32Kind,
				types.Int64Kind:
				return true
			default:
				return false
			}
		default:
			return false
		}
	}
	panic("unreachable")
}

func (c *compiler) VisitIndexExpr(expr *ast.IndexExpr) Value {
	value := c.VisitExpr(expr.X).(*LLVMValue)
	index := c.VisitExpr(expr.Index)

	typ := value.Type()
	if typ == types.String {
		ptr := c.builder.CreateExtractValue(value.LLVMValue(), 0, "")
		gepindices := []llvm.Value{index.LLVMValue()}
		ptr = c.builder.CreateGEP(ptr, gepindices, "")
		result := c.NewLLVMValue(ptr, &types.Pointer{Base: types.Byte})
		return result.makePointee()
	}

	// We can index a pointer to an array.
	if _, ok := types.Underlying(typ).(*types.Pointer); ok {
		value = value.makePointee()
		typ = value.Type()
	}

	switch types.Underlying(typ).(type) {
	case *types.Array, *types.Slice:
		var gep_indices []llvm.Value
		var ptr llvm.Value
		var result_type types.Type
		switch typ := types.Underlying(typ).(type) {
		case *types.Array:
			// FIXME what to do if value is not addressable?
			// Do we have to load the array onto the stack?
			result_type = typ.Elt
			ptr = value.pointer.LLVMValue()
			gep_indices = append(gep_indices, llvm.ConstNull(llvm.Int32Type()))
		case *types.Slice:
			result_type = typ.Elt
			ptr = c.builder.CreateExtractValue(value.LLVMValue(), 0, "")
		}

		gep_indices = append(gep_indices, index.LLVMValue())
		element := c.builder.CreateGEP(ptr, gep_indices, "")
		result := c.NewLLVMValue(element, &types.Pointer{Base: result_type})
		return result.makePointee()

	case *types.Map:
		value, _ = c.mapLookup(value, index, false)
		return value
	}
	panic(fmt.Sprintf("unreachable (%s)", typ))
}

type selectorCandidate struct {
	Indices []int
	Type    types.Type
}

func (c *compiler) VisitSelectorExpr(expr *ast.SelectorExpr) Value {
	lhs := c.VisitExpr(expr.X)
	if lhs == nil {
		// The only time we should get a nil result is if the object is
		// a package.
		obj := expr.Sel.Obj
		if obj.Kind == ast.Typ {
			return TypeValue{obj.Type.(types.Type)}
		}
		return c.Resolve(obj)
	}

	// TODO(?) record path to field/method during typechecking, so we don't
	// have to search again here.

	name := expr.Sel.Name
	if iface, ok := types.Underlying(lhs.Type()).(*types.Interface); ok {
		i := sort.Search(len(iface.Methods), func(i int) bool {
			return iface.Methods[i].Name >= name
		})
		structValue := lhs.LLVMValue()
		receiver := c.builder.CreateExtractValue(structValue, 0, "")
		f := c.builder.CreateExtractValue(structValue, i+2, "")
		ftype := c.ObjGetType(iface.Methods[i]).(*types.Func)
		method := c.NewLLVMValue(c.builder.CreateBitCast(f, c.types.ToLLVM(ftype), ""), ftype)
		method.receiver = c.NewLLVMValue(receiver, ftype.Recv.Type.(types.Type))
		return method
	}

	// Search through embedded types for field/method.
	var result selectorCandidate
	curr := []selectorCandidate{{nil, lhs.Type()}}
	for result.Type == nil && len(curr) > 0 {
		var next []selectorCandidate
		for _, candidate := range curr {
			indices := candidate.Indices[0:]
			t := candidate.Type

			if p, ok := types.Underlying(t).(*types.Pointer); ok {
				if _, ok := types.Underlying(p.Base).(*types.Struct); ok {
					t = p.Base
				}
			}

			if n, ok := t.(*types.Name); ok {
				i := sort.Search(len(n.Methods), func(i int) bool {
					return n.Methods[i].Name >= name
				})
				if i < len(n.Methods) && n.Methods[i].Name == name {
					result.Indices = indices
					result.Type = t
				}
			}

			if t, ok := types.Underlying(t).(*types.Struct); ok {
				if i, ok := t.FieldIndices[name]; ok {
					result.Indices = append(indices, int(i))
					result.Type = t
				} else {
					// Add embedded types to the next set of types
					// to check.
					for i, field := range t.Fields {
						if field.Name == "" {
							indices = append(indices[0:], i)
							t := field.Type.(types.Type)
							candidate := selectorCandidate{indices, t}
							next = append(next, candidate)
						}
					}
				}
			}
		}
		curr = next
	}

	// Get a pointer to the field/receiver.
	recvValue := lhs.(*LLVMValue)
	if _, ok := types.Underlying(lhs.Type()).(*types.Pointer); !ok {
		recvValue = recvValue.pointer
	}
	recvValue = c.NewLLVMValue(recvValue.LLVMValue(), recvValue.Type())
	if len(result.Indices) > 0 {
		for _, v := range result.Indices {
			ptr := recvValue.LLVMValue()
			field := types.Underlying(types.Deref(recvValue.typ)).(*types.Struct).Fields[v]
			fieldPtr := c.builder.CreateStructGEP(ptr, v, "")
			fieldPtrTyp := &types.Pointer{Base: field.Type.(types.Type)}
			recvValue = c.NewLLVMValue(fieldPtr, fieldPtrTyp)

			// GEP returns a pointer; if the field is a pointer,
			// we must load our pointer-to-a-pointer.
			if _, ok := field.Type.(*types.Pointer); ok {
				recvValue = recvValue.makePointee()
			}
		}
	}
	if !types.Identical(recvValue.typ, expr.Sel.Obj.Type.(types.Type)) {
		recvValue = recvValue.makePointee()
	}

	// Method?
	if expr.Sel.Obj.Kind == ast.Fun {
		method := c.Resolve(expr.Sel.Obj).(*LLVMValue)
		methodType := expr.Sel.Obj.Type.(*types.Func)
		receiverType := methodType.Recv.Type.(types.Type)
		if types.Identical(recvValue.Type(), receiverType) {
			method.receiver = recvValue
		} else if types.Identical(&types.Pointer{Base: recvValue.Type()}, receiverType) {
			method.receiver = recvValue.pointer
		} else {
			method.receiver = recvValue.makePointee()
		}
		return method
	} else {
		return recvValue
	}
	panic("unreachable")
}

func (c *compiler) VisitStarExpr(expr *ast.StarExpr) Value {
	switch operand := c.VisitExpr(expr.X).(type) {
	case TypeValue:
		return TypeValue{&types.Pointer{Base: operand.Type()}}
	case *LLVMValue:
		// We don't want to immediately load the value, as we might be doing an
		// assignment rather than an evaluation. Instead, we return the pointer
		// and tell the caller to load it on demand.
		return operand.makePointee()
	}
	panic("unreachable")
}

func (c *compiler) VisitTypeAssertExpr(expr *ast.TypeAssertExpr) Value {
	if expr.Type == nil {
		// .(type) switch
		// XXX this will probably be handled in the switch statement.
		panic("TODO")
	} else {
		lhs := c.VisitExpr(expr.X)
		typ := c.GetType(expr.Type)
		return lhs.Convert(typ)
	}
	return nil
}

func (c *compiler) VisitExpr(expr ast.Expr) Value {
	switch x := expr.(type) {
	case *ast.BasicLit:
		return c.VisitBasicLit(x)
	case *ast.BinaryExpr:
		return c.VisitBinaryExpr(x)
	case *ast.FuncLit:
		return c.VisitFuncLit(x)
	case *ast.CompositeLit:
		return c.VisitCompositeLit(x)
	case *ast.UnaryExpr:
		return c.VisitUnaryExpr(x)
	case *ast.CallExpr:
		return c.VisitCallExpr(x)
	case *ast.IndexExpr:
		return c.VisitIndexExpr(x)
	case *ast.SelectorExpr:
		return c.VisitSelectorExpr(x)
	case *ast.StarExpr:
		return c.VisitStarExpr(x)
	case *ast.ParenExpr:
		return c.VisitExpr(x.X)
	case *ast.TypeAssertExpr:
		return c.VisitTypeAssertExpr(x)
	case *ast.SliceExpr:
		return c.VisitSliceExpr(x)
	case *ast.FuncType:
		return c.VisitFuncType(x)
	case *ast.MapType:
		return c.VisitMapType(x)
	case *ast.ArrayType:
		return c.VisitArrayType(x)
	case *ast.StructType:
		return c.VisitStructType(x)
	case *ast.InterfaceType:
		return c.VisitInterfaceType(x)
	case *ast.Ident:
		if x.Obj == nil {
			x.Obj = c.LookupObj(x.Name)
		}
		return c.Resolve(x.Obj)
	}
	panic(fmt.Sprintf("Unhandled Expr node: %s", reflect.TypeOf(expr)))
}

// vim: set ft=go :
