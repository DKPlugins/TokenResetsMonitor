package config

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"
)

var errFormatBudget = errors.New("template formatting budget exceeded")

// fmt buffers an entire printf result before writing it. Preflight aggregate
// padding, precision and argument expansion so a bounded output writer is not
// bypassed by one enormous intermediate formatted string.
func formatWork(format string, args []any) (int, error) {
	budget := len(format)
	if budget > MaxTemplateBytes {
		return 0, errFormatBudget
	}
	arg := 0
	indexed := false
	index := func(pos *int) error {
		if *pos >= len(format) || format[*pos] != '[' {
			return nil
		}
		indexed = true
		*pos++
		n := 0
		start := *pos
		for *pos < len(format) && format[*pos] >= '0' && format[*pos] <= '9' {
			n = n*10 + int(format[*pos]-'0')
			*pos++
			if n > len(args) {
				return errFormatBudget
			}
		}
		if *pos == start || *pos >= len(format) || format[*pos] != ']' || n < 1 {
			return errFormatBudget
		}
		*pos++
		arg = n - 1
		return nil
	}
	number := func(pos *int) (int, error) {
		n := 0
		for *pos < len(format) && format[*pos] >= '0' && format[*pos] <= '9' {
			n = n*10 + int(format[*pos]-'0')
			*pos++
			if n > MaxTemplateBytes {
				return 0, errFormatBudget
			}
		}
		return n, nil
	}
	dimension := func(pos *int) (int, error) {
		if *pos < len(format) && format[*pos] == '*' {
			*pos++
			if arg >= len(args) {
				return 0, errFormatBudget
			}
			value := reflect.ValueOf(args[arg])
			arg++
			if !value.IsValid() {
				return 0, errFormatBudget
			}
			var n int64
			switch value.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				n = value.Int()
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				if value.Uint() > MaxTemplateBytes {
					return 0, errFormatBudget
				}
				n = int64(value.Uint())
			default:
				return 0, errFormatBudget
			}
			if n < -MaxTemplateBytes || n > MaxTemplateBytes {
				return 0, errFormatBudget
			}
			if n < 0 {
				n = -n
			}
			return int(n), nil
		}
		return number(pos)
	}
	for pos := 0; pos < len(format); {
		if format[pos] != '%' {
			pos++
			continue
		}
		pos++
		for pos < len(format) && strings.ContainsRune("#0+- ", rune(format[pos])) {
			pos++
		}
		if err := index(&pos); err != nil {
			return 0, err
		}
		width, err := dimension(&pos)
		if err != nil {
			return 0, err
		}
		precision := 0
		if pos < len(format) && format[pos] == '.' {
			pos++
			if err := index(&pos); err != nil {
				return 0, err
			}
			precision, err = dimension(&pos)
			if err != nil {
				return 0, err
			}
		}
		if err := index(&pos); err != nil {
			return 0, err
		}
		if pos >= len(format) {
			return 0, errFormatBudget
		}
		verb, size := utf8.DecodeRuneInString(format[pos:])
		pos += size
		if verb == '%' {
			continue
		}
		if arg >= len(args) {
			return 0, errFormatBudget
		}
		sizeBound, leaves := formatValueWork(reflect.ValueOf(args[arg]), 0)
		arg++
		if sizeBound > MaxTemplateBytes || leaves > MaxTemplateBytes/(width+precision+1) {
			return 0, errFormatBudget
		}
		budget += sizeBound + leaves*(width+precision)
		if budget > MaxTemplateBytes {
			return 0, errFormatBudget
		}
	}
	// fmt appends diagnostics for unused arguments unless indices were used.
	if !indexed {
		for ; arg < len(args); arg++ {
			size, _ := formatValueWork(reflect.ValueOf(args[arg]), 0)
			budget += size
			if budget > MaxTemplateBytes {
				return 0, errFormatBudget
			}
		}
	}
	return budget, nil
}

// Only notification values and standard-library values are reachable from
// templates. Count container leaves because fmt applies widths recursively to
// their contents. The estimate deliberately allows room for quoting/type names.
func formatValueWork(v reflect.Value, depth int) (int, int) {
	if !v.IsValid() {
		return 32, 1
	}
	if depth > 64 {
		return MaxTemplateBytes + 1, 1
	}
	if v.CanInterface() {
		if value, ok := v.Interface().(fmt.Stringer); ok {
			if (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil() {
				return 32, 1
			}
			return 6*len(value.String()) + 256, 1
		}
	}
	switch v.Kind() {
	case reflect.String:
		return 6*v.Len() + 128, 1
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return 32, 1
		}
		return formatValueWork(v.Elem(), depth+1)
	case reflect.Array, reflect.Slice, reflect.Struct:
		size, leaves := 128, 0
		count := 0
		if v.Kind() == reflect.Struct {
			count = v.NumField()
		} else {
			count = v.Len()
		}
		for i := 0; i < count; i++ {
			var child reflect.Value
			if v.Kind() == reflect.Struct {
				child = v.Field(i)
			} else {
				child = v.Index(i)
			}
			n, l := formatValueWork(child, depth+1)
			size += n
			leaves += l
			if size > MaxTemplateBytes {
				return size, leaves
			}
		}
		if leaves == 0 {
			leaves = 1
		}
		return size, leaves
	case reflect.Map:
		size, leaves := 128, 0
		it := v.MapRange()
		for it.Next() {
			a, al := formatValueWork(it.Key(), depth+1)
			b, bl := formatValueWork(it.Value(), depth+1)
			size += a + b
			leaves += al + bl
			if size > MaxTemplateBytes {
				return size, leaves
			}
		}
		if leaves == 0 {
			leaves = 1
		}
		return size, leaves
	default:
		return 512, 1
	}
}
