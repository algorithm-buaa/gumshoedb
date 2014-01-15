package gumshoe

import (
	"encoding/json"
	"testing"

	. "github.com/cespare/a"
)

func tableFixture() *FactTable {
	return NewFactTable("", []string{"col1", "col2"})
}

func insertRow(table *FactTable, column1Value Untyped, column2Value Untyped) {
	table.InsertRowMaps([]map[string]Untyped{{"col1": column1Value, "col2": column2Value}})
}

func createQuery() Query {
	query := Query{"", []QueryAggregate{{"sum", "col1", "col1"}}, nil, nil}
	return query
}

func convertToJsonAndBack(o interface{}) interface{} {
	b, err := json.Marshal(o)
	if err != nil {
		panic(err.Error)
	}
	result := new(interface{})
	json.Unmarshal(b, result)
	return *result
}

// A variant of DeepEquals which is less finicky about which numeric type you're using in maps.
func HasEqualJson(args ...interface{}) (ok bool, message string) {
	o1 := convertToJsonAndBack(args[0])
	o2 := convertToJsonAndBack(args[1])
	return DeepEquals(o1, o2)
}

func TestConvertRowMapToRowArrayThrowsErrorForUnrecognizedColumn(t *testing.T) {
	_, error := tableFixture().convertRowMapToRowArray(map[string]Untyped{"col1": 5, "unknownColumn": 10})
	Assert(t, error, NotNil)
}

func createTableFixtureForFilterTests() *FactTable {
	table := tableFixture()
	insertRow(table, 1, "stringvalue1")
	insertRow(table, 2, "stringvalue2")
	return table
}

func runWithFilter(table *FactTable, filter QueryFilter) []map[string]Untyped {
	query := createQuery()
	query.Filters = []QueryFilter{filter}
	return table.InvokeQuery(&query)["results"].([]map[string]Untyped)
}

func runWithGroupBy(table *FactTable, filter QueryGrouping) []map[string]Untyped {
	query := createQuery()
	query.Groupings = []QueryGrouping{filter}
	return table.InvokeQuery(&query)["results"].([]map[string]Untyped)
}

func TestInvokeQueryFiltersRowsUsingEqualsFilter(t *testing.T) {
	table := createTableFixtureForFilterTests()
	results := runWithFilter(table, QueryFilter{"equal", "col1", 2})
	Assert(t, results[0]["col1"], Equals, 2.0)

	results = runWithFilter(table, QueryFilter{"equal", "col2", "stringvalue2"})
	Assert(t, results[0]["col1"], Equals, 2.0)

	// These match zero rows.
	results = runWithFilter(table, QueryFilter{"equal", "col1", 3})
	Assert(t, results[0]["col1"], Equals, 0.0)

	results = runWithFilter(table, QueryFilter{"equal", "col2", "non-existant"})
	Assert(t, results[0]["col1"], Equals, 0.0)
}

func TestInvokeQueryFiltersRowsUsingLessThan(t *testing.T) {
	table := createTableFixtureForFilterTests()
	Assert(t, runWithFilter(table, QueryFilter{"lessThan", "col1", 2})[0]["col1"], Equals, 1.0)
	// Matches zero rows.
	Assert(t, runWithFilter(table, QueryFilter{"lessThan", "col1", 1})[0]["col1"], Equals, 0.0)
}

func TestInvokeQueryFiltersRowsUsingIn(t *testing.T) {
	table := createTableFixtureForFilterTests()
	Assert(t, runWithFilter(table, QueryFilter{"in", "col1", []interface{}{2}})[0]["col1"], Equals, 2.0)
	Assert(t, runWithFilter(table, QueryFilter{"in", "col1", []interface{}{2, 1}})[0]["col1"], Equals, 3.0)
	Assert(t, runWithFilter(table, QueryFilter{"in", "col2", []interface{}{"stringvalue1"}})[0]["col1"],
		Equals, 1.0)
	// These match zero rows.
	Assert(t, runWithFilter(table, QueryFilter{"in", "col2", []interface{}{3}})[0]["col1"], Equals, 0.0)
	Assert(t, runWithFilter(table, QueryFilter{"in", "col2", []interface{}{"non-existant"}})[0]["col1"],
		Equals, 0.0)
}

func TestInvokeQueryWorksWhenGroupingByAStringColumn(t *testing.T) {
	table := tableFixture()
	insertRow(table, 1, "stringvalue1")
	insertRow(table, 2, "stringvalue1")
	insertRow(table, 5, "stringvalue2")
	result := runWithGroupBy(table, QueryGrouping{"", "col2", "groupbykey"})
	Assert(t, result[0], HasEqualJson,
		map[string]Untyped{"groupbykey": "stringvalue1", "rowCount": 2, "col1": 3})
	Assert(t, result[1], HasEqualJson,
		map[string]Untyped{"groupbykey": "stringvalue2", "rowCount": 1, "col1": 5})
}

func TestGroupingWithATimeTransformFunctionWorks(t *testing.T) {
	table := tableFixture()
	// col1 will be truncated into minutes when we group by it, so these rows represent 0 and 2 minutes
	// respectively.
	insertRow(table, 0, "")
	insertRow(table, 120, "")
	insertRow(table, 150, "")
	result := runWithGroupBy(table, QueryGrouping{"minute", "col1", "groupbykey"})
	Assert(t, result[0], HasEqualJson, map[string]Untyped{"groupbykey": 0, "rowCount": 1, "col1": 0})
	Assert(t, result[1], HasEqualJson, map[string]Untyped{"groupbykey": 120, "rowCount": 2, "col1": 270})
}
