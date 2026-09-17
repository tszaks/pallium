package codeindex

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"github.com/tszaks/pallium/internal/db"
)

// parseGo uses the standard library's own parser. This is the one language
// where Pallium gets exact symbols for free: no cgo, no grammar files, no
// heuristics, and doc comments come attached to their declarations. Everything
// else in this package is a line scanner by comparison.
func parseGo(path string, content []byte, resolver *Resolver) (ParsedFile, bool) {
	fset := token.NewFileSet()
	// Tolerate broken syntax: a half-written file mid-edit still has a
	// usable declaration list, and refusing to index it would make the tool
	// least useful exactly when someone is working.
	file, err := parser.ParseFile(fset, path, content, parser.ParseComments|parser.SkipObjectResolution)
	if file == nil || (err != nil && len(file.Decls) == 0) {
		return ParsedFile{}, false
	}
	_ = err

	parsed := ParsedFile{Lang: "go", Parser: "go/ast"}
	line := func(pos token.Pos) int {
		if !pos.IsValid() {
			return 0
		}
		return fset.Position(pos).Line
	}

	declared := make(map[string]struct{})

	for _, decl := range file.Decls {
		switch node := decl.(type) {
		case *ast.FuncDecl:
			kind := "func"
			receiver := ""
			if node.Recv != nil && len(node.Recv.List) > 0 {
				kind = "method"
				receiver = typeName(node.Recv.List[0].Type)
			}
			name := node.Name.Name
			declared[name] = struct{}{}
			parsed.Symbols = append(parsed.Symbols, db.CodeSymbol{
				Path:      path,
				Name:      name,
				Kind:      kind,
				Receiver:  receiver,
				Signature: goFuncSignature(node, receiver),
				Doc:       docText(node.Doc),
				StartLine: line(node.Pos()),
				EndLine:   line(node.End()),
				Exported:  ast.IsExported(name),
			})
		case *ast.GenDecl:
			for _, spec := range node.Specs {
				switch item := spec.(type) {
				case *ast.TypeSpec:
					name := item.Name.Name
					declared[name] = struct{}{}
					parsed.Symbols = append(parsed.Symbols, db.CodeSymbol{
						Path:      path,
						Name:      name,
						Kind:      goTypeKind(item),
						Signature: "type " + name,
						Doc:       firstNonEmptyDoc(item.Doc, node.Doc),
						StartLine: line(item.Pos()),
						EndLine:   line(item.End()),
						Exported:  ast.IsExported(name),
					})
				case *ast.ValueSpec:
					kind := "var"
					if node.Tok == token.CONST {
						kind = "const"
					}
					for _, ident := range item.Names {
						if ident.Name == "_" {
							continue
						}
						declared[ident.Name] = struct{}{}
						parsed.Symbols = append(parsed.Symbols, db.CodeSymbol{
							Path:      path,
							Name:      ident.Name,
							Kind:      kind,
							Signature: kind + " " + ident.Name,
							Doc:       firstNonEmptyDoc(item.Doc, node.Doc),
							StartLine: line(ident.Pos()),
							EndLine:   line(item.End()),
							Exported:  ast.IsExported(ident.Name),
						})
					}
				}
			}
		}
	}

	for _, imp := range file.Imports {
		spec := strings.Trim(imp.Path.Value, `"`)
		if spec == "" {
			continue
		}
		target := resolver.ResolveGo(spec)
		parsed.Imports = append(parsed.Imports, db.CodeImport{
			FromPath: path,
			RawSpec:  spec,
			ToPath:   target,
			Kind:     "go-import",
			External: target == "" && !isLocalGoImport(spec, resolver.GoModulePath()),
		})
	}

	parsed.Refs = goRefs(file, declared, path)
	return parsed, true
}

// goRefs collects the names this file uses but does not declare. Restricted to
// call targets, selector tails and composite-literal types on purpose: every
// identifier in the file would include local variables, which collide with
// real symbol names and would turn the callers query into a grep.
func goRefs(file *ast.File, declared map[string]struct{}, path string) []db.CodeRef {
	seen := make(map[string]struct{})
	refs := make([]db.CodeRef, 0)
	add := func(name string) {
		if name == "" || name == "_" {
			return
		}
		if _, ok := declared[name]; ok {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		refs = append(refs, db.CodeRef{Path: path, Name: name})
	}

	ast.Inspect(file, func(node ast.Node) bool {
		switch item := node.(type) {
		case *ast.CallExpr:
			switch fun := item.Fun.(type) {
			case *ast.Ident:
				add(fun.Name)
			case *ast.SelectorExpr:
				add(fun.Sel.Name)
			case *ast.IndexExpr:
				switch expression := fun.X.(type) {
				case *ast.Ident:
					add(expression.Name)
				case *ast.SelectorExpr:
					add(expression.Sel.Name)
				}
			case *ast.IndexListExpr:
				switch expression := fun.X.(type) {
				case *ast.Ident:
					add(expression.Name)
				case *ast.SelectorExpr:
					add(expression.Sel.Name)
				}
			}
		case *ast.SelectorExpr:
			add(item.Sel.Name)
		case *ast.CompositeLit:
			add(typeName(item.Type))
		}
		return true
	})
	return refs
}

func isLocalGoImport(spec, module string) bool {
	return module != "" && (spec == module || strings.HasPrefix(spec, module+"/"))
}

func goTypeKind(spec *ast.TypeSpec) string {
	switch spec.Type.(type) {
	case *ast.InterfaceType:
		return "interface"
	case *ast.StructType:
		return "struct"
	default:
		return "type"
	}
}

func goFuncSignature(decl *ast.FuncDecl, receiver string) string {
	var builder strings.Builder
	builder.WriteString("func ")
	if receiver != "" {
		builder.WriteString("(" + receiver + ") ")
	}
	builder.WriteString(decl.Name.Name)
	builder.WriteString("(")
	if decl.Type.Params != nil {
		parts := make([]string, 0, len(decl.Type.Params.List))
		for _, field := range decl.Type.Params.List {
			parts = append(parts, fieldSignature(field))
		}
		builder.WriteString(strings.Join(parts, ", "))
	}
	builder.WriteString(")")
	if decl.Type.Results != nil && len(decl.Type.Results.List) > 0 {
		parts := make([]string, 0, len(decl.Type.Results.List))
		for _, field := range decl.Type.Results.List {
			parts = append(parts, typeName(field.Type))
		}
		if len(parts) == 1 {
			builder.WriteString(" " + parts[0])
		} else {
			builder.WriteString(" (" + strings.Join(parts, ", ") + ")")
		}
	}
	return builder.String()
}

func fieldSignature(field *ast.Field) string {
	typeText := typeName(field.Type)
	if len(field.Names) == 0 {
		return typeText
	}
	names := make([]string, 0, len(field.Names))
	for _, name := range field.Names {
		names = append(names, name.Name)
	}
	return strings.Join(names, ", ") + " " + typeText
}

// typeName renders enough of a type expression to be recognizable in a
// signature. It deliberately does not reproduce go/printer output: signatures
// here are for reading and for FTS, not for compiling.
func typeName(expr ast.Expr) string {
	switch item := expr.(type) {
	case *ast.Ident:
		return item.Name
	case *ast.StarExpr:
		return "*" + typeName(item.X)
	case *ast.SelectorExpr:
		return typeName(item.X) + "." + item.Sel.Name
	case *ast.ArrayType:
		return "[]" + typeName(item.Elt)
	case *ast.MapType:
		return "map[" + typeName(item.Key) + "]" + typeName(item.Value)
	case *ast.Ellipsis:
		return "..." + typeName(item.Elt)
	case *ast.FuncType:
		return "func"
	case *ast.ChanType:
		return "chan " + typeName(item.Value)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.StructType:
		return "struct{}"
	case *ast.IndexExpr:
		return typeName(item.X) + "[" + typeName(item.Index) + "]"
	default:
		return ""
	}
}

func docText(group *ast.CommentGroup) string {
	if group == nil {
		return ""
	}
	// Only the first sentence-ish worth: a doc comment can be forty lines,
	// and what a caller wants in a symbol listing is the opening claim.
	text := strings.TrimSpace(group.Text())
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, 3)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		out = append(out, line)
		if len(out) == 3 {
			break
		}
	}
	return strings.Join(out, " ")
}

func firstNonEmptyDoc(groups ...*ast.CommentGroup) string {
	for _, group := range groups {
		if text := docText(group); text != "" {
			return text
		}
	}
	return ""
}
