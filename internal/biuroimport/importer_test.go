package biuroimport

import (
	"reflect"
	"testing"
)

func TestUniqueStringsAndMaskPhone(t *testing.T) {
	got := uniqueStrings([]string{"b", "a", "b", "", "c"})
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("uniqueStrings() = %#v, want %#v", got, want)
	}
	if got := maskPhone("+48698631622"); got != "+4869***622" {
		t.Fatalf("maskPhone() = %q", got)
	}
}
