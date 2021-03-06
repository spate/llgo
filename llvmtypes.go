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
	"reflect"
)

type LLVMTypeMap struct {
	module llvm.Module
	target llvm.TargetData
	types  map[string]llvm.Type // compile-time LLVM type
}

type TypeMap struct {
	*LLVMTypeMap
	types     map[types.Type]llvm.Value // runtime/reflect type representation
	expr      map[ast.Expr]types.Type
	functions *FunctionCache

	runtimeType,
	runtimeCommonType,
	runtimeUncommonType,
	runtimeArrayType,
	runtimeChanType,
	runtimeFuncType,
	runtimeInterfaceType,
	runtimeMapType,
	runtimePtrType,
	runtimeSliceType,
	runtimeStructType llvm.Type

	hashAlgFunctionType,
	equalAlgFunctionType,
	printAlgFunctionType,
	copyAlgFunctionType llvm.Type
}

func NewLLVMTypeMap(module llvm.Module, target llvm.TargetData) *LLVMTypeMap {
	tm := &LLVMTypeMap{module: module, target: target}
	tm.types = make(map[string]llvm.Type)
	return tm
}

func NewTypeMap(llvmtm *LLVMTypeMap, exprTypes map[ast.Expr]types.Type, c *FunctionCache) *TypeMap {
	tm := &TypeMap{LLVMTypeMap: llvmtm}
	tm.types = make(map[types.Type]llvm.Value)
	tm.expr = exprTypes
	tm.functions = c

	// Load "reflect.go", and generate LLVM types for the runtime type
	// structures.
	pkg, err := parseReflect()
	if err != nil {
		panic(err) // FIXME return err
	}
	objToLLVMType := func(name string) llvm.Type {
		obj := pkg.Scope.Lookup(name)
		return tm.ToLLVM(obj.Type.(types.Type))
	}
	tm.runtimeType = objToLLVMType("runtimeType")
	tm.runtimeCommonType = objToLLVMType("commonType")
	tm.runtimeUncommonType = objToLLVMType("uncommonType")
	tm.runtimeArrayType = objToLLVMType("arrayType")
	tm.runtimeChanType = objToLLVMType("chanType")
	tm.runtimeFuncType = objToLLVMType("funcType")
	tm.runtimeInterfaceType = objToLLVMType("interfaceType")
	tm.runtimeMapType = objToLLVMType("mapType")
	tm.runtimePtrType = objToLLVMType("ptrType")
	tm.runtimeSliceType = objToLLVMType("sliceType")
	tm.runtimeStructType = objToLLVMType("structType")

	// Types for algorithms. See 'runtime/runtime.h'.
	uintptrType := tm.target.IntPtrType()
	voidPtrType := llvm.PointerType(llvm.Int8Type(), 0)
	boolType := llvm.Int1Type()

	// Create runtime algorithm function types.
	params := []llvm.Type{uintptrType, voidPtrType}
	tm.hashAlgFunctionType = llvm.FunctionType(uintptrType, params, false)
	params = []llvm.Type{uintptrType, uintptrType, uintptrType}
	tm.equalAlgFunctionType = llvm.FunctionType(boolType, params, false)
	params = []llvm.Type{uintptrType, voidPtrType}
	tm.printAlgFunctionType = llvm.FunctionType(llvm.VoidType(), params, false)
	params = []llvm.Type{uintptrType, voidPtrType, voidPtrType}
	tm.copyAlgFunctionType = llvm.FunctionType(llvm.VoidType(), params, false)

	return tm
}

func (tm *LLVMTypeMap) ToLLVM(t types.Type) llvm.Type {
	t = types.Underlying(t)
	tstr := t.String()
	lt, ok := tm.types[tstr]
	if !ok {
		lt = tm.makeLLVMType(t)
		if lt.IsNil() {
			panic(fmt.Sprint("Failed to create LLVM type for: ", t))
		}
		tm.types[tstr] = lt
	}
	return lt
}

func (tm *TypeMap) ToRuntime(t types.Type) llvm.Value {
	t = types.Underlying(t)
	r, ok := tm.types[t]
	if !ok {
		_, r = tm.makeRuntimeType(t)
		if r.IsNil() {
			panic(fmt.Sprint("Failed to create runtime type for: ", t))
		}
		tm.types[t] = r
	}
	return r
}

func (tm *LLVMTypeMap) makeLLVMType(t types.Type) llvm.Type {
	switch t := t.(type) {
	case *types.Bad:
		return tm.badLLVMType(t)
	case *types.Basic:
		return tm.basicLLVMType(t)
	case *types.Array:
		return tm.arrayLLVMType(t)
	case *types.Slice:
		return tm.sliceLLVMType(t)
	case *types.Struct:
		return tm.structLLVMType(t)
	case *types.Pointer:
		return tm.pointerLLVMType(t)
	case *types.Func:
		return tm.funcLLVMType(t)
	case *types.Interface:
		return tm.interfaceLLVMType(t)
	case *types.Map:
		return tm.mapLLVMType(t)
	case *types.Chan:
		return tm.chanLLVMType(t)
	case *types.Name:
		return tm.nameLLVMType(t)
	}
	panic("unreachable")
}

func (tm *LLVMTypeMap) badLLVMType(b *types.Bad) llvm.Type {
	return llvm.Type{nil}
}

func (tm *LLVMTypeMap) basicLLVMType(b *types.Basic) llvm.Type {
	switch b.Kind {
	case types.BoolKind:
		return llvm.Int1Type()
	case types.Int8Kind, types.Uint8Kind:
		return llvm.Int8Type()
	case types.Int16Kind, types.Uint16Kind:
		return llvm.Int16Type()
	case types.Int32Kind, types.Uint32Kind, types.UintKind, types.IntKind:
		return llvm.Int32Type()
	case types.Int64Kind, types.Uint64Kind:
		return llvm.Int64Type()
	case types.Float32Kind:
		return llvm.FloatType()
	case types.Float64Kind:
		return llvm.DoubleType()
	case types.UnsafePointerKind, types.UintptrKind:
		return tm.target.IntPtrType()
	//case Complex64: TODO
	//case Complex128:
	//case UntypedInt:
	//case UntypedFloat:
	//case UntypedComplex:
	case types.StringKind:
		i8ptr := llvm.PointerType(llvm.Int8Type(), 0)
		elements := []llvm.Type{i8ptr, llvm.Int32Type()}
		return llvm.StructType(elements, false)
	}
	panic(fmt.Sprint("unhandled kind: ", b.Kind))
}

func (tm *LLVMTypeMap) arrayLLVMType(a *types.Array) llvm.Type {
	return llvm.ArrayType(tm.ToLLVM(a.Elt), int(a.Len))
}

func (tm *LLVMTypeMap) sliceLLVMType(s *types.Slice) llvm.Type {
	elements := []llvm.Type{
		llvm.PointerType(tm.ToLLVM(s.Elt), 0),
		tm.ToLLVM(types.Uint),
		tm.ToLLVM(types.Uint),
	}
	return llvm.StructType(elements, false)
}

func (tm *LLVMTypeMap) structLLVMType(s *types.Struct) llvm.Type {
	// Types may be circular, so we need to first create an empty
	// struct type, then fill in its body after visiting its
	// members.
	sstr := s.String()
	typ, ok := tm.types[sstr]
	if !ok {
		typ = llvm.GlobalContext().StructCreateNamed("")
		tm.types[sstr] = typ
		elements := make([]llvm.Type, len(s.Fields))
		for i, f := range s.Fields {
			ft := f.Type.(types.Type)
			elements[i] = tm.ToLLVM(ft)
		}
		typ.StructSetBody(elements, false)
	}
	return typ
}

func (tm *LLVMTypeMap) pointerLLVMType(p *types.Pointer) llvm.Type {
	return llvm.PointerType(tm.ToLLVM(p.Base), 0)
}

func (tm *LLVMTypeMap) funcLLVMType(f *types.Func) llvm.Type {
	param_types := make([]llvm.Type, 0)

	// Add receiver parameter.
	if f.Recv != nil {
		recv_type := f.Recv.Type.(types.Type)
		param_types = append(param_types, tm.ToLLVM(recv_type))
	}

	for _, param := range f.Params {
		param_type := param.Type.(types.Type)
		param_types = append(param_types, tm.ToLLVM(param_type))
	}

	var return_type llvm.Type
	switch len(f.Results) {
	case 0:
		return_type = llvm.VoidType()
	case 1:
		return_type = tm.ToLLVM(f.Results[0].Type.(types.Type))
	default:
		elements := make([]llvm.Type, len(f.Results))
		for i, result := range f.Results {
			elements[i] = tm.ToLLVM(result.Type.(types.Type))
		}
		return_type = llvm.StructType(elements, false)
	}

	fn_type := llvm.FunctionType(return_type, param_types, false)
	return llvm.PointerType(fn_type, 0)
}

func (tm *LLVMTypeMap) interfaceLLVMType(i *types.Interface) llvm.Type {
	valptr_type := llvm.PointerType(llvm.Int8Type(), 0)
	typptr_type := valptr_type // runtimeCommonType may not be defined yet
	elements := make([]llvm.Type, 2+len(i.Methods))
	elements[0] = valptr_type // value
	elements[1] = typptr_type // type
	for n, m := range i.Methods {
		// Add an opaque pointer parameter to the function for the
		// struct pointer.
		fntype := m.Type.(*types.Func)
		receiver_type := &types.Pointer{Base: types.Int8}
		fntype.Recv = ast.NewObj(ast.Var, "")
		fntype.Recv.Type = receiver_type
		elements[n+2] = tm.ToLLVM(fntype)
	}
	return llvm.StructType(elements, false)
}

func (tm *LLVMTypeMap) mapLLVMType(m *types.Map) llvm.Type {
	// XXX This map type will change in the future, when I get around to it.
	// At the moment, it's representing a really dumb singly linked list.
	list_type := llvm.GlobalContext().StructCreateNamed("")
	list_ptr_type := llvm.PointerType(list_type, 0)
	size_type := llvm.Int32Type()
	element_types := []llvm.Type{size_type, list_type}
	typ := llvm.StructType(element_types, false)
	tm.types[m.String()] = typ

	list_element_types := []llvm.Type{
		list_ptr_type, tm.ToLLVM(m.Key), tm.ToLLVM(m.Elt)}
	list_type.StructSetBody(list_element_types, false)
	return typ
}

func (tm *LLVMTypeMap) chanLLVMType(c *types.Chan) llvm.Type {
	panic("unimplemented")
}

func (tm *LLVMTypeMap) nameLLVMType(n *types.Name) llvm.Type {
	return tm.ToLLVM(n.Underlying)
}

///////////////////////////////////////////////////////////////////////////////

func (tm *TypeMap) makeRuntimeType(t types.Type) (global, ptr llvm.Value) {
	switch t := t.(type) {
	case *types.Bad:
		return tm.badRuntimeType(t)
	case *types.Basic:
		return tm.basicRuntimeType(t)
	case *types.Array:
		return tm.arrayRuntimeType(t)
	case *types.Slice:
		return tm.sliceRuntimeType(t)
	case *types.Struct:
		return tm.structRuntimeType(t)
	case *types.Pointer:
		return tm.pointerRuntimeType(t)
	case *types.Func:
		return tm.funcRuntimeType(t)
	case *types.Interface:
		return tm.interfaceRuntimeType(t)
	case *types.Map:
		return tm.mapRuntimeType(t)
	case *types.Chan:
		return tm.chanRuntimeType(t)
	case *types.Name:
		return tm.nameRuntimeType(t)
	}
	panic("unreachable")
}

func (tm *TypeMap) makeAlgorithmTable(t types.Type) llvm.Value {
	// TODO set these to actual functions.
	hashAlg := llvm.ConstNull(llvm.PointerType(tm.hashAlgFunctionType, 0))
	printAlg := llvm.ConstNull(llvm.PointerType(tm.printAlgFunctionType, 0))
	copyAlg := llvm.ConstNull(llvm.PointerType(tm.copyAlgFunctionType, 0))

	equalAlg := tm.functions.NamedFunction("runtime.memequal", "func f(uintptr, unsafe.Pointer, unsafe.Pointer) bool")
	elems := []llvm.Value{hashAlg, equalAlg, printAlg, copyAlg}
	return llvm.ConstStruct(elems, false)
}

func (tm *TypeMap) makeRuntimeTypeGlobal(v llvm.Value) (global, ptr llvm.Value) {
	runtimeTypeValue := llvm.ConstNull(tm.runtimeType)
	initType := llvm.StructType([]llvm.Type{tm.runtimeType, v.Type()}, false)
	global = llvm.AddGlobal(tm.module, initType, "")
	ptr = llvm.ConstBitCast(global, llvm.PointerType(tm.runtimeType, 0))

	// Set ptrToThis in v's commonType.
	if v.Type() == tm.runtimeCommonType {
		v = llvm.ConstInsertValue(v, ptr, []uint32{10})
	} else {
		commonType := llvm.ConstExtractValue(v, []uint32{0})
		commonType = llvm.ConstInsertValue(commonType, ptr, []uint32{10})
		v = llvm.ConstInsertValue(v, commonType, []uint32{0})
	}

	init := llvm.Undef(initType)
	//runtimeTypeValue = llvm.ConstInsertValue() TODO
	init = llvm.ConstInsertValue(init, runtimeTypeValue, []uint32{0})
	init = llvm.ConstInsertValue(init, v, []uint32{1})
	global.SetInitializer(init)

	return
}

func (tm *TypeMap) makeCommonType(t types.Type, k reflect.Kind) llvm.Value {
	// Not sure if there's an easier way to do this, but if you just
	// use ConstStruct, you end up getting a different llvm.Type.
	lt := tm.ToLLVM(t)
	typ := llvm.ConstNull(tm.runtimeCommonType)
	elementTypes := tm.runtimeCommonType.StructElementTypes()

	// Size.
	size := llvm.SizeOf(lt)
	if size.Type().IntTypeWidth() > elementTypes[0].IntTypeWidth() {
		size = llvm.ConstTrunc(size, elementTypes[0])
	}
	typ = llvm.ConstInsertValue(typ, size, []uint32{0})

	// TODO hash
	// TODO padding

	// Alignment.
	align := llvm.ConstTrunc(llvm.AlignOf(lt), llvm.Int8Type())
	typ = llvm.ConstInsertValue(typ, align, []uint32{3}) // var
	typ = llvm.ConstInsertValue(typ, align, []uint32{4}) // field

	// Kind.
	kind := llvm.ConstInt(llvm.Int8Type(), uint64(k), false)
	typ = llvm.ConstInsertValue(typ, kind, []uint32{5})

	// Algorithm table.
	alg := tm.makeAlgorithmTable(t)
	algptr := llvm.AddGlobal(tm.module, alg.Type(), "")
	algptr.SetInitializer(alg)
	algptr = llvm.ConstBitCast(algptr, elementTypes[6])
	typ = llvm.ConstInsertValue(typ, algptr, []uint32{6})

	// TODO string, gc
	return typ
}

func (tm *TypeMap) badRuntimeType(b *types.Bad) (global, ptr llvm.Value) {
	panic("bad type")
}

func (tm *TypeMap) basicRuntimeType(b *types.Basic) (global, ptr llvm.Value) {
	commonType := tm.makeCommonType(b, reflect.Kind(b.Kind))
	return tm.makeRuntimeTypeGlobal(commonType)
}

func (tm *TypeMap) arrayRuntimeType(a *types.Array) (global, ptr llvm.Value) {
	panic("unimplemented")
}

func (tm *TypeMap) sliceRuntimeType(s *types.Slice) (global, ptr llvm.Value) {
	commonType := tm.makeCommonType(s, reflect.Slice)
	elemRuntimeType := tm.ToRuntime(s.Elt)
	sliceType := llvm.ConstNull(tm.runtimeSliceType)
	sliceType = llvm.ConstInsertValue(sliceType, commonType, []uint32{0})
	sliceType = llvm.ConstInsertValue(sliceType, elemRuntimeType, []uint32{1})
	return tm.makeRuntimeTypeGlobal(sliceType)
}

func (tm *TypeMap) structRuntimeType(s *types.Struct) (global, ptr llvm.Value) {
	commonType := tm.makeCommonType(s, reflect.Struct)
	structType := llvm.ConstNull(tm.runtimeStructType)
	structType = llvm.ConstInsertValue(structType, commonType, []uint32{0})
	// TODO set fields
	return tm.makeRuntimeTypeGlobal(structType)
}

func (tm *TypeMap) pointerRuntimeType(p *types.Pointer) (global, ptr llvm.Value) {
	commonType := tm.makeCommonType(p, reflect.Map)
	ptrType := llvm.ConstNull(tm.runtimePtrType)
	ptrType = llvm.ConstInsertValue(ptrType, commonType, []uint32{0})
	ptrType = llvm.ConstInsertValue(ptrType, tm.ToRuntime(p.Base), []uint32{1})
	return tm.makeRuntimeTypeGlobal(ptrType)
}

func (tm *TypeMap) funcRuntimeType(f *types.Func) (global, ptr llvm.Value) {
	panic("unimplemented")
}

func (tm *TypeMap) interfaceRuntimeType(i *types.Interface) (global, ptr llvm.Value) {
	commonType := tm.makeCommonType(i, reflect.Interface)
	interfaceType := llvm.ConstNull(tm.runtimeInterfaceType)
	interfaceType = llvm.ConstInsertValue(interfaceType, commonType, []uint32{0})
	// TODO set methods
	//interfaceType = llvm.ConstInsertValue(interfaceType, methods, []uint32{1})
	return tm.makeRuntimeTypeGlobal(interfaceType)
}

func (tm *TypeMap) mapRuntimeType(m *types.Map) (global, ptr llvm.Value) {
	commonType := tm.makeCommonType(m, reflect.Map)
	mapType := llvm.ConstNull(tm.runtimeMapType)
	mapType = llvm.ConstInsertValue(mapType, commonType, []uint32{0})
	mapType = llvm.ConstInsertValue(mapType, tm.ToRuntime(m.Key), []uint32{1})
	mapType = llvm.ConstInsertValue(mapType, tm.ToRuntime(m.Elt), []uint32{2})
	return tm.makeRuntimeTypeGlobal(mapType)
}

func (tm *TypeMap) chanRuntimeType(c *types.Chan) (global, ptr llvm.Value) {
	panic("unimplemented")
}

func (tm *TypeMap) nameRuntimeType(n *types.Name) (global, ptr llvm.Value) {
	global, ptr = tm.makeRuntimeType(n.Underlying)
	globalInit := global.Initializer()

	// Locate the common type.
	underlyingRuntimeType := llvm.ConstExtractValue(globalInit, []uint32{1})
	commonType := underlyingRuntimeType
	if _, ok := n.Underlying.(*types.Basic); !ok {
		commonType = llvm.ConstExtractValue(commonType, []uint32{0})
	}

	// Insert the uncommon type.
	uncommonTypeInit := llvm.ConstNull(tm.runtimeUncommonType)
	uncommonType := llvm.AddGlobal(tm.module, uncommonTypeInit.Type(), "")
	uncommonType.SetInitializer(uncommonTypeInit)
	commonType = llvm.ConstInsertValue(commonType, uncommonType, []uint32{9})

	// Update the global's initialiser.
	if _, ok := n.Underlying.(*types.Basic); !ok {
		underlyingRuntimeType = llvm.ConstInsertValue(underlyingRuntimeType, commonType, []uint32{0})
	} else {
		underlyingRuntimeType = commonType
	}
	globalInit = llvm.ConstInsertValue(globalInit, underlyingRuntimeType, []uint32{1})
	global.SetName("__llgo.reflect." + n.Obj.Name)
	global.SetLinkage(llvm.PrivateLinkage)
	return global, ptr
}

// vim: set ft=go :
