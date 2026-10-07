// Extract JSON configuration fields from the pinned core source for Monaco.
package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

type schema = map[string]any

var types = map[string]ast.Expr{}

func convert(expr ast.Expr) schema {
	switch v := expr.(type) {
	case *ast.StarExpr:
		return convert(v.X)
	case *ast.Ident:
		switch v.Name {
		case "string", "Address", "TransportProtocol":
			return schema{"type": "string"}
		case "bool":
			return schema{"type": "boolean"}
		case "int", "int32", "uint32", "int64", "uint64", "uint16", "uint8", "byte":
			return schema{"type": "integer"}
		case "float32", "float64":
			return schema{"type": "number"}
		case "Int32Range", "PortList", "PortRange", "Bandwidth", "Duration":
			return schema{"type": []string{"integer", "string"}}
		case "StringList", "NetworkList":
			return schema{"anyOf": []any{schema{"type": "string"}, schema{"type": "array", "items": schema{"type": "string"}}}}
		}
		if _, ok := types[v.Name]; ok {
			return schema{"$ref": "#/definitions/RemnacustCore" + v.Name}
		}
	case *ast.ArrayType:
		if ident, ok := v.Elt.(*ast.Ident); ok && ident.Name == "byte" {
			return schema{"anyOf": []any{schema{"type": "string"}, schema{"type": "array", "items": schema{"type": "integer", "minimum": 0, "maximum": 255}}}}
		}
		return schema{"type": "array", "items": convert(v.Elt)}
	case *ast.MapType:
		return schema{"type": "object", "additionalProperties": convert(v.Value)}
	case *ast.StructType:
		props := schema{}
		for _, field := range v.Fields.List {
			if field.Tag == nil {
				continue
			}
			tag, err := strconv.Unquote(field.Tag.Value)
			if err != nil {
				panic(err)
			}
			name := strings.Split(reflect.StructTag(tag).Get("json"), ",")[0]
			if name != "" && name != "-" {
				props[name] = convert(field.Type)
			}
		}
		return schema{"type": "object", "properties": props, "additionalProperties": true}
	}
	return schema{}
}

func main() {
	files, err := filepath.Glob(filepath.Join(os.Args[1], "*.go"))
	if err != nil {
		panic(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			panic(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if t, ok := node.(*ast.TypeSpec); ok {
				types[t.Name.Name] = t.Type
			}
			return true
		})
	}
	definitions := schema{}
	for name, expr := range types {
		definitions["RemnacustCore"+name] = convert(expr)
	}
	data, err := json.MarshalIndent(definitions, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(os.Args[2], data, 0644); err != nil {
		panic(err)
	}
}
