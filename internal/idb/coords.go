package idb

// ScaleTap maps a normalized tap (xNorm,yNorm in [0,1] of the displayed frame)
// to a Point in the simulator's logical-point coordinate space, which is what
// hid expects. Verified live: tapping in pixel space (Width/Height) lands
// off-screen because pixels are ~density× larger than points.
func ScaleTap(xNorm, yNorm float64, s Screen) Point {
	xNorm = clamp01(xNorm)
	yNorm = clamp01(yNorm)
	return Point{
		X: xNorm * float64(s.WidthPoints),
		Y: yNorm * float64(s.HeightPoints),
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
