package main

import "testing"

func testCfg() config {
	return config{minDist: 60, tpFrac: 0.06, tolRatio: 0.7}
}

// feed runs a sequence of (type, code, value) events through a detector and
// reports how many times it would have launched.
func feed(kind devKind, width float64, evs [][3]int) int {
	launches := 0
	d := &detector{cfg: testCfg(), launch: func() { launches++ }, kind: kind, width: width}
	for _, e := range evs {
		d.handle(&inputEvent{Type: uint16(e[0]), Code: uint16(e[1]), Value: int32(e[2])})
	}
	return launches
}

func relMove(dx, dy int) [][3]int {
	return [][3]int{
		{evKey, btnRight, 1},
		{evRel, relX, dx},
		{evRel, relY, dy},
		{evKey, btnRight, 0},
	}
}

func TestMouseGesture(t *testing.T) {
	tests := []struct {
		name    string
		evs     [][3]int
		wantHit bool
	}{
		{"straight right", relMove(120, 0), true},
		{"right with small wobble", relMove(120, 40), true},
		{"too short", relMove(30, 0), false},
		{"too steep (vertical)", relMove(80, 200), false},
		{"leftward", relMove(-120, 0), false},
		{"incremental right", [][3]int{
			{evKey, btnRight, 1},
			{evRel, relX, 30}, {evRel, relX, 30}, {evRel, relX, 30},
			{evKey, btnRight, 0},
		}, true},
		{"movement without button held is ignored", [][3]int{
			{evRel, relX, 500},
			{evKey, btnRight, 1},
			{evKey, btnRight, 0},
		}, false},
	}
	for _, tt := range tests {
		got := feed(kindMouse, 0, tt.evs) > 0
		if got != tt.wantHit {
			t.Errorf("%s: got hit=%v want %v", tt.name, got, tt.wantHit)
		}
	}
}

// twoFingerSwipe builds a clickpad two-finger click+drag from x0 to x1.
func twoFingerSwipe(x0, x1, y0, y1 int) [][3]int {
	return [][3]int{
		{evKey, btnToolDoubletap, 1}, // two fingers present
		{evAbs, absX, x0}, {evAbs, absY, y0},
		{evKey, btnLeft, 1}, // physical click = right-click with 2 fingers
		{evAbs, absX, (x0 + x1) / 2}, {evAbs, absY, (y0 + y1) / 2},
		{evAbs, absX, x1}, {evAbs, absY, y1},
		{evKey, btnLeft, 0},
		{evKey, btnToolDoubletap, 0},
	}
}

func oneFingerSwipe(x0, x1 int) [][3]int {
	return [][3]int{
		{evKey, btnToolFinger, 1},
		{evAbs, absX, x0},
		{evKey, btnLeft, 1}, // one-finger click = left click
		{evAbs, absX, x1},
		{evKey, btnLeft, 0},
		{evKey, btnToolFinger, 0},
	}
}

func TestTouchpadGesture(t *testing.T) {
	const width = 15432 // matches the Apple trackpad observed on this machine
	tests := []struct {
		name    string
		evs     [][3]int
		wantHit bool
	}{
		// 0.06 * 15432 ≈ 926 units required.
		{"two-finger right swipe", twoFingerSwipe(2000, 6000, 8000, 8050), true},
		{"two-finger short", twoFingerSwipe(2000, 2500, 8000, 8000), false},
		{"two-finger too steep", twoFingerSwipe(2000, 5000, 2000, 9000), false},
		{"two-finger leftward", twoFingerSwipe(6000, 2000, 8000, 8000), false},
		{"one-finger click+drag (left click)", oneFingerSwipe(2000, 9000), false},
		{"two fingers but no click (scroll)", [][3]int{
			{evKey, btnToolDoubletap, 1},
			{evAbs, absX, 2000}, {evAbs, absX, 9000},
			{evKey, btnToolDoubletap, 0},
		}, false},
	}
	for _, tt := range tests {
		got := feed(kindTouchpad, width, tt.evs) > 0
		if got != tt.wantHit {
			t.Errorf("%s: got hit=%v want %v", tt.name, got, tt.wantHit)
		}
	}
}
