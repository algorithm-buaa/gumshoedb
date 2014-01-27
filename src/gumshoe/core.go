// The core table creation and query execution functions.
package gumshoe

import (
	"fmt"
	"reflect"
	"sort"
	"sync"
	"unsafe"

	mmap "github.com/edsrzf/mmap-go"
)

// The size of the fact table is currently a compile time constant, so we can use native arrays instead of
// ranges. In the future we'll use byte arrays so we that rows can be composites of many column types.
// TODO(philc): Get rid of this Cell data type.
type Cell float32

const (
	Uint8Type = iota
	Int8Type
	Uint16Type
	Int16Type
	Uint32Type
	Int32Type
	Uint64Type
	Int64Type
	Float32Type
	Float64Type
)

var typeSizes = map[int]int{
	Uint8Type:   int(unsafe.Sizeof(*new(uint8))),
	Int8Type:    int(unsafe.Sizeof(*new(int8))),
	Uint16Type:  int(unsafe.Sizeof(*new(uint16))),
	Int16Type:   int(unsafe.Sizeof(*new(int16))),
	Uint32Type:  int(unsafe.Sizeof(*new(uint32))),
	Int32Type:   int(unsafe.Sizeof(*new(int32))),
	Uint64Type:  int(unsafe.Sizeof(*new(uint64))),
	Int64Type:   int(unsafe.Sizeof(*new(int64))),
	Float32Type: int(unsafe.Sizeof(*new(float32))),
	Float64Type: int(unsafe.Sizeof(*new(float64))),
}

type Schema struct {
	NumericColumns map[string]int // name => size
	StringColumns  map[string]int // name => size
}

func NewSchema() *Schema {
	s := new(Schema)
	s.NumericColumns = make(map[string]int)
	s.StringColumns = make(map[string]int)
	return s
}

// A fixed sized table of rows.
// When we insert more rows than the table's capacity, we wrap around and begin inserting rows at index 0.
type FactTable struct {
	// We serialize this struct using JSON. The unexported fields are fields we don't want to serialize.
	rows     []byte
	FilePath string // Path to this table on disk, where we will periodically snapshot it to.
	// TODO(caleb): This is not enough. Reads still race with writes. We need to fix this, possibly by removing
	// the circular writes and instead persisting historic chunks to disk (or deleting them) and allocating
	// fresh tables.
	insertLock *sync.Mutex
	// The mmap bookkeeping object which contains the file descriptor we are mapping the table rows to.
	memoryMap           mmap.MMap
	NextInsertPosition  int
	Count               int               // The number of used rows currently in the table. This is <= ROWS.
	ColumnCount         int               // The number of columns in use in the table. This is <= COLS.
	Capacity            int               // For now, this is an alias for the ROWS constant.
	RowSize             int               // In bytes
	DimensionTables     []*DimensionTable // A mapping from column index => column's DimensionTable.
	ColumnNameToIndex   map[string]int
	ColumnIndexToName   []string
	ColumnIndexToOffset []uintptr // The byte offset of each column from the beggining byte of the row
	ColumnIndexToType   []int     // Index => one of the type constants (e.g. Uint8Type).
}

func (table *FactTable) Rows() []byte {
	return table.rows
}

// A DimensionTable is a mapping of string column values to int IDs, so that the FactTable can store rows of
// integer IDs rather than string values.
type DimensionTable struct {
	Name      string
	Rows      []string
	ValueToId map[string]int32
}

func NewDimensionTable(name string) *DimensionTable {
	return &DimensionTable{
		Name:      name,
		ValueToId: make(map[string]int32),
	}
}

type RowAggregate struct {
	GroupByValue Cell
	Sums         []float64
	Count        int
}

type FactTableFilterFunc func(uintptr) bool

// Returns the keys from a map in sorted order.
func getSortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Allocates a new FactTable. If a non-empty filePath is specified, this table's rows are immediately
// persisted to disk in the form of a memory-mapped file.
// String columns appear first, and then numeric columns, for no particular reason other than
// implementation convenience in a few places.
func NewFactTable(filePath string, rowCount int, schema Schema) *FactTable {
	stringColumnNames := getSortedKeys(schema.StringColumns)
	numericColumnNames := getSortedKeys(schema.NumericColumns)
	allColumnNames := append(stringColumnNames, numericColumnNames...)
	table := &FactTable{
		ColumnCount: len(allColumnNames),
		FilePath:    filePath,
		insertLock:  new(sync.Mutex),
		Capacity:    rowCount,
	}

	columnToType := make(map[string]int)
	for k, v := range schema.StringColumns {
		columnToType[k] = v
	}
	for k, v := range schema.NumericColumns {
		columnToType[k] = v
	}

	// Compute the byte offset from the beginning of the row for each column
	table.ColumnIndexToOffset = make([]uintptr, table.ColumnCount)
	table.ColumnIndexToType = make([]int, table.ColumnCount)
	columnOffset := 0
	for i, name := range allColumnNames {
		table.ColumnIndexToOffset[i] = uintptr(columnOffset)
		table.ColumnIndexToType[i] = columnToType[name]
		columnOffset += typeSizes[columnToType[name]]
	}

	table.DimensionTables = make([]*DimensionTable, len(stringColumnNames))
	for i, column := range stringColumnNames {
		table.DimensionTables[i] = NewDimensionTable(column)
	}

	table.RowSize = columnOffset
	tableSize := table.Capacity * table.RowSize
	if filePath == "" {
		// Create an in-memory database only, without a file backing.
		slice := make([]byte, tableSize)
		table.rows = slice
	} else {
		table.memoryMap, table.rows = CreateMemoryMappedFactTableStorage(table.FilePath, tableSize)
	}

	table.ColumnIndexToName = make([]string, len(allColumnNames))
	table.ColumnNameToIndex = make(map[string]int, len(allColumnNames))
	for i, name := range allColumnNames {
		table.ColumnIndexToName[i] = name
		table.ColumnNameToIndex[name] = i
	}

	return table
}

// Return a set of row maps. Useful for debugging the contents of the table.
func (table *FactTable) GetRowMaps(start, end int) []map[string]Untyped {
	results := make([]map[string]Untyped, 0, table.Count)
	if start > end {
		panic("Invalid row indices passed to GetRowMaps")
	}
	for i := start; i < end; i++ {
		results = append(results, table.DenormalizeRow(table.getRowSlice(i)))
	}
	return results
}

// Given a column value from a row vector, return either the column value if it's a numeric column, or the
// corresponding string if it's a normalized string column.
// E.g. denormalizeColumnValue(213, 1) => "Japan"
func (table *FactTable) denormalizeColumnValue(value Untyped, columnIndex int) Untyped {
	if table.columnUsesDimensionTable(columnIndex) {
		dimensionTable := table.DimensionTables[columnIndex]
		return dimensionTable.Rows[int(convertUntypedToFloat64(value))]
	} else {
		return value
	}
}

// Takes a normalized row vector and returns a map consisting of column names and values pulled from the
// dimension tables.
// e.g. [0, 1, 17] => {"country": "Japan", "browser": "Chrome", "age": 17}
func (table *FactTable) DenormalizeRow(row []byte) map[string]Untyped {
	result := make(map[string]Untyped)
	for i := 0; i < table.ColumnCount; i++ {
		name := table.ColumnIndexToName[i]
		value := table.getColumnValue(row, i)
		result[name] = table.denormalizeColumnValue(value, i)
	}
	return result
}

// Takes a map of column names => values, and returns a vector with the map's values in the correct column
// position according to the table's schema, e.g.:
// e.g. {"country": "Japan", "browser": "Chrome", "age": 17} => ["Chrome", 17, "Japan"]
// Returns an error if there are unrecognized columns, or if a column is missing.
func (table *FactTable) convertRowMapToRowArray(rowMap map[string]Untyped) ([]Untyped, error) {
	result := make([]Untyped, table.ColumnCount)
	for columnName, value := range rowMap {
		columnIndex, found := table.ColumnNameToIndex[columnName]
		if !found {
			return nil, fmt.Errorf("Unrecognized column name: %s", columnName)
		}
		result[columnIndex] = value
	}
	return result, nil
}

func (table *FactTable) getRowOffset(row int) int {
	return row * table.RowSize
}

func (table *FactTable) getRowSlice(row int) []byte {
	rowOffset := table.getRowOffset(row)
	newSlice := table.Rows()[rowOffset : rowOffset+table.RowSize]
	return newSlice
}

func (table *FactTable) columnUsesDimensionTable(columnIndex int) bool {
	// This expression is valid because we put the string columns at the beginning of the row.
	return columnIndex < len(table.DimensionTables)
}

// Takes a row of mixed types and replaces all string columns with the ID of the matching string in the
// corresponding dimension table (creating a new row in the dimension table if the dimension table doesn't
// already contain the string).
// Note that all numeric values are assumed to be float64 (this is what Go's JSON unmarshaller produces).
// e.g. {"country": "Japan", "browser": "Chrome", "age": 17} => [0, 1, 17]
// TODO(philc): Make this return []byte
func (table *FactTable) normalizeRow(rowMap map[string]Untyped) (*[]byte, error) {
	rowAsArray, err := table.convertRowMapToRowArray(rowMap)
	if err != nil {
		return nil, err
	}
	rowSlice := make([]byte, table.RowSize)
	for columnIndex, value := range rowAsArray {
		var valueAsFloat64 float64
		if table.columnUsesDimensionTable(columnIndex) {
			if !isString(value) {
				return nil, fmt.Errorf("Cannot insert a non-string value into column %s.",
					table.ColumnIndexToName[columnIndex])
			}
			stringValue := value.(string)
			dimensionTable := table.DimensionTables[columnIndex]
			dimensionRowId, ok := dimensionTable.ValueToId[stringValue]
			if !ok {
				dimensionRowId = dimensionTable.addRow(stringValue)
			}
			valueAsFloat64 = float64(dimensionRowId)
		} else {
			valueAsFloat64 = value.(float64)
		}
		table.setColumnValue(rowSlice, columnIndex, valueAsFloat64)
	}
	return &rowSlice, nil
}

func (table *FactTable) getColumnValue(row []byte, column int) Untyped {
	rowPtr := uintptr(unsafe.Pointer(&row[0]))
	columnPtr := unsafe.Pointer(rowPtr + table.ColumnIndexToOffset[column])
	switch table.ColumnIndexToType[column] {
	case Uint8Type:
		return *(*uint8)(columnPtr)
	case Int8Type:
		return *(*int8)(columnPtr)
	case Uint16Type:
		return *(*uint16)(columnPtr)
	case Int16Type:
		return *(*int16)(columnPtr)
	case Uint32Type:
		return *(*uint32)(columnPtr)
	case Int32Type:
		return *(*int32)(columnPtr)
	case Uint64Type:
		return *(*uint64)(columnPtr)
	case Int64Type:
		return *(*int64)(columnPtr)
	case Float32Type:
		return *(*float32)(columnPtr)
	case Float64Type:
		return *(*float64)(columnPtr)
	}
	return nil
}

func (table *FactTable) setColumnValue(row []byte, column int, value float64) {
	rowPtr := uintptr(unsafe.Pointer(&row[0]))
	columnPtr := unsafe.Pointer(rowPtr + table.ColumnIndexToOffset[column])
	switch table.ColumnIndexToType[column] {
	case Uint8Type:
		*(*uint8)(columnPtr) = uint8(value)
	case Int8Type:
		*(*int8)(columnPtr) = int8(value)
	case Uint16Type:
		*(*uint16)(columnPtr) = uint16(value)
	case Int16Type:
		*(*int16)(columnPtr) = int16(value)
	case Uint32Type:
		*(*uint32)(columnPtr) = uint32(value)
	case Int32Type:
		*(*int32)(columnPtr) = int32(value)
	case Uint64Type:
		*(*uint64)(columnPtr) = uint64(value)
	case Int64Type:
		*(*int64)(columnPtr) = int64(value)
	case Float32Type:
		*(*float32)(columnPtr) = float32(value)
	case Float64Type:
		*(*float64)(columnPtr) = float64(value)
	}
}

func (table *FactTable) insertNormalizedRow(row *[]byte) {
	copy(table.getRowSlice(table.NextInsertPosition), *row)
	table.NextInsertPosition = (table.NextInsertPosition + 1) % table.Capacity
	if table.Count < table.Capacity {
		table.Count++
	}
}

// Inserts the given rows into the table. Returns an error if one of the rows contains an unrecognized column.
func (table *FactTable) InsertRowMaps(rows []map[string]Untyped) error {
	table.insertLock.Lock()
	defer table.insertLock.Unlock()

	for _, rowMap := range rows {
		normalizedRow, err := table.normalizeRow(rowMap)
		if err != nil {
			return err
		}
		table.insertNormalizedRow(normalizedRow)
	}
	return nil
}

func (table *DimensionTable) addRow(rowValue string) int32 {
	nextId := int32(len(table.Rows))
	table.Rows = append(table.Rows, rowValue)
	table.ValueToId[rowValue] = nextId
	return nextId
}

// Scans all rows in the table, aggregating columns, filtering and grouping rows.
// This logic is performance critical.
// TODO(philc): make the groupByColumnName parameter be an integer, for consistency
func (table *FactTable) scan(filters []FactTableFilterFunc, columnIndices []int,
	groupByColumnName string, groupByColumnTransformFn func(Cell) Cell) []RowAggregate {
	columnIndexToGroupBy, useGrouping := table.ColumnNameToIndex[groupByColumnName]
	// This maps the values of the group-by column => RowAggregate.
	// Due to laziness, only one level of grouping is supported. TODO(philc): Support multiple levels of
	// grouping.
	rowAggregatesMap := make(map[Cell]*RowAggregate)
	// When the query has no group-by clause, we accumulate results into a single RowAggregate.
	rowAggregate := new(RowAggregate)
	rowAggregate.Sums = make([]float64, table.ColumnCount)
	rowCount := table.Count
	columnCountInQuery := len(columnIndices)
	filterCount := len(filters)
	rowPtr := (*reflect.SliceHeader)(unsafe.Pointer(&table.rows)).Data
	rowSize := uintptr(table.RowSize)
	groupByColumnOffset := uintptr(columnIndexToGroupBy * 4)

outerLoop:
	for i := 0; i < rowCount; i++ {
		for filterIndex := 0; filterIndex < filterCount; filterIndex++ {
			if !filters[filterIndex](rowPtr) {
				rowPtr += rowSize
				continue outerLoop
			}
		}

		if useGrouping {
			groupByValue := *(*Cell)(unsafe.Pointer(rowPtr + groupByColumnOffset))
			if groupByColumnTransformFn != nil {
				groupByValue = groupByColumnTransformFn(groupByValue)
			}
			var found bool
			rowAggregate, found = rowAggregatesMap[groupByValue]
			if !found {
				rowAggregate = new(RowAggregate)
				rowAggregate.Sums = make([]float64, table.ColumnCount)
				(*rowAggregate).GroupByValue = groupByValue
				rowAggregatesMap[groupByValue] = rowAggregate
			}
		}

		for j := 0; j < columnCountInQuery; j++ {
			columnIndex := columnIndices[j]
			// TODO(philc): Pre-compute these offsets.
			columnOffset := uintptr(4 * columnIndex)
			columnValue := *(*Cell)(unsafe.Pointer(rowPtr + columnOffset))
			(*rowAggregate).Sums[columnIndex] += float64(columnValue)
		}
		(*rowAggregate).Count++
		rowPtr += rowSize
	}

	results := make([]RowAggregate, 0)
	if useGrouping {
		for _, value := range rowAggregatesMap {
			results = append(results, *value)
		}
	} else {
		results = append(results, *rowAggregate)
	}
	return results
}

// TODO(philc): This function probably be inlined.
func (table *FactTable) getColumnIndicesFromQuery(query *Query) []int {
	columnIndices := make([]int, 0)
	for _, queryAggregate := range query.Aggregates {
		columnIndices = append(columnIndices, table.ColumnNameToIndex[queryAggregate.Column])
	}
	return columnIndices
}

func (table *FactTable) mapRowAggregatesToJSONResultsFormat(query *Query,
	rowAggregates []RowAggregate) [](map[string]Untyped) {
	jsonRows := make([](map[string]Untyped), 0)
	for _, rowAggregate := range rowAggregates {
		jsonRow := make(map[string]Untyped)
		for _, queryAggregate := range query.Aggregates {
			columnIndex := table.ColumnNameToIndex[queryAggregate.Column]
			// TODO(philc): Change this to an enum
			sums := rowAggregate.Sums[columnIndex]
			if queryAggregate.Type == "sum" {
				jsonRow[queryAggregate.Name] = sums
			} else if queryAggregate.Type == "average" {
				jsonRow[queryAggregate.Name] = sums / float64(rowAggregate.Count)
			}
		}
		// TODO(philc): This code does not handle multi-level groupings.
		for _, grouping := range query.Groupings {
			columnIndex := table.ColumnNameToIndex[grouping.Column]
			jsonRow[grouping.Name] = table.denormalizeColumnValue(rowAggregate.GroupByValue, columnIndex)
		}
		jsonRow["rowCount"] = rowAggregate.Count
		jsonRows = append(jsonRows, jsonRow)
	}
	return jsonRows
}

// Given a list of values, looks up the corresponding row IDs for those values. If those values don't
// exist in the dimension table, they're omitted.
func (table *DimensionTable) getDimensionRowIdsForValues(values []string) []Cell {
	rowIds := make([]Cell, 0)
	for _, value := range values {
		if id, ok := table.ValueToId[value]; ok {
			rowIds = append(rowIds, Cell(id))
		}
	}
	return rowIds
}

// Given a QueryFilter, return a filter function that can be tested against a row.
func convertQueryFilterToFilterFunc(queryFilter QueryFilter, table *FactTable) FactTableFilterFunc {
	columnIndex := table.ColumnNameToIndex[queryFilter.Column]
	var f FactTableFilterFunc

	// The query value can either be a single value (in the case of =, >, < queries) or an array of values (in
	// the case of "in", "not in" queries.
	var valueAsCell Cell
	var valueAsCells []Cell

	queryValueIsList := queryFilter.Type == "in"

	if queryValueIsList {
		untypedQueryValues := queryFilter.Value.([]interface{})
		shouldTranslateToDimensionColumnIds := len(untypedQueryValues) > 0 && isString(untypedQueryValues[0])
		if shouldTranslateToDimensionColumnIds {
			// Convert this slice of untyped objects to []string. We encounter a panic if we try to cast straight
			// to []string; I'm not sure why.
			queryValuesAstrings := make([]string, 0, len(untypedQueryValues))
			for _, value := range untypedQueryValues {
				queryValuesAstrings = append(queryValuesAstrings, value.(string))
			}
			dimensionTable := table.DimensionTables[columnIndex]
			valueAsCells = dimensionTable.getDimensionRowIdsForValues(queryValuesAstrings)
		} else {
			valueAsCells = make([]Cell, 0, len(untypedQueryValues))
			for _, value := range untypedQueryValues {
				valueAsCells = append(valueAsCells, (Cell(convertUntypedToFloat64(value))))
			}
		}
	} else {
		if isString(queryFilter.Value) {
			dimensionTable := table.DimensionTables[columnIndex]
			matchingRowIds := dimensionTable.getDimensionRowIdsForValues([]string{queryFilter.Value.(string)})
			if len(matchingRowIds) == 0 {
				return func(row uintptr) bool { return false }
			} else {
				valueAsCell = matchingRowIds[0]
			}
		} else {
			valueAsCell = Cell(convertUntypedToFloat64(queryFilter.Value))
		}
	}

	columnOffset := table.ColumnIndexToOffset[columnIndex]

	switch queryFilter.Type {
	case "greaterThan", ">":
		f = func(row uintptr) bool {
			columnValue := *(*Cell)(unsafe.Pointer(row + columnOffset))
			return columnValue > valueAsCell
		}
	case "greaterThanOrEqualTo", ">=":
		f = func(row uintptr) bool {
			columnValue := *(*Cell)(unsafe.Pointer(row + columnOffset))
			return columnValue >= valueAsCell
		}
	case "lessThan", "<":
		f = func(row uintptr) bool {
			columnValue := *(*Cell)(unsafe.Pointer(row + columnOffset))
			return columnValue < valueAsCell
		}
	case "lessThanOrEqualTo", "<=":
		f = func(row uintptr) bool {
			columnValue := *(*Cell)(unsafe.Pointer(row + columnOffset))
			return columnValue <= valueAsCell
		}
	case "notequal", "!=":
		f = func(row uintptr) bool {
			columnValue := *(*Cell)(unsafe.Pointer(row + columnOffset))
			return columnValue != valueAsCell
		}
	case "equal", "=":
		f = func(row uintptr) bool {
			columnValue := *(*Cell)(unsafe.Pointer(row + columnOffset))
			return columnValue == valueAsCell
		}
	case "in":
		count := len(valueAsCells)
		// TODO(philc): A hash table may be more efficient for longer lists. We should determine what that list
		// size is and use a hash table in that case.
		f = func(row uintptr) bool {
			columnValue := *(*Cell)(unsafe.Pointer(row + columnOffset))
			for i := 0; i < count; i++ {
				if columnValue == valueAsCells[i] {
					return true
				}
			}
			return false
		}
	}
	return f
}

// Returns a function which, given a cell, performs a date-truncation transformation.
// - transformFunctionName: one of [minute, hour, day].
func convertTimeTransformToFunc(transformFunctionName string) func(Cell) Cell {
	var divisor int
	switch transformFunctionName {
	case "minute":
		divisor = 60
	case "hour":
		divisor = 60 * 60
	case "day":
		divisor = 60 * 60 * 24
	}
	return func(cell Cell) Cell {
		cellInt := int(cell)
		remainder := cellInt % divisor
		return Cell(cellInt - remainder)
	}
}

func (table *FactTable) InvokeQuery(query *Query) map[string]Untyped {
	columnIndices := table.getColumnIndicesFromQuery(query)
	var groupByColumn string
	var groupByTransformFunc func(Cell) Cell
	// NOTE(philc): For now, only support one level of grouping. We intend to support multiple levels.
	if len(query.Groupings) > 0 {
		grouping := query.Groupings[0]
		groupByColumn = grouping.Column
		if grouping.TimeTransform != "" {
			groupByTransformFunc = convertTimeTransformToFunc(grouping.TimeTransform)
		}
	}

	filterFuncs := make([]FactTableFilterFunc, 0, len(query.Filters))
	for _, queryFilter := range query.Filters {
		filterFuncs = append(filterFuncs, convertQueryFilterToFilterFunc(queryFilter, table))
	}

	results := table.scan(filterFuncs, columnIndices, groupByColumn, groupByTransformFunc)
	jsonResultRows := table.mapRowAggregatesToJSONResultsFormat(query, results)
	return map[string]Untyped{
		"results": jsonResultRows,
	}
}

func isString(value interface{}) bool {
	result := false
	switch value.(type) {
	case string:
		result = true
	}
	return result
}

func convertUntypedToFloat64(v Untyped) float64 {
	var result float64
	switch v.(type) {
	case float32:
		result = float64(v.(float32))
	case float64:
		result = v.(float64)
	case uint8:
		result = float64(v.(uint8))
	case uint16:
		result = float64(v.(uint16))
	case uint32:
		result = float64(v.(uint32))
	case uint64:
		result = float64(v.(uint64))
	case int:
		result = float64(v.(int))
	case int8:
		result = float64(v.(int8))
	case int32:
		result = float64(v.(int32))
	case int64:
		result = float64(v.(int64))
	case Cell:
		result = float64(v.(Cell))
	default:
		panic(fmt.Sprintf("Unrecognized type: %s", reflect.TypeOf(v)))
	}
	return result
}
