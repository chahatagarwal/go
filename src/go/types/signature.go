// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types

import (
	"go/ast"
	"go/token"
)

// ----------------------------------------------------------------------------
// API

// A Signature represents a (non-builtin) function or method type.
// The receiver is ignored when comparing signatures for identity.
type Signature struct {
	// We need to keep the scope in Signature (rather than passing it around
	// and store it in the Func Object) because when type-checking a function
	// literal we call the general type checker which returns a general Type.
	// We then unpack the *Signature and use the scope for the literal body.
	rparams  *TypeParamList // receiver type parameters from left to right, or nil
	tparams  *TypeParamList // type parameters from left to right, or nil
	scope    *Scope         // function scope for package-local and non-instantiated signatures; nil otherwise
	recv     *Var           // nil if not a method
	params   *Tuple         // (incoming) parameters from left to right; or nil
	results  *Tuple         // (outgoing) results from left to right; or nil
	variadic bool           // true if the last parameter's type is of the form ...T (or string, for append built-in only)
}

// NewSignature returns a new function type for the given receiver, parameters,
// and results, either of which may be nil. If variadic is set, the function
// is variadic, it must have at least one parameter, and the last parameter
// must be of unnamed slice type.
//
// Deprecated: Use NewSignatureType instead which allows for type parameters.
func NewSignature(recv *Var, params, results *Tuple, variadic bool) *Signature {
	return NewSignatureType(recv, nil, nil, params, results, variadic)
}

// NewSignatureType creates a new function type for the given receiver,
// receiver type parameters, type parameters, parameters, and results. If
// variadic is set, params must hold at least one parameter and the last
// parameter must be of unnamed slice type. If recv is non-nil, typeParams must
// be empty. If recvTypeParams is non-empty, recv must be non-nil.
func NewSignatureType(recv *Var, recvTypeParams, typeParams []*TypeParam, params, results *Tuple, variadic bool) *Signature {
	if variadic {
		n := params.Len()
		if n == 0 {
			panic("variadic function must have at least one parameter")
		}
		if _, ok := params.At(n - 1).typ.(*Slice); !ok {
			panic("variadic parameter must be of unnamed slice type")
		}
	}
	sig := &Signature{recv: recv, params: params, results: results, variadic: variadic}
	if len(recvTypeParams) != 0 {
		if recv == nil {
			panic("function with receiver type parameters must have a receiver")
		}
		sig.rparams = bindTParams(recvTypeParams)
	}
	if len(typeParams) != 0 {
		if recv != nil {
			panic("function with type parameters cannot have a receiver")
		}
		sig.tparams = bindTParams(typeParams)
	}
	return sig
}

// Recv returns the receiver of signature s (if a method), or nil if a
// function. It is ignored when comparing signatures for identity.
//
// For an abstract method, Recv returns the enclosing interface either
// as a *Named or an *Interface. Due to embedding, an interface may
// contain methods whose receiver type is a different interface.
func (s *Signature) Recv() *Var { return s.recv }

// TypeParams returns the type parameters of signature s, or nil.
func (s *Signature) TypeParams() *TypeParamList { return s.tparams }

// RecvTypeParams returns the receiver type parameters of signature s, or nil.
func (s *Signature) RecvTypeParams() *TypeParamList { return s.rparams }

// Params returns the parameters of signature s, or nil.
func (s *Signature) Params() *Tuple { return s.params }

// Results returns the results of signature s, or nil.
func (s *Signature) Results() *Tuple { return s.results }

// Variadic reports whether the signature s is variadic.
func (s *Signature) Variadic() bool { return s.variadic }

func (t *Signature) Underlying() Type { return t }
func (t *Signature) String() string   { return TypeString(t, nil) }

// ----------------------------------------------------------------------------
// Implementation

// funcType type-checks a function or method type.
func (check *Checker) funcType(sig *Signature, recvPar *ast.FieldList, ftyp *ast.FuncType) {
	check.openScope(ftyp, "function")
	check.scope.isFunc = true
	check.recordScope(ftyp, check.scope)
	sig.scope = check.scope
	defer check.closeScope()

	if recvPar != nil && len(recvPar.List) > 0 {
		// collect generic receiver type parameters, if any
		// - a receiver type parameter is like any other type parameter, except that it is declared implicitly
		// - the receiver specification acts as local declaration for its type parameters, which may be blank
		_, rname, rparams := check.unpackRecv(recvPar.List[0].Type, true)
		if len(rparams) > 0 {
			sig.rparams = bindTParams(check.declareTypeParams(nil, rparams))
			// Blank identifiers don't get declared, so naive type-checking of the
			// receiver type expression would fail in Checker.collectParams below,
			// when Checker.ident cannot resolve the _ to a type.
			//
			// Checker.recvTParamMap maps these blank identifiers to their type parameter
			// types, so that they may be resolved in Checker.ident when they fail
			// lookup in the scope.
			for i, p := range rparams {
				if p.Name == "_" {
					tpar := sig.rparams.At(i)
					if check.recvTParamMap == nil {
						check.recvTParamMap = make(map[*ast.Ident]*TypeParam)
					}
					check.recvTParamMap[p] = tpar
				}
			}
			// determine receiver type to get its type parameters
			// and the respective type parameter bounds
			var recvTParams []*TypeParam
			if rname != nil {
				// recv should be a Named type (otherwise an error is reported elsewhere)
				// Also: Don't report an error via genericType since it will be reported
				//       again when we type-check the signature.
				// TODO(gri) maybe the receiver should be marked as invalid instead?
				if recv, _ := check.genericType(rname, false).(*Named); recv != nil {
					recvTParams = recv.TypeParams().list()
				}
			}
			// provide type parameter bounds
			// - only do this if we have the right number (otherwise an error is reported elsewhere)
			if sig.RecvTypeParams().Len() == len(recvTParams) {
				// We have a list of *TypeNames but we need a list of Types.
				list := make([]Type, sig.RecvTypeParams().Len())
				for i, t := range sig.RecvTypeParams().list() {
					list[i] = t
					check.mono.recordCanon(t, recvTParams[i])
				}
				smap := makeSubstMap(recvTParams, list)
				for i, tpar := range sig.RecvTypeParams().list() {
					bound := recvTParams[i].bound
					// bound is (possibly) parameterized in the context of the
					// receiver type declaration. Substitute parameters for the
					// current context.
					tpar.bound = check.subst(tpar.obj.pos, bound, smap, nil)
				}
			}
		}
	}

	if ftyp.TypeParams != nil {
		check.collectTypeParams(&sig.tparams, ftyp.TypeParams)
		// Always type-check method type parameters but complain that they are not allowed.
		// (A separate check is needed when type-checking interface method signatures because
		// they don't have a receiver specification.)
		if recvPar != nil {
			check.errorf(ftyp.TypeParams, _Todo, "methods cannot have type parameters")
		}
	}

	// Value (non-type) parameters' scope starts in the function body. Use a temporary scope for their
	// declarations and then squash that scope into the parent scope (and report any redeclarations at
	// that time).
	scope := NewScope(check.scope, token.NoPos, token.NoPos, "function body (temp. scope)")
	recvList, _ := check.collectParams(scope, recvPar, false)
	params, variadic := check.collectParams(scope, ftyp.Params, true)
	results, _ := check.collectParams(scope, ftyp.Results, false)
	scope.squash(func(obj, alt Object) {
		check.errorf(obj, _DuplicateDecl, "%s redeclared in this block", obj.Name())
		check.reportAltDecl(alt)
	})

	if recvPar != nil {
		// recv parameter list present (may be empty)
		// spec: "The receiver is specified via an extra parameter section preceding the
		// method name. That parameter section must declare a single parameter, the receiver."
		var recv *Var
		switch len(recvList) {
		case 0:
			// error reported by resolver
			recv = NewParam(0, nil, "", Typ[Invalid]) // ignore recv below
		default:
			// more than one receiver
			check.error(recvList[len(recvList)-1], _BadRecv, "method must have exactly one receiver")
			fallthrough // continue with first receiver
		case 1:
			recv = recvList[0]
		}

		// TODO(gri) We should delay rtyp expansion to when we actually need the
		//           receiver; thus all checks here should be delayed to later.
		rtyp, _ := deref(recv.typ)

		// spec: "The receiver type must be of the form T or *T where T is a type name."
		// (ignore invalid types - error was reported before)
		if rtyp != Typ[Invalid] {
			var err string
			switch T := rtyp.(type) {
			case *Named:
				T.resolve(check.conf.Context)
				// The receiver type may be an instantiated type referred to
				// by an alias (which cannot have receiver parameters for now).
				if T.TypeArgs() != nil && sig.RecvTypeParams() == nil {
					check.errorf(atPos(recv.pos), _Todo, "cannot define methods on instantiated type %s", recv.typ)
					break
				}
				// spec: "The type denoted by T is called the receiver base type; it must not
				// be a pointer or interface type and it must be declared in the same package
				// as the method."
				if T.obj.pkg != check.pkg {
					err = "type not defined in this package"
				} else {
					// The underlying type of a receiver base type can be a type parameter;
					// e.g. for methods with a generic receiver T[P] with type T[P any] P.
					underIs(T, func(u Type) bool {
						switch u := u.(type) {
						case *Basic:
							// unsafe.Pointer is treated like a regular pointer
							if u.kind == UnsafePointer {
								err = "unsafe.Pointer"
								return false
							}
						case *Pointer, *Interface:
							err = "pointer or interface type"
							return false
						}
						return true
					})
				}
			case *Basic:
				err = "basic or unnamed type"
			default:
				check.errorf(recv, _InvalidRecv, "invalid receiver type %s", recv.typ)
			}
			if err != "" {
				check.errorf(recv, _InvalidRecv, "invalid receiver type %s (%s)", recv.typ, err)
				// ok to continue
			}
		}
		sig.recv = recv
	}

	sig.params = NewTuple(params...)
	sig.results = NewTuple(results...)
	sig.variadic = variadic
}

// collectParams declares the parameters of list in scope and returns the corresponding
// variable list.
func (check *Checker) collectParams(scope *Scope, list *ast.FieldList, variadicOk bool) (params []*Var, variadic bool) {
	if list == nil {
		return
	}

	var named, anonymous bool
	for i, field := range list.List {
		ftype := field.Type
		if t, _ := ftype.(*ast.Ellipsis); t != nil {
			ftype = t.Elt
			if variadicOk && i == len(list.List)-1 && len(field.Names) <= 1 {
				variadic = true
			} else {
				check.softErrorf(t, _MisplacedDotDotDot, "can only use ... with final parameter in list")
				// ignore ... and continue
			}
		}
		typ := check.varType(ftype)
		// The parser ensures that f.Tag is nil and we don't
		// care if a constructed AST contains a non-nil tag.
		if len(field.Names) > 0 {
			// named parameter
			for _, name := range field.Names {
				if name.Name == "" {
					check.invalidAST(name, "anonymous parameter")
					// ok to continue
				}
				par := NewParam(name.Pos(), check.pkg, name.Name, typ)
				check.declare(scope, name, par, scope.pos)
				params = append(params, par)
			}
			named = true
		} else {
			// anonymous parameter
			par := NewParam(ftype.Pos(), check.pkg, "", typ)
			check.recordImplicit(field, par)
			params = append(params, par)
			anonymous = true
		}
	}

	if named && anonymous {
		check.invalidAST(list, "list contains both named and anonymous parameters")
		// ok to continue
	}

	// For a variadic function, change the last parameter's type from T to []T.
	// Since we type-checked T rather than ...T, we also need to retro-actively
	// record the type for ...T.
	if variadic {
		last := params[len(params)-1]
		last.typ = &Slice{elem: last.typ}
		check.recordTypeAndValue(list.List[len(list.List)-1].Type, typexpr, last.typ, nil)
	}

	return
}
