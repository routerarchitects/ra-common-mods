package cache

import "testing"

type sample struct {
	Name string `json:"name"`
}

func TestJSONCodecRoundTrip(t *testing.T) {
	codec := JSONCodec{}
	in := sample{Name: "alice"}
	b, err := codec.Marshal(in)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var out sample
	if err := codec.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if out.Name != "alice" {
		t.Fatalf("unexpected value: %+v", out)
	}
	if codec.ContentType() != "application/json" {
		t.Fatalf("unexpected content type: %s", codec.ContentType())
	}
}
