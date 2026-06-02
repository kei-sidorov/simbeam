package idb

import "testing"

func TestScaleTapCenter(t *testing.T) {
	s := Screen{Width: 1170, Height: 2532, WidthPoints: 390, HeightPoints: 844}
	p := ScaleTap(0.5, 0.5, s) // scales by logical points, not pixels
	if p.X != 195 || p.Y != 422 {
		t.Fatalf("got (%v,%v), want (195,422)", p.X, p.Y)
	}
}

func TestScaleTapClamps(t *testing.T) {
	s := Screen{Width: 100, Height: 100, WidthPoints: 50, HeightPoints: 50}
	p := ScaleTap(1.5, -0.2, s) // out of range → clamped to [0,1]
	if p.X != 50 || p.Y != 0 {
		t.Fatalf("got (%v,%v), want (50,0)", p.X, p.Y)
	}
}
