package graphql

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/vuka-lang/web/v2/graphql/language"
)

// The specification's scalars. Resolvers get Int as int (32-bit), Float as
// float64, String and ID as string, Boolean as bool; each serializes the
// Go values that hold it without loss: any integer kind within 32 bits for
// Int, any number for Float, a string or an integer for ID.
var (
	Int = &Scalar{
		Name:         "Int",
		Description:  "The `Int` scalar type represents non-fractional signed whole numeric values. Int can represent values between -(2^31) and 2^31 - 1.",
		Serialize:    serializeInt,
		ParseValue:   parseIntValue,
		ParseLiteral: parseIntLiteral,
	}
	Float = &Scalar{
		Name:         "Float",
		Description:  "The `Float` scalar type represents signed double-precision fractional values as specified by [IEEE 754](https://en.wikipedia.org/wiki/IEEE_floating_point).",
		Serialize:    serializeFloat,
		ParseValue:   parseFloatValue,
		ParseLiteral: parseFloatLiteral,
	}
	String = &Scalar{
		Name:         "String",
		Description:  "The `String` scalar type represents textual data, represented as UTF-8 character sequences. The String type is most often used by GraphQL to represent free-form human-readable text.",
		Serialize:    serializeString,
		ParseValue:   parseStringValue,
		ParseLiteral: parseStringLiteral,
	}
	Boolean = &Scalar{
		Name:         "Boolean",
		Description:  "The `Boolean` scalar type represents `true` or `false`.",
		Serialize:    serializeBoolean,
		ParseValue:   parseBooleanValue,
		ParseLiteral: parseBooleanLiteral,
	}
	ID = &Scalar{
		Name:         "ID",
		Description:  "The `ID` scalar type represents a unique identifier, often used to refetch an object or as key for a cache. The ID type appears in a JSON response as a String; however, it is not intended to be human-readable. When expected as an input type, any string (such as `\"4\"`) or integer (such as `4`) input value will be accepted as an ID.",
		Serialize:    serializeID,
		ParseValue:   parseIDValue,
		ParseLiteral: parseIDLiteral,
	}
)

// inspect writes a value as a message quotes it: JSON for what came as JSON.
func inspect(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case json.Number:
		return string(x)
	case language.Value:
		return language.Print(x)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

const maxInt32, minInt32 = math.MaxInt32, math.MinInt32

// asInt64 reads any Go integer kind; ok false for other values.
func asInt64(v any) (n int64, ok, overflow bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true, false
	case int8:
		return int64(x), true, false
	case int16:
		return int64(x), true, false
	case int32:
		return int64(x), true, false
	case int64:
		return x, true, false
	case uint:
		return int64(x), true, uint64(x) > math.MaxInt64
	case uint8:
		return int64(x), true, false
	case uint16:
		return int64(x), true, false
	case uint32:
		return int64(x), true, false
	case uint64:
		return int64(x), true, x > math.MaxInt64
	case uintptr:
		return int64(x), true, uint64(x) > math.MaxInt64
	}
	return 0, false, false
}

func serializeInt(v any) (any, error) {
	if n, ok, over := asInt64(v); ok {
		if over || n > maxInt32 || n < minInt32 {
			return nil, fmt.Errorf("Int cannot represent non 32-bit signed integer value: %v", v)
		}
		return int(n), nil
	}
	var f float64
	switch x := v.(type) {
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case float32:
		f = float64(x)
	case float64:
		f = x
	case json.Number:
		var err error
		if f, err = x.Float64(); err != nil {
			return nil, fmt.Errorf("Int cannot represent non-integer value: %s", inspect(v))
		}
	case string:
		var err error
		if f, err = strconv.ParseFloat(x, 64); err != nil || x == "" {
			return nil, fmt.Errorf("Int cannot represent non-integer value: %s", inspect(v))
		}
	default:
		return nil, fmt.Errorf("Int cannot represent non-integer value: %s", inspect(v))
	}
	if f != math.Trunc(f) || math.IsInf(f, 0) || math.IsNaN(f) {
		return nil, fmt.Errorf("Int cannot represent non-integer value: %s", inspect(v))
	}
	if f > maxInt32 || f < minInt32 {
		return nil, fmt.Errorf("Int cannot represent non 32-bit signed integer value: %s", inspect(v))
	}
	return int(f), nil
}

func parseIntValue(v any) (any, error) {
	var f float64
	switch x := v.(type) {
	case json.Number:
		if n, err := x.Int64(); err == nil {
			if n > maxInt32 || n < minInt32 {
				return nil, fmt.Errorf("Int cannot represent non 32-bit signed integer value: %s", x)
			}
			return int(n), nil
		}
		var err error
		if f, err = x.Float64(); err != nil {
			return nil, fmt.Errorf("Int cannot represent non-integer value: %s", inspect(v))
		}
	case float64:
		f = x
	default:
		if n, ok, over := asInt64(v); ok {
			if over || n > maxInt32 || n < minInt32 {
				return nil, fmt.Errorf("Int cannot represent non 32-bit signed integer value: %v", v)
			}
			return int(n), nil
		}
		return nil, fmt.Errorf("Int cannot represent non-integer value: %s", inspect(v))
	}
	if f != math.Trunc(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("Int cannot represent non-integer value: %s", inspect(v))
	}
	if f > maxInt32 || f < minInt32 {
		return nil, fmt.Errorf("Int cannot represent non 32-bit signed integer value: %s", inspect(v))
	}
	return int(f), nil
}

func parseIntLiteral(v language.Value) (any, error) {
	lit, ok := v.(*language.IntValue)
	if !ok {
		return nil, fmt.Errorf("Int cannot represent non-integer value: %s", language.Print(v))
	}
	n, err := strconv.ParseInt(lit.Value, 10, 64)
	if err != nil || n > maxInt32 || n < minInt32 {
		return nil, fmt.Errorf("Int cannot represent non 32-bit signed integer value: %s", lit.Value)
	}
	return int(n), nil
}

func serializeFloat(v any) (any, error) {
	if n, ok, _ := asInt64(v); ok {
		return float64(n), nil
	}
	var f float64
	switch x := v.(type) {
	case bool:
		if x {
			return 1.0, nil
		}
		return 0.0, nil
	case float32:
		f = float64(x)
	case float64:
		f = x
	case json.Number:
		var err error
		if f, err = x.Float64(); err != nil {
			return nil, fmt.Errorf("Float cannot represent non numeric value: %s", inspect(v))
		}
	case string:
		var err error
		if f, err = strconv.ParseFloat(x, 64); err != nil || x == "" {
			return nil, fmt.Errorf("Float cannot represent non numeric value: %s", inspect(v))
		}
	default:
		return nil, fmt.Errorf("Float cannot represent non numeric value: %s", inspect(v))
	}
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return nil, fmt.Errorf("Float cannot represent non numeric value: %v", f)
	}
	return f, nil
}

func parseFloatValue(v any) (any, error) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		if err != nil || math.IsInf(f, 0) {
			return nil, fmt.Errorf("Float cannot represent non numeric value: %s", x)
		}
		return f, nil
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return nil, fmt.Errorf("Float cannot represent non numeric value: %v", x)
		}
		return x, nil
	case float32:
		return float64(x), nil
	}
	if n, ok, _ := asInt64(v); ok {
		return float64(n), nil
	}
	return nil, fmt.Errorf("Float cannot represent non numeric value: %s", inspect(v))
}

func parseFloatLiteral(v language.Value) (any, error) {
	var text string
	switch lit := v.(type) {
	case *language.IntValue:
		text = lit.Value
	case *language.FloatValue:
		text = lit.Value
	default:
		return nil, fmt.Errorf("Float cannot represent non numeric value: %s", language.Print(v))
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(f, 0) {
		return nil, fmt.Errorf("Float cannot represent non numeric value: %s", text)
	}
	return f, nil
}

func serializeString(v any) (any, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case float64:
		if !math.IsInf(x, 0) && !math.IsNaN(x) {
			return strconv.FormatFloat(x, 'g', -1, 64), nil
		}
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32), nil
	case json.Number:
		return string(x), nil
	case fmt.Stringer:
		return x.String(), nil
	}
	if n, ok, _ := asInt64(v); ok {
		if u, isU := v.(uint64); isU {
			return strconv.FormatUint(u, 10), nil
		}
		return strconv.FormatInt(n, 10), nil
	}
	return nil, fmt.Errorf("String cannot represent value: %s", inspect(v))
}

func parseStringValue(v any) (any, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	return nil, fmt.Errorf("String cannot represent a non string value: %s", inspect(v))
}

func parseStringLiteral(v language.Value) (any, error) {
	if s, ok := v.(*language.StringValue); ok {
		return s.Value, nil
	}
	return nil, fmt.Errorf("String cannot represent a non string value: %s", language.Print(v))
}

func serializeBoolean(v any) (any, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case float64:
		if !math.IsInf(x, 0) && !math.IsNaN(x) {
			return x != 0, nil
		}
	}
	if n, ok, _ := asInt64(v); ok {
		return n != 0, nil
	}
	return nil, fmt.Errorf("Boolean cannot represent a non boolean value: %s", inspect(v))
}

func parseBooleanValue(v any) (any, error) {
	if b, ok := v.(bool); ok {
		return b, nil
	}
	return nil, fmt.Errorf("Boolean cannot represent a non boolean value: %s", inspect(v))
}

func parseBooleanLiteral(v language.Value) (any, error) {
	if b, ok := v.(*language.BooleanValue); ok {
		return b.Value, nil
	}
	return nil, fmt.Errorf("Boolean cannot represent a non boolean value: %s", language.Print(v))
}

func serializeID(v any) (any, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case json.Number:
		if _, err := x.Int64(); err == nil {
			return string(x), nil
		}
	case fmt.Stringer:
		return x.String(), nil
	}
	if n, ok, _ := asInt64(v); ok {
		if u, isU := v.(uint64); isU {
			return strconv.FormatUint(u, 10), nil
		}
		return strconv.FormatInt(n, 10), nil
	}
	if f, ok := v.(float64); ok && f == math.Trunc(f) && !math.IsInf(f, 0) {
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	}
	return nil, fmt.Errorf("ID cannot represent value: %s", inspect(v))
}

func parseIDValue(v any) (any, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case json.Number:
		if _, err := x.Int64(); err == nil {
			return string(x), nil
		}
	}
	if n, ok, _ := asInt64(v); ok {
		return strconv.FormatInt(n, 10), nil
	}
	return nil, fmt.Errorf("ID cannot represent value: %s", inspect(v))
}

func parseIDLiteral(v language.Value) (any, error) {
	switch lit := v.(type) {
	case *language.StringValue:
		return lit.Value, nil
	case *language.IntValue:
		return lit.Value, nil
	}
	return nil, fmt.Errorf("ID cannot represent a non-string and non-integer value: %s", language.Print(v))
}
