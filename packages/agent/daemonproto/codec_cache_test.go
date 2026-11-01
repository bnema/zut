package daemonproto

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

type plainCycle struct {
	Next *plainCycle
	Name string
}
type richCycle struct {
	Next  *richCycle
	Delay time.Duration
}
type cycleA struct {
	B     *cycleB
	Delay time.Duration
}
type cycleB struct{ A *cycleA }
type EmbeddedDuration struct{ Delay time.Duration }

func TestEnrichMetadata(t *testing.T) {
	for _, tc := range []struct {
		value any
		needs bool
	}{
		{plainCycle{}, false}, {richCycle{}, true}, {cycleA{}, true}, {cycleB{}, true},
		{[]plainCycle{}, false}, {[2]plainCycle{}, false}, {map[string]plainCycle{}, false},
		{[][]*richCycle{}, true}, {map[string][]time.Duration{}, true}, {map[int]time.Duration{}, false},
		{json.RawMessage{}, false}, {time.Time{}, false}, {provider.TextBlock{}, true},
		{struct{ Value any }{}, true}, {struct {
			Delay time.Duration `json:"-"`
		}{}, false},
		{struct{ delay time.Duration }{}, false},
	} {
		typ := reflect.TypeOf(tc.value)
		t.Run(typ.String(), func(t *testing.T) {
			m := enrichMetadata(typ)
			if m.needs != tc.needs {
				t.Fatalf("needs = %v, want %v", m.needs, tc.needs)
			}
			if enrichMetadata(typ) != m {
				t.Fatal("metadata not cached")
			}
		})
	}
}

func TestEnrichPlainReturnsRawUnchanged(t *testing.T) {
	for _, value := range []any{plainCycle{Name: "plain"}, []plainCycle{{Name: "plain"}}, [1]plainCycle{{Name: "plain"}}, map[string]plainCycle{"x": {Name: "plain"}}} {
		// Whitespace and key order make any unmarshal/remarshal visible.
		raw := json.RawMessage(" { \"z\": 1, \"a\": 2 } ")
		got, err := enrich(reflect.ValueOf(value), raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(raw) || &got[0] != &raw[0] {
			t.Fatalf("plain %T did not return original raw", value)
		}
	}
}

func TestEnrichMetadataConcurrentCycle(t *testing.T) {
	// Unique types exercise concurrent first lookup, not only warm cache reads.
	type node struct {
		Next   *node
		Values []any
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			v := node{Values: []any{time.Duration(1234567)}}
			for range 20 {
				got, err := marshalPayload(v)
				if err != nil {
					t.Error(err)
					return
				}
				if string(got) != `{"Next":null,"Values":[{"ms":1,"remainder_ns":234567}]}` {
					t.Errorf("unexpected payload: %s", got)
					return
				}
			}
		})
	}
	wg.Wait()
}

func TestEnrichFieldRules(t *testing.T) {
	type fields struct {
		EmbeddedDuration
		Named   time.Duration `json:"named,omitempty"`
		Ignored time.Duration `json:"-"`
		hidden  time.Duration
	}
	got, err := marshalPayload(fields{EmbeddedDuration: EmbeddedDuration{Delay: time.Second}, Named: 1234567, Ignored: time.Second, hidden: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	// Anonymous fields remain unvisited when their direct field name is absent
	// from the JSON object, matching the original codec rather than flattening.
	const want = `{"Delay":1000000000,"named":{"ms":1,"remainder_ns":234567}}`
	if string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
