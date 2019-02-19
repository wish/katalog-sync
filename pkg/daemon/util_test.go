package daemon

import (
	"reflect"
	"strconv"
	"testing"
)

func TestParseMap(t *testing.T) {
	tests := []struct {
		in  string
		out map[string]string
	}{
		// Basic well formed
		{
			in:  "a:1,b:2",
			out: map[string]string{"a": "1", "b": "2"},
		},

		// With some spaces
		{
			in:  "a:1, b:2",
			out: map[string]string{"a": "1", "b": "2"},
		},

		// With some invalid mappings
		{
			in:  "a:1,b",
			out: map[string]string{"a": "1"},
		},
	}

	for i, test := range tests {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			m := ParseMap(test.in)
			if !reflect.DeepEqual(m, test.out) {
				t.Fatalf("Mismatch expected=%v acutal=%v", test.out, m)
			}
		})
	}
}
