package server

import "testing"

func TestParseControlTap(t *testing.T) {
	m, err := parseControl([]byte(`{"type":"tap","x":0.25,"y":0.75}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != "tap" || m.X != 0.25 || m.Y != 0.75 {
		t.Fatalf("unexpected: %+v", m)
	}
}

func TestParseControlHome(t *testing.T) {
	m, err := parseControl([]byte(`{"type":"home"}`))
	if err != nil || m.Type != "home" {
		t.Fatalf("unexpected: %+v err %v", m, err)
	}
}

func TestParseControlBadJSON(t *testing.T) {
	if _, err := parseControl([]byte(`not json`)); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseControlUnknownType(t *testing.T) {
	if _, err := parseControl([]byte(`{"type":"wat"}`)); err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestParseControlSwipe(t *testing.T) {
	m, err := parseControl([]byte(`{"type":"swipe","x1":0.1,"y1":0.2,"x2":0.8,"y2":0.9,"duration":0.3}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != "swipe" || m.X1 != 0.1 || m.Y1 != 0.2 || m.X2 != 0.8 || m.Y2 != 0.9 || m.Duration != 0.3 {
		t.Fatalf("unexpected: %+v", m)
	}
}

func TestParseControlKey(t *testing.T) {
	m, err := parseControl([]byte(`{"type":"key","key":"A"}`))
	if err != nil || m.Type != "key" || m.Key != "A" {
		t.Fatalf("unexpected: %+v err %v", m, err)
	}
}

func TestParseControlManagementTypes(t *testing.T) {
	cases := []struct {
		in   string
		typ  string
		udid string
	}{
		{`{"type":"list"}`, "list", ""},
		{`{"type":"boot","udid":"ABC"}`, "boot", "ABC"},
		{`{"type":"attach","udid":"XYZ"}`, "attach", "XYZ"},
		{`{"type":"detach"}`, "detach", ""},
		{`{"type":"shutdown","udid":"ABC"}`, "shutdown", "ABC"},
	}
	for _, c := range cases {
		m, err := parseControl([]byte(c.in))
		if err != nil {
			t.Fatalf("parseControl(%s): %v", c.in, err)
		}
		if m.Type != c.typ || m.UDID != c.udid {
			t.Fatalf("parseControl(%s) = {%q,%q}, want {%q,%q}", c.in, m.Type, m.UDID, c.typ, c.udid)
		}
	}
}

func TestParseControlUnknownStillErrors(t *testing.T) {
	if _, err := parseControl([]byte(`{"type":"explode"}`)); err == nil {
		t.Fatal("want error for unknown type, got nil")
	}
}
