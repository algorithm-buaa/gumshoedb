// +build ignore

// This file generates Go source code for various types and type/query-function combinations.
//
// Invoke as
//
//		go run gen.go | gofmt > types_gen.go
//
package main

import (
	"log"
	"os"
	"strings"
	"text/template"
)

var types = []string{
	"uint8",
	"int8",
	"uint16",
	"int16",
	"uint32",
	"int32",
	"float32",
}

var filters = []FilterType{
	{"FilterEqual", "=", "=="},
	{"FilterNotEqual", "!=", "!="},
	{"FilterGreaterThan", ">", ">"},
	{"FilterGreaterThenOrEqual", ">=", ">="},
	{"FilterLessThan", "<", "<"},
	{"FilterLessThanOrEqual", "<=", "<="},
	{"FilterIn", "in", ""},
}

type Type struct {
	GoName          string // uint8
	TitleName       string // Uint8
	GumshoeTypeName string // TypeUint8
}

// Simple filters have a corresponding Go operator
type FilterType struct {
	GumshoeTypeName string // FilterEqual
	Symbol          string // =
	GoOperator      string // ==
}

func main() {
	elements := struct {
		Types             []Type
		IntTypes          []Type
		UintTypes         []Type
		FloatTypes        []Type
		FilterTypes       []FilterType
		SimpleFilterTypes []FilterType // Binary op filters
	}{FilterTypes: filters}

	for _, name := range types {
		typ := Type{
			GoName:          name,
			TitleName:       strings.Title(name),
			GumshoeTypeName: "Type" + strings.Title(name),
		}
		if strings.HasPrefix(name, "int") || strings.HasPrefix(name, "uint") {
			elements.IntTypes = append(elements.IntTypes, typ)
		}
		if strings.HasPrefix(name, "uint") {
			elements.UintTypes = append(elements.UintTypes, typ)
		}
		if strings.HasPrefix(name, "float") {
			elements.FloatTypes = append(elements.FloatTypes, typ)
		}
		elements.Types = append(elements.Types, typ)
	}

	for _, filter := range filters {
		if filter.GoOperator != "" {
			elements.SimpleFilterTypes = append(elements.SimpleFilterTypes, filter)
		}
	}

	if err := tmpl.Execute(os.Stdout, elements); err != nil {
		log.Fatal(err)
	}
}

var tmpl = template.Must(template.New("source").Parse(sourceTemplate))

const sourceTemplate = `
// WARNING: AUTOGENERATED CODE
// Do not edit by hand (see gen.go).

package gumshoe

import (
	"math"
	"unsafe"
)

type Type int

const ( {{range .Types}}
{{.GumshoeTypeName}} Type = iota{{end}}
)

var typeWidths = []int{ {{range .Types}}
{{.GumshoeTypeName}}: int(unsafe.Sizeof({{.GoName}}(0))),{{end}}
}

var typeMaxes = []float64{ {{range .Types}}
{{.GumshoeTypeName}}: math.Max{{.TitleName}},{{end}}
}

var typeNames = []string{ {{range .Types}}
{{.GumshoeTypeName}}: "{{.GoName}}",{{end}}
}

var NameToType = map[string]Type{ {{range .Types}}
"{{.GoName}}": {{.GumshoeTypeName}},{{end}}
}

// add adds other to m (only m is modified).
func (m MetricBytes) add(s *Schema, other MetricBytes) {
	p1 := uintptr(unsafe.Pointer(&m[0]))
	p2 := uintptr(unsafe.Pointer(&other[0]))
	for i, column := range s.MetricColumns {
		offset := uintptr(s.MetricOffsets[i])
		col1 := unsafe.Pointer(p1 + offset)
		col2 := unsafe.Pointer(p2 + offset)
		switch column.Type { {{range .Types}}
		case {{.GumshoeTypeName}}:
			*(*{{.GoName}})(col1) = *(*{{.GoName}})(col1) + (*(*{{.GoName}})(col2)){{end}}
		}
	}
}

func setRowValue(pos unsafe.Pointer, typ Type, value float64) {
	switch typ { {{range .Types}}
	case {{.GumshoeTypeName}}:
		*(*{{.GoName}})(pos) = {{.GoName}}(value){{end}}
	}
}


// numericCellValue decodes a numeric value from cell based on typ. It does not look into any dimension
// tables.
func (s *State) numericCellValue(cell unsafe.Pointer, typ Type) Untyped {
	switch typ { {{range .Types}}
	case {{.GumshoeTypeName}}:
		return *(*{{.GoName}})(cell){{end}}
	}
	panic("unexpected type")
}

// UntypedToFloat64 converts u to a float, if it is some int or float type. Otherwise, it panics.
func UntypedToFloat64(u Untyped) float64 {
	switch n := u.(type) { {{range .Types}}
	case {{.GoName}}:
		return float64(n){{end}}
	}
	panic("unexpected type")
}

// UntypedToInt converts u to an int, if it is some integer type. Otherwise, it panics.
func UntypedToInt(u Untyped) int {
	switch n := u.(type) { {{range .IntTypes}}
	case {{.GoName}}:
		return int(n){{end}}
	}
	panic("not an integer type")
}

// Query helper functions

type FilterType int

const ( {{range .FilterTypes}}
{{.GumshoeTypeName}} FilterType = iota{{end}}
)

var filterTypeToName = []string{ {{range .FilterTypes}}
{{.GumshoeTypeName}}: "{{.Symbol}}",{{end}}
}

var filterNameToType = map[string]FilterType{ {{range .FilterTypes}}
"{{.Symbol}}": {{.GumshoeTypeName}},{{end}}
}

var typeToSumFunc = map[Type]func(offset int) sumFunc{ {{range .Types}}
	{{.GumshoeTypeName}}: func(offset int) sumFunc {
		return func(sum UntypedBytes, metrics MetricBytes) {
			*(*uint32)(unsafe.Pointer(&sum[0])) += *(*uint32)(unsafe.Pointer(&metrics[offset]))
		}
	},{{end}}
}

type typeAndFilter struct {
	Type Type
	Filter FilterType
}

var typeAndFilterToNilFilterFuncSimple = map[typeAndFilter]func(nilOffset int, mask byte) filterFunc{
{{range $type := .Types}}{{range $filter := $.SimpleFilterTypes}}
typeAndFilter{ {{$type.GumshoeTypeName}}, {{$filter.GumshoeTypeName}} }:
func(nilOffset int, mask byte) filterFunc {
	return func(row RowBytes) bool {
		// See comparison truth table
		if row[nilOffset] & mask > 0 {
			return {{if eq $filter.Symbol "="}} true {{else}} false {{end}}
		}
		return {{if eq $filter.Symbol "!="}} true {{else}} false {{end}}
	}
},{{end}}{{end}}
}

var typeAndFilterToStringFilterFuncSimple = map[typeAndFilter]func(uint32, int, byte, int) filterFunc{
{{range $type := .UintTypes}}{{range $filter := $.SimpleFilterTypes}}
typeAndFilter{ {{$type.GumshoeTypeName}}, {{$filter.GumshoeTypeName}} }:
func(index uint32, nilOffset int, mask byte, valueOffset int) filterFunc {
	v := {{$type.GoName}}(index)
	return func(row RowBytes) bool {
		if row[nilOffset] & mask > 0 {
			return {{if eq $filter.Symbol "!="}} true {{else}} false {{end}}
		}
		return *(*{{$type.GoName}})(unsafe.Pointer(&row[valueOffset])) {{$filter.GoOperator}} v
	}
},{{end}}{{end}}
}

var typeAndFilterToDimensionFilterFuncSimple = map[typeAndFilter]func(float64, int, byte, int) filterFunc{
{{range $type := .Types}}{{range $filter := $.SimpleFilterTypes}}
typeAndFilter{ {{$type.GumshoeTypeName}}, {{$filter.GumshoeTypeName}} }:
func(value float64, nilOffset int, mask byte, valueOffset int) filterFunc {
	v := {{$type.GoName}}(value)
	return func(row RowBytes) bool {
		if row[nilOffset] & mask > 0 {
			return {{if eq $filter.Symbol "!="}} true {{else}} false {{end}}
		}
		return *(*{{$type.GoName}})(unsafe.Pointer(&row[valueOffset])) {{$filter.GoOperator}} v
	}
},{{end}}{{end}}
}

var typeAndFilterToMetricFilterFuncSimple = map[typeAndFilter]func(value float64, offset int) filterFunc{
{{range $type := .Types}}{{range $filter := $.SimpleFilterTypes}}
typeAndFilter{ {{$type.GumshoeTypeName}}, {{$filter.GumshoeTypeName}} }:
func(value float64, offset int) filterFunc {
	v := {{$type.GoName}}(value)
	return func(row RowBytes) bool {
		return *(*{{$type.GoName}})(unsafe.Pointer(&row[offset])) {{$filter.GoOperator}} v
	}
},{{end}}{{end}}
}
`
