package proptools

import (
	"strings"
	"testing"
)

func mustHash(t *testing.T, data interface{}) uint64 {
	t.Helper()
	result, err := CalculateHash(data)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestHashingMapGetsSameResults(t *testing.T) {
	data := map[string]string{"foo": "bar", "baz": "qux"}
	first := mustHash(t, data)
	second := mustHash(t, data)
	third := mustHash(t, data)
	fourth := mustHash(t, data)
	if first != second || second != third || third != fourth {
		t.Fatal("Did not get the same result every time for a map")
	}
}

func TestHashingNonSerializableTypesFails(t *testing.T) {
	testCases := []struct {
		name string
		data interface{}
	}{
		{
			name: "function pointer",
			data: []func(){nil},
		},
		{
			name: "channel",
			data: []chan int{make(chan int)},
		},
		{
			name: "list with non-serializable type",
			data: []interface{}{"foo", make(chan int)},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := CalculateHash(testCase)
			if err == nil {
				t.Fatal("Expected hashing error but didn't get one")
			}
			expected := "data may only contain primitives, strings, arrays, slices, structs, maps, and pointers"
			if !strings.Contains(err.Error(), expected) {
				t.Fatalf("Expected %q, got %q", expected, err.Error())
			}
		})
	}
}

func TestHashSuccessful(t *testing.T) {
	testCases := []struct {
		name string
		data interface{}
	}{
		{
			name: "int",
			data: 5,
		},
		{
			name: "string",
			data: "foo",
		},
		{
			name: "*string",
			data: StringPtr("foo"),
		},
		{
			name: "array",
			data: [3]string{"foo", "bar", "baz"},
		},
		{
			name: "slice",
			data: []string{"foo", "bar", "baz"},
		},
		{
			name: "struct",
			data: struct {
				foo string
				bar int
			}{
				foo: "foo",
				bar: 3,
			},
		},
		{
			name: "map",
			data: map[string]int{
				"foo": 3,
				"bar": 4,
			},
		},
		{
			name: "list of interfaces with different types",
			data: []interface{}{"foo", 3, []string{"bar", "baz"}},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			mustHash(t, testCase.data)
		})
	}
}

func TestHashingDereferencePointers(t *testing.T) {
	str1 := "this is a hash test for pointers"
	str2 := "this is a hash test for pointers"
	data := []struct {
		content *string
	}{
		{content: &str1},
		{content: &str2},
	}
	first := mustHash(t, data[0])
	second := mustHash(t, data[1])
	if first != second {
		t.Fatal("Got different results for the same string")
	}
}
