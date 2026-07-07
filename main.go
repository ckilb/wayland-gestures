// wayland-gestures detects a "swipe right while the right mouse button is held"
// gesture and launches a command (dolphin by default) when it happens.
//
// It reads raw input directly from the Linux evdev interface
// (/dev/input/event*), so it works globally regardless of the display server
// (Wayland or X11) and regardless of which window has focus.
//
// Two device classes are supported:
//
//   - Relative pointers (real mice, EV_REL): press the physical right button,
//     move right, release.
//
//   - Absolute clickpads / touchpads (EV_ABS, e.g. an Apple MacBook trackpad):
//     these have no hardware right button, so — exactly like libinput's default
//     "clickfinger" behaviour — a right-click is a TWO-FINGER click. The gesture
//     is therefore: place two fingers, press the pad down, slide right, release.
//
// In both cases vertical wobble is tolerated, so the motion does not have to be
// a perfectly straight horizontal line.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// evdev event types and codes (from <linux/input-event-codes.h>).
const (
	evSyn = 0x00
	evKey = 0x01
	evRel = 0x02
	evAbs = 0x03

	relX = 0x00
	relY = 0x01

	absX = 0x00
	absY = 0x01

	absMTSlot      = 0x2f // 47 — selects which contact the following MT axes describe
	absMTPositionX = 0x35 // 53 — multitouch contact X (used when ST emulation freezes)
	absMTPositionY = 0x36 // 54

	btnLeft  = 0x110 // 272
	btnRight = 0x111 // 273

	btnToolFinger    = 0x145 // 325 — one finger on the pad
	btnToolDoubletap = 0x14d // 333 — two fingers
	btnToolTripletap = 0x14e // 334 — three fingers

	// Keys injected for the 3-finger swipe chords.
	keyLeftCtrl = 0x1d // 29  — KEY_LEFTCTRL
	keyLeftAlt  = 0x38 // 56  — KEY_LEFTALT
	keyW        = 0x11 // 17  — KEY_W
	keyT        = 0x14 // 20  — KEY_T
	keyLeft     = 0x69 // 105 — KEY_LEFT
	keyRight    = 0x6a // 106 — KEY_RIGHT

	synReport = 0x00 // SYN_REPORT
)

// inputEvent mirrors `struct input_event` on 64-bit Linux (24 bytes).
type inputEvent struct {
	Sec   int64
	Usec  int64
	Type  uint16
	Code  uint16
	Value int32
}

const inputEventSize = int(unsafe.Sizeof(inputEvent{}))

// config holds the tunable gesture parameters.
type config struct {
	command   string   // command to launch on a successful gesture
	args      []string // command arguments
	minDist   float64  // mouse: minimum rightward travel (evdev relative units)
	tpFrac    float64  // touchpad: minimum rightward travel as a fraction of pad width
	swipeFrac float64  // touchpad: 3-finger swipe minimum rightward travel as a fraction of pad width
	tolRatio  float64  // max |dy|/dx ratio (vertical wobble allowance), both classes
	cooldown  time.Duration
	debug     bool
}

func main() {
	var (
		cmd     = flag.String("cmd", "dolphin", "command to launch when the gesture is recognized")
		minDist = flag.Float64("dist", 60, "MOUSE: minimum rightward movement to trigger (evdev relative units)")
		tpFrac  = flag.Float64("tpdist", 0.06, "TOUCHPAD: minimum rightward travel as a fraction of pad width (0..1)")
		swFrac  = flag.Float64("swipedist", 0.05, "TOUCHPAD 3-finger swipe: minimum travel in the dominant direction as a fraction of pad size (0..1)")
		tol     = flag.Float64("tol", 0.7, "vertical tolerance as a fraction of horizontal travel (0 = perfectly straight)")
		cool    = flag.Duration("cooldown", 800*time.Millisecond, "minimum time between launches")
		debug   = flag.Bool("debug", false, "log every input event and gesture decision (for troubleshooting)")
	)
	flag.Parse()

	cfg := config{
		command:   *cmd,
		args:      flag.Args(),
		minDist:   *minDist,
		tpFrac:    *tpFrac,
		swipeFrac: *swFrac,
		tolRatio:  *tol,
		cooldown:  *cool,
		debug:     *debug,
	}

	log.SetFlags(log.Ltime)
	log.Printf("wayland-gestures: launch %q on a rightward right-button swipe", cfg.command)
	log.Printf("  mouse:    hold right button, move right, release")
	log.Printf("  touchpad: two-finger click, slide right, release")
	log.Printf("  params: mouse-dist=%.0f touchpad-frac=%.2f swipe-frac=%.2f vertical-tol=%.2f", cfg.minDist, cfg.tpFrac, cfg.swipeFrac, cfg.tolRatio)

	// The 3-finger swipe injects a synthetic Alt+arrow keypress, which needs
	// /dev/uinput. If that is unavailable (usually a permission issue) the
	// rest of the tool still works; only the swipe gesture is disabled.
	var swipe func(swipeDir)
	inj, err := newInjector(cfg.cooldown)
	if err != nil {
		log.Printf("3-finger swipe DISABLED: cannot open /dev/uinput: %v", err)
		log.Printf("  run as root, or grant your user write access to /dev/uinput")
	} else {
		swipe = inj.chord
		log.Printf("  3-finger swipe: right->Alt+Right left->Alt+Left down->Ctrl+T up->Ctrl+W (via /dev/uinput)")
	}

	l := &launcher{cfg: cfg}
	m := &monitor{cfg: cfg, launch: l.maybeLaunch, swipe: swipe, open: map[string]bool{}, firstScan: true}
	m.run()
}

// monitor enumerates pointer devices and reads each one in its own goroutine,
// re-scanning periodically so hot-plugged devices are picked up.
type monitor struct {
	cfg    config
	launch func()
	swipe  func(swipeDir) // 3-finger swipe action (touchpads only); nil if uinput unavailable

	firstScan bool
	monitored atomic.Int32 // number of devices currently being read

	mu   sync.Mutex
	open map[string]bool // device path -> being handled (read or skipped)
}

func (m *monitor) run() {
	m.scan()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		m.scan()
	}
}

func (m *monitor) scan() {
	paths, err := filepath.Glob("/dev/input/event*")
	if err != nil {
		log.Printf("scan: %v", err)
		return
	}
	for _, p := range paths {
		m.mu.Lock()
		already := m.open[p]
		m.mu.Unlock()
		if already {
			continue
		}

		f, err := os.Open(p)
		if err != nil {
			// Usually EACCES (user not in the "input" group). Mark it so we
			// don't retry/log endlessly.
			m.markDone(p)
			log.Printf("cannot open %s: %v", p, err)
			continue
		}

		hasRel, hasAbs := eventCaps(f.Fd())
		name := deviceName(f.Fd())

		var d *detector
		switch {
		case hasRel:
			d = &detector{cfg: m.cfg, launch: m.launch, kind: kindMouse, name: name}
			log.Printf("monitoring mouse: %s (%s)", p, name)
		case hasAbs:
			ax, ok := absInfo(f.Fd(), absX)
			if !ok || ax.Max <= ax.Min {
				f.Close()
				m.markDone(p)
				continue
			}
			width := float64(ax.Max - ax.Min)
			height := width // fallback if ABS_Y is unavailable
			if ay, ok := absInfo(f.Fd(), absY); ok && ay.Max > ay.Min {
				height = float64(ay.Max - ay.Min)
			}
			d = &detector{cfg: m.cfg, launch: m.launch, swipeAction: m.swipe, kind: kindTouchpad, name: name, width: width, height: height}
			log.Printf("monitoring touchpad: %s (%s) width=%.0f height=%.0f", p, name, width, height)
		default:
			f.Close()
			m.markDone(p)
			continue
		}

		m.monitored.Add(1)
		m.markDone(p) // mark as handled; readLoop clears it on exit for replug
		go m.readLoop(p, f, d)
	}

	if m.firstScan {
		m.firstScan = false
		if m.monitored.Load() == 0 {
			log.Printf("WARNING: no mouse or touchpad could be monitored.")
			log.Printf("  If you saw 'permission denied' above, your user is not in the 'input' group")
			log.Printf("  in THIS session. Add it and start a NEW login session:")
			log.Printf("    sudo usermod -aG input \"$USER\"   (then log out and back in)")
			log.Printf("  Or run once with the group active:  sg input -c '%s'", os.Args[0])
		}
	}
}

func (m *monitor) markDone(p string) {
	m.mu.Lock()
	m.open[p] = true
	m.mu.Unlock()
}

func (m *monitor) readLoop(path string, f *os.File, d *detector) {
	defer func() {
		f.Close()
		m.monitored.Add(-1)
		// Allow re-opening on a later scan (e.g. unplug/replug).
		m.mu.Lock()
		delete(m.open, path)
		m.mu.Unlock()
		log.Printf("stopped monitoring %s", path)
	}()

	buf := make([]byte, inputEventSize*64)
	for {
		n, err := f.Read(buf)
		if err != nil {
			log.Printf("read %s: %v", path, err)
			return
		}
		for off := 0; off+inputEventSize <= n; off += inputEventSize {
			ev := (*inputEvent)(unsafe.Pointer(&buf[off]))
			d.handle(ev)
		}
	}
}

type devKind int

const (
	kindMouse devKind = iota
	kindTouchpad
)

// swipeDir is the dominant direction of a 3-finger swipe.
type swipeDir int

const (
	swipeRight swipeDir = iota
	swipeLeft
	swipeDown
	swipeUp
)

func (s swipeDir) String() string {
	switch s {
	case swipeRight:
		return "right -> Alt+Right"
	case swipeLeft:
		return "left -> Alt+Left"
	case swipeDown:
		return "down -> Ctrl+T"
	case swipeUp:
		return "up -> Ctrl+W"
	}
	return "?"
}

// detector tracks the gesture state for a single device.
type detector struct {
	cfg    config
	launch func()
	kind   devKind
	name   string

	tracking bool

	// mouse: accumulated relative movement while the right button is held.
	dx, dy float64

	// touchpad
	width          float64 // ABS_X span, for normalizing horizontal travel
	height         float64 // ABS_Y span, for normalizing vertical travel
	curSlot        int32   // current MT slot; we only follow slot 0 (one finger)
	curX, curY     int32   // latest absolute position of the followed finger
	haveAbs        bool
	startX, startY int32  // position when the click started
	f1, f2, f3     int32  // active finger tools (1 finger / 2 / 3)
	activeButton   uint16 // button currently held (btnLeft/btnRight), 0 if none
	forceRight     bool   // started from a real BTN_RIGHT
	maxFingers     int    // most fingers seen during the current hold

	// 3-finger swipe (no click): on a swipe of three fingers, fire swipeAction
	// with the dominant direction once the fingers leave the pad.
	swipeAction              func(swipeDir)
	swipeTracking            bool
	swipeStartX, swipeStartY int32
}

func (d *detector) handle(ev *inputEvent) {
	if d.cfg.debug && ev.Type != evSyn {
		log.Printf("[%s] %s", d.name, eventString(ev))
	}
	if d.kind == kindMouse {
		d.handleMouse(ev)
		return
	}
	d.handleTouchpad(ev)
}

func (d *detector) handleMouse(ev *inputEvent) {
	switch ev.Type {
	case evKey:
		if ev.Code != btnRight {
			return
		}
		switch ev.Value {
		case 1: // press
			d.tracking = true
			d.dx, d.dy = 0, 0
		case 0: // release
			if d.tracking {
				d.tracking = false
				d.evalMouse()
			}
		}
	case evRel:
		if !d.tracking {
			return
		}
		switch ev.Code {
		case relX:
			d.dx += float64(ev.Value)
		case relY:
			d.dy += float64(ev.Value)
		}
	}
}

func (d *detector) evalMouse() {
	if d.dx < d.cfg.minDist {
		return
	}
	if abs(d.dy) > d.dx*d.cfg.tolRatio {
		return
	}
	log.Printf("mouse gesture (dx=%.0f dy=%.0f) -> launching", d.dx, d.dy)
	d.launch()
}

func (d *detector) handleTouchpad(ev *inputEvent) {
	switch ev.Type {
	case evAbs:
		// Track position from both single-touch and multitouch axes; some
		// drivers freeze ST emulation (ABS_X/Y) while >1 finger is down and
		// only update the MT axes. The MT axes report one finger per slot, so
		// follow slot 0 only — otherwise curX/curY jump between fingers sitting
		// at different positions and the swipe direction comes out garbled.
		switch ev.Code {
		case absMTSlot:
			d.curSlot = ev.Value
		case absX:
			d.curX = ev.Value
			d.haveAbs = true
		case absY:
			d.curY = ev.Value
		case absMTPositionX:
			if d.curSlot == 0 {
				d.curX = ev.Value
				d.haveAbs = true
			}
		case absMTPositionY:
			if d.curSlot == 0 {
				d.curY = ev.Value
			}
		}
	case evKey:
		switch ev.Code {
		case btnToolFinger:
			d.f1 = ev.Value
		case btnToolDoubletap:
			d.f2 = ev.Value
		case btnToolTripletap:
			d.f3 = ev.Value
		case btnRight:
			// Some touchpads expose a real BTN_RIGHT (e.g. corner areas).
			d.onButton(btnRight, ev.Value == 1, true)
		case btnLeft:
			// On a clickpad the whole surface is BTN_LEFT; a click with two
			// fingers down means "right click" (libinput clickfinger rule).
			d.onButton(btnLeft, ev.Value == 1, false)
		}
	}
	// Keep the running max finger count up to date during a hold, since the
	// finger count may settle slightly after the physical click.
	if d.tracking {
		if f := d.fingers(); f > d.maxFingers {
			d.maxFingers = f
		}
	}

	d.updateSwipe()
}

// updateSwipe watches for a clickless three-finger gesture: it latches the
// start position when the third finger lands and evaluates travel when the
// fingers lift.
func (d *detector) updateSwipe() {
	if d.swipeAction == nil {
		return
	}
	threeDown := d.f3 == 1
	switch {
	case threeDown && !d.swipeTracking && d.haveAbs:
		d.swipeTracking = true
		d.swipeStartX, d.swipeStartY = d.curX, d.curY
	case !threeDown && d.swipeTracking:
		d.swipeTracking = false
		d.evalSwipe()
	}
}

func (d *detector) evalSwipe() {
	dx := float64(d.curX - d.swipeStartX)
	dy := float64(d.curY - d.swipeStartY)

	// Classify by RAW physical travel: the axis the finger actually moved
	// further along wins. (ABS_X and ABS_Y share the same units, so comparing
	// raw deltas reflects real direction. Comparing per-axis *fractions* would
	// be wrong — the pad is wider than tall, so a tiny vertical wobble divided
	// by the small height balloons and a horizontal swipe reads as up/down.)
	horizontal := abs(dx) >= abs(dy)

	// The distance gate, however, is normalized per axis so a short vertical
	// swipe isn't held to the wider pad's horizontal threshold.
	frac := 0.0
	if horizontal && d.width > 0 {
		frac = dx / d.width
	} else if !horizontal && d.height > 0 {
		frac = dy / d.height
	}

	if d.cfg.debug {
		log.Printf("[%s] 3-finger lift: dx=%.0f dy=%.0f horizontal=%v frac=%.2f (need >=%.2f)",
			d.name, dx, dy, horizontal, frac, d.cfg.swipeFrac)
	}

	if abs(frac) < d.cfg.swipeFrac {
		return // too short in the dominant direction
	}

	var dir swipeDir
	switch {
	case horizontal && frac > 0:
		dir = swipeRight
	case horizontal:
		dir = swipeLeft
	case frac > 0:
		dir = swipeDown // ABS_Y grows downward
	default:
		dir = swipeUp
	}
	log.Printf("3-finger swipe %s (dx=%.0f dy=%.0f, %.0f%% of pad)", dir, dx, dy, 100*abs(frac))
	d.swipeAction(dir)
}

func (d *detector) onButton(code uint16, down, isRight bool) {
	if down {
		if d.activeButton != 0 {
			return // already holding a button; ignore the other
		}
		d.activeButton = code
		d.forceRight = isRight
		d.tracking = d.haveAbs
		d.startX, d.startY = d.curX, d.curY
		d.maxFingers = d.fingers()
		return
	}
	if code != d.activeButton {
		return
	}
	d.activeButton = 0
	if d.tracking {
		d.tracking = false
		d.endTouch()
	}
}

func (d *detector) fingers() int {
	switch {
	case d.f3 == 1:
		return 3
	case d.f2 == 1:
		return 2
	case d.f1 == 1:
		return 1
	default:
		return 0
	}
}

func (d *detector) endTouch() {
	rightClick := d.forceRight || d.maxFingers >= 2
	dx := float64(d.curX - d.startX)
	dy := float64(d.curY - d.startY)
	frac := 0.0
	if d.width > 0 {
		frac = dx / d.width
	}

	if d.cfg.debug {
		log.Printf("[%s] click released: rightClick=%v maxFingers=%d dx=%.0f dy=%.0f frac=%.2f (need >=%.2f, |dy|<=dx*%.2f)",
			d.name, rightClick, d.maxFingers, dx, dy, frac, d.cfg.tpFrac, d.cfg.tolRatio)
	}

	if !rightClick {
		return // a one-finger (left) click
	}
	if frac < d.cfg.tpFrac {
		return // not far enough to the right
	}
	if abs(dy) > dx*d.cfg.tolRatio {
		return // too steep / vertical
	}
	log.Printf("touchpad gesture (dx=%.0f dy=%.0f, %.0f%% of width) -> launching", dx, dy, 100*frac)
	d.launch()
}

// launcher launches the configured command, with a cooldown to avoid
// double-launches from a single gesture.
type launcher struct {
	cfg  config
	mu   sync.Mutex
	last time.Time
}

func (l *launcher) maybeLaunch() {
	l.mu.Lock()
	if time.Since(l.last) < l.cfg.cooldown {
		l.mu.Unlock()
		return
	}
	l.last = time.Now()
	l.mu.Unlock()

	cmd := exec.Command(l.cfg.command, l.cfg.args...)
	// Detach into its own session so it survives if this app exits.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		log.Printf("failed to launch %q: %v", l.cfg.command, err)
		return
	}
	go cmd.Wait() // reap asynchronously
}

// injector owns a virtual /dev/uinput keyboard used to synthesize a modifier +
// key chord: press the modifier, tap the key, release the modifier. It
// advertises the relevant keys so libinput classifies it as a real keyboard.
type injector struct {
	f        *os.File
	cooldown time.Duration

	mu   sync.Mutex
	last time.Time
}

func newInjector(cooldown time.Duration) (*injector, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}

	setEv := ioc(iocWrite, 'U', 100, 4)  // UI_SET_EVBIT
	setKey := ioc(iocWrite, 'U', 101, 4) // UI_SET_KEYBIT
	devCreate := ioc(iocNone, 'U', 1, 0) // UI_DEV_CREATE

	set := func(req, val uintptr) {
		if err == nil {
			err = ioctlVal(f.Fd(), req, val)
		}
	}
	set(setEv, evKey)
	set(setEv, evSyn)
	for _, k := range []uintptr{keyLeftAlt, keyLeftCtrl, keyLeft, keyRight, keyT, keyW} {
		set(setKey, k)
	}
	if err != nil {
		f.Close()
		return nil, err
	}

	var dev uinputUserDev
	copy(dev.Name[:], "wayland-gestures virtual keyboard")
	dev.ID = inputID{Bustype: 0x03, Vendor: 0x1234, Product: 0x5678, Version: 1}
	devBytes := (*[uinputUserDevSize]byte)(unsafe.Pointer(&dev))[:]
	if _, err := f.Write(devBytes); err != nil {
		f.Close()
		return nil, err
	}
	if err := ioctlVal(f.Fd(), devCreate, 0); err != nil {
		f.Close()
		return nil, err
	}
	// Give udev a moment to materialize the node before we emit into it.
	time.Sleep(200 * time.Millisecond)
	return &injector{f: f, cooldown: cooldown}, nil
}

func (in *injector) emit(typ, code uint16, val int32) error {
	ev := inputEvent{Type: typ, Code: code, Value: val}
	b := (*[inputEventSize]byte)(unsafe.Pointer(&ev))[:]
	_, err := in.f.Write(b)
	return err
}

// chord maps a swipe direction to a modifier+key combination and emits it:
//
//	right -> Alt+Right   left -> Alt+Left   down -> Ctrl+T   up -> Ctrl+W
//
// It presses the modifier, taps the key, releases the modifier, with the same
// cooldown semantics as the launcher.
func (in *injector) chord(dir swipeDir) {
	var mod, key uint16 = keyLeftAlt, keyRight
	switch dir {
	case swipeRight:
		mod, key = keyLeftAlt, keyRight
	case swipeLeft:
		mod, key = keyLeftAlt, keyLeft
	case swipeDown:
		mod, key = keyLeftCtrl, keyT
	case swipeUp:
		mod, key = keyLeftCtrl, keyW
	}

	in.mu.Lock()
	defer in.mu.Unlock()
	if time.Since(in.last) < in.cooldown {
		return
	}
	in.last = time.Now()

	steps := []struct {
		code uint16
		val  int32
	}{
		{mod, 1}, // modifier down
		{key, 1}, // key down
		{key, 0}, // key up
		{mod, 0}, // modifier up
	}
	for _, s := range steps {
		if err := in.emit(evKey, s.code, s.val); err != nil {
			log.Printf("uinput emit: %v", err)
			return
		}
		in.emit(evSyn, synReport, 0)
		time.Sleep(20 * time.Millisecond)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// eventString renders an input event for -debug output. Code names depend on
// the event type (e.g. code 0 is REL_X for EV_REL but ABS_X for EV_ABS).
func eventString(ev *inputEvent) string {
	var typ, code string
	switch ev.Type {
	case evKey:
		typ = "KEY"
		code = map[uint16]string{
			btnLeft: "BTN_LEFT", btnRight: "BTN_RIGHT",
			btnToolFinger: "BTN_TOOL_FINGER", btnToolDoubletap: "BTN_TOOL_DOUBLETAP",
			btnToolTripletap: "BTN_TOOL_TRIPLETAP",
		}[ev.Code]
	case evRel:
		typ = "REL"
		code = map[uint16]string{relX: "REL_X", relY: "REL_Y"}[ev.Code]
	case evAbs:
		typ = "ABS"
		code = map[uint16]string{
			absX: "ABS_X", absY: "ABS_Y", absMTSlot: "ABS_MT_SLOT",
			absMTPositionX: "ABS_MT_X", absMTPositionY: "ABS_MT_Y",
		}[ev.Code]
	default:
		typ = fmt.Sprintf("t%d", ev.Type)
	}
	if code == "" {
		code = fmt.Sprintf("c0x%x", ev.Code)
	}
	return fmt.Sprintf("%s %-20s %d", typ, code, ev.Value)
}

// --- evdev ioctl helpers ---

const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2
)

func ioc(dir, typ, nr, size uintptr) uintptr {
	return (dir << 30) | (size << 16) | (typ << 8) | nr
}

// inputID and uinputUserDev mirror the kernel uinput legacy setup structs.
type inputID struct {
	Bustype, Vendor, Product, Version uint16
}

type uinputUserDev struct {
	Name         [80]byte
	ID           inputID
	FFEffectsMax uint32
	Absmax       [64]int32
	Absmin       [64]int32
	Absfuzz      [64]int32
	Absflat      [64]int32
}

const uinputUserDevSize = int(unsafe.Sizeof(uinputUserDev{}))

// ioctlVal issues an ioctl whose argument is passed by value (as uinput's
// UI_SET_* and UI_DEV_CREATE expect), rather than as a pointer.
func ioctlVal(fd, request, value uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, value)
	if errno != 0 {
		return fmt.Errorf("ioctl: %w", errno)
	}
	return nil
}

// eventCaps reports which event classes the device supports, via
// EVIOCGBIT(0): relative motion (mouse) and/or absolute motion (touchpad).
func eventCaps(fd uintptr) (hasRel, hasAbs bool) {
	var bits [4]byte // EV_CNT (32) bits -> 4 bytes
	if err := ioctl(fd, ioc(iocRead, 'E', 0x20, uintptr(len(bits))), unsafe.Pointer(&bits)); err != nil {
		return false, false
	}
	hasRel = bits[evRel/8]&(1<<(evRel%8)) != 0
	hasAbs = bits[evAbs/8]&(1<<(evAbs%8)) != 0
	return
}

// inputAbsinfo mirrors `struct input_absinfo` (6 x int32).
type inputAbsinfo struct {
	Value, Min, Max, Fuzz, Flat, Resolution int32
}

// absInfo returns an absolute axis range via EVIOCGABS(axis), e.g. absX/absY.
func absInfo(fd uintptr, axis uintptr) (inputAbsinfo, bool) {
	var ai inputAbsinfo
	req := ioc(iocRead, 'E', 0x40+axis, unsafe.Sizeof(ai))
	if err := ioctl(fd, req, unsafe.Pointer(&ai)); err != nil {
		return ai, false
	}
	return ai, true
}

func deviceName(fd uintptr) string {
	var buf [256]byte
	if err := ioctl(fd, ioc(iocRead, 'E', 0x06, uintptr(len(buf))), unsafe.Pointer(&buf)); err != nil {
		return "unknown"
	}
	return string(bytes.TrimRight(buf[:], "\x00"))
}

func ioctl(fd, request uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(arg))
	if errno != 0 {
		return fmt.Errorf("ioctl: %w", errno)
	}
	return nil
}
