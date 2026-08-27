package process

import (
	"fmt"
	"reflect"
	"testing"
)

func TestRingBufferEmpty(t *testing.T) {
	r := newRingBuffer(4)
	if got := r.Last(0); len(got) != 0 {
		t.Errorf("Last(0) on empty = %v, want empty", got)
	}
	if got := r.Last(10); len(got) != 0 {
		t.Errorf("Last(10) on empty = %v, want empty", got)
	}
}

func TestRingBufferPartiallyFilled(t *testing.T) {
	r := newRingBuffer(4)
	r.Append("a")
	r.Append("b")

	if got := r.Last(0); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("Last(0) = %v, want [a b]", got)
	}
	if got := r.Last(1); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("Last(1) = %v, want [b]", got)
	}
	if got := r.Last(10); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("Last(10) = %v, want [a b]", got)
	}
}

func TestRingBufferEvictsOldestOnWraparound(t *testing.T) {
	r := newRingBuffer(3)
	for i := 1; i <= 5; i++ {
		r.Append(fmt.Sprintf("line %d", i))
	}

	want := []string{"line 3", "line 4", "line 5"}
	if got := r.Last(0); !reflect.DeepEqual(got, want) {
		t.Errorf("Last(0) = %v, want %v", got, want)
	}
	if got := r.Last(2); !reflect.DeepEqual(got, []string{"line 4", "line 5"}) {
		t.Errorf("Last(2) = %v, want [line 4, line 5]", got)
	}
}
