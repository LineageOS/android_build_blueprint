package proptools

import (
	"fmt"
	"reflect"
	"testing"
)

func TestPostProcessor(t *testing.T) {
	// Same as the ascii art example in Configurable.evaluate()
	prop := NewConfigurable[[]string](nil, nil)
	prop.AppendSimpleValue([]string{"a"})
	prop.AppendSimpleValue([]string{"b"})
	prop.AddPostProcessor(addToElements("1"))

	prop2 := NewConfigurable[[]string](nil, nil)
	prop2.AppendSimpleValue([]string{"c"})

	prop3 := NewConfigurable[[]string](nil, nil)
	prop3.AppendSimpleValue([]string{"d"})
	prop3.AppendSimpleValue([]string{"e"})
	prop3.AddPostProcessor(addToElements("2"))

	prop4 := NewConfigurable[[]string](nil, nil)
	prop4.AppendSimpleValue([]string{"f"})

	prop5 := NewConfigurable[[]string](nil, nil)
	prop5.AppendSimpleValue([]string{"g"})
	prop5.AddPostProcessor(addToElements("3"))

	prop2.Append(prop3)
	prop2.AddPostProcessor(addToElements("z"))

	prop.Append(prop2)
	prop.AddPostProcessor(addToElements("y"))
	prop.Append(prop4)
	prop.Append(prop5)

	expected := []string{"a1y", "b1y", "czy", "d2zy", "e2zy", "f", "g3"}
	x := prop.Get(&configurableEvalutorForTesting{})
	if !reflect.DeepEqual(x.Get(), expected) {
		t.Fatalf("Expected %v, got %v", expected, x.Get())
	}
}

func addToElements(s string) func([]string) []string {
	return func(arr []string) []string {
		for i := range arr {
			arr[i] = arr[i] + s
		}
		return arr
	}
}

type configurableEvalutorForTesting struct {
	vars map[string]string
}

func (e *configurableEvalutorForTesting) EvaluateConfiguration(condition ConfigurableCondition, property string) ConfigurableValue {
	if condition.functionName != "f" {
		panic("Expected functionName to be f")
	}
	if len(condition.args) != 1 {
		panic("Expected exactly 1 arg")
	}
	val, ok := e.vars[condition.args[0]]
	if ok {
		return ConfigurableValueString(val)
	}
	return ConfigurableValueUndefined()
}

func (e *configurableEvalutorForTesting) PropertyErrorf(property, fmtString string, args ...interface{}) {
	panic(fmt.Sprintf(fmtString, args...))
}

var _ ConfigurableEvaluator = (*configurableEvalutorForTesting)(nil)
