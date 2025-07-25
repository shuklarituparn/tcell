//go:build windows
// +build windows

// Copyright 2024 The TCell Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use file except in compliance with the License.
// You may obtain a copy of the license at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tcell

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

type cScreen struct {
	in         syscall.Handle
	out        syscall.Handle
	cancelflag syscall.Handle
	scandone   chan struct{}
	quit       chan struct{}
	curx       int
	cury       int
	style      Style
	fini       bool
	vten       bool
	truecolor  bool
	running    bool
	disableAlt bool // disable the alternate screen
	title      string

	w int
	h int

	oscreen     consoleInfo
	ocursor     cursorInfo
	cursorStyle CursorStyle
	cursorColor Color
	oimode      uint32
	oomode      uint32
	cells       CellBuffer
	focusEnable bool

	mouseEnabled bool
	wg           sync.WaitGroup
	eventQ       chan Event
	stopQ        chan struct{}
	finiOnce     sync.Once

	sync.Mutex
}

var winLock sync.Mutex

var winPalette = []Color{
	ColorBlack,
	ColorMaroon,
	ColorGreen,
	ColorNavy,
	ColorOlive,
	ColorPurple,
	ColorTeal,
	ColorSilver,
	ColorGray,
	ColorRed,
	ColorLime,
	ColorBlue,
	ColorYellow,
	ColorFuchsia,
	ColorAqua,
	ColorWhite,
}

var winColors = map[Color]Color{
	ColorBlack:   ColorBlack,
	ColorMaroon:  ColorMaroon,
	ColorGreen:   ColorGreen,
	ColorNavy:    ColorNavy,
	ColorOlive:   ColorOlive,
	ColorPurple:  ColorPurple,
	ColorTeal:    ColorTeal,
	ColorSilver:  ColorSilver,
	ColorGray:    ColorGray,
	ColorRed:     ColorRed,
	ColorLime:    ColorLime,
	ColorBlue:    ColorBlue,
	ColorYellow:  ColorYellow,
	ColorFuchsia: ColorFuchsia,
	ColorAqua:    ColorAqua,
	ColorWhite:   ColorWhite,
}

var (
	k32 = syscall.NewLazyDLL("kernel32.dll")
	u32 = syscall.NewLazyDLL("user32.dll")
)

// We have to bring in the kernel32 and user32 DLLs directly, so we can get
// access to some system calls that the core Go API lacks.
//
// Note that Windows appends some functions with W to indicate that wide
// characters (Unicode) are in use.  The documentation refers to them
// without this suffix, as the resolution is made via preprocessor.
var (
	procReadConsoleInput            = k32.NewProc("ReadConsoleInputW")
	procWaitForMultipleObjects      = k32.NewProc("WaitForMultipleObjects")
	procCreateEvent                 = k32.NewProc("CreateEventW")
	procSetEvent                    = k32.NewProc("SetEvent")
	procGetConsoleCursorInfo        = k32.NewProc("GetConsoleCursorInfo")
	procSetConsoleCursorInfo        = k32.NewProc("SetConsoleCursorInfo")
	procSetConsoleCursorPosition    = k32.NewProc("SetConsoleCursorPosition")
	procSetConsoleMode              = k32.NewProc("SetConsoleMode")
	procGetConsoleMode              = k32.NewProc("GetConsoleMode")
	procGetConsoleScreenBufferInfo  = k32.NewProc("GetConsoleScreenBufferInfo")
	procFillConsoleOutputAttribute  = k32.NewProc("FillConsoleOutputAttribute")
	procFillConsoleOutputCharacter  = k32.NewProc("FillConsoleOutputCharacterW")
	procSetConsoleWindowInfo        = k32.NewProc("SetConsoleWindowInfo")
	procSetConsoleScreenBufferSize  = k32.NewProc("SetConsoleScreenBufferSize")
	procSetConsoleTextAttribute     = k32.NewProc("SetConsoleTextAttribute")
	procGetLargestConsoleWindowSize = k32.NewProc("GetLargestConsoleWindowSize")
	procMessageBeep                 = u32.NewProc("MessageBeep")
)

const (
	w32Infinite    = ^uintptr(0)
	w32WaitObject0 = uintptr(0)
)

const (
	// VT100/XTerm escapes understood by the console
	vtShowCursor              = "\x1b[?25h"
	vtHideCursor              = "\x1b[?25l"
	vtCursorPos               = "\x1b[%d;%dH" // Note that it is Y then X
	vtSgr0                    = "\x1b[0m"
	vtBold                    = "\x1b[1m"
	vtUnderline               = "\x1b[4m"
	vtBlink                   = "\x1b[5m" // Not sure if this is processed
	vtReverse                 = "\x1b[7m"
	vtSetFg                   = "\x1b[38;5;%dm"
	vtSetBg                   = "\x1b[48;5;%dm"
	vtSetFgRGB                = "\x1b[38;2;%d;%d;%dm" // RGB
	vtSetBgRGB                = "\x1b[48;2;%d;%d;%dm" // RGB
	vtCursorDefault           = "\x1b[0 q"
	vtCursorBlinkingBlock     = "\x1b[1 q"
	vtCursorSteadyBlock       = "\x1b[2 q"
	vtCursorBlinkingUnderline = "\x1b[3 q"
	vtCursorSteadyUnderline   = "\x1b[4 q"
	vtCursorBlinkingBar       = "\x1b[5 q"
	vtCursorSteadyBar         = "\x1b[6 q"
	vtDisableAm               = "\x1b[?7l"
	vtEnableAm                = "\x1b[?7h"
	vtEnterCA                 = "\x1b[?1049h\x1b[22;0;0t"
	vtExitCA                  = "\x1b[?1049l\x1b[23;0;0t"
	vtDoubleUnderline         = "\x1b[4:2m"
	vtCurlyUnderline          = "\x1b[4:3m"
	vtDottedUnderline         = "\x1b[4:4m"
	vtDashedUnderline         = "\x1b[4:5m"
	vtUnderColor              = "\x1b[58:5:%dm"
	vtUnderColorRGB           = "\x1b[58:2::%d:%d:%dm"
	vtUnderColorReset         = "\x1b[59m"
	vtEnterUrl                = "\x1b]8;%s;%s\x1b\\" // NB arg 1 is id, arg 2 is url
	vtExitUrl                 = "\x1b]8;;\x1b\\"
	vtCursorColorRGB          = "\x1b]12;#%02x%02x%02x\007"
	vtCursorColorReset        = "\x1b]112\007"
	vtSaveTitle               = "\x1b[22;2t"
	vtRestoreTitle            = "\x1b[23;2t"
	vtSetTitle                = "\x1b]2;%s\x1b\\"
)

var vtCursorStyles = map[CursorStyle]string{
	CursorStyleDefault:           vtCursorDefault,
	CursorStyleBlinkingBlock:     vtCursorBlinkingBlock,
	CursorStyleSteadyBlock:       vtCursorSteadyBlock,
	CursorStyleBlinkingUnderline: vtCursorBlinkingUnderline,
	CursorStyleSteadyUnderline:   vtCursorSteadyUnderline,
	CursorStyleBlinkingBar:       vtCursorBlinkingBar,
	CursorStyleSteadyBar:         vtCursorSteadyBar,
}

// NewConsoleScreen returns a Screen for the Windows console associated
// with the current process.  The Screen makes use of the Windows Console
// API to display content and read events.
func NewConsoleScreen() (Screen, error) {
	return &baseScreen{screenImpl: &cScreen{}}, nil
}

func (s *cScreen) Init() error {
	s.eventQ = make(chan Event, 10)
	s.quit = make(chan struct{})
	s.scandone = make(chan struct{})
	in, e := syscall.Open("CONIN$", syscall.O_RDWR, 0)
	if e != nil {
		return e
	}
	s.in = in
	out, e := syscall.Open("CONOUT$", syscall.O_RDWR, 0)
	if e != nil {
		_ = syscall.Close(s.in)
		return e
	}
	s.out = out

	s.truecolor = true

	// ConEmu handling of colors and scrolling when in VT output mode is extremely poor.
	// The color palette will scroll even though characters do not, when
	// emitting stuff for the last character.  In the future we might change this to
	// look at specific versions of ConEmu if they fix the bug.
	// We can also try disabling auto margin mode.
	tryVt := true
	if os.Getenv("ConEmuPID") != "" {
		s.truecolor = false
		tryVt = false
	}
	switch os.Getenv("TCELL_TRUECOLOR") {
	case "disable":
		s.truecolor = false
	case "enable":
		s.truecolor = true
		tryVt = true
	}

	s.Lock()

	s.curx = -1
	s.cury = -1
	s.style = StyleDefault
	s.getCursorInfo(&s.ocursor)
	s.getConsoleInfo(&s.oscreen)
	s.getOutMode(&s.oomode)
	s.getInMode(&s.oimode)
	s.resize()

	s.fini = false
	s.setInMode(modeResizeEn | modeExtendFlg)

	// If a user needs to force old style console, they may do so
	// by setting TCELL_VTMODE to disable.  This is an undocumented safety net for now.
	// It may be removed in the future.  (This mostly exists because of ConEmu.)
	switch os.Getenv("TCELL_VTMODE") {
	case "disable":
		tryVt = false
	case "enable":
		tryVt = true
	}
	switch os.Getenv("TCELL_ALTSCREEN") {
	case "enable":
		s.disableAlt = false // also the default
	case "disable":
		s.disableAlt = true
	}
	if tryVt {
		s.setOutMode(modeVtOutput | modeNoAutoNL | modeCookedOut | modeUnderline)
		var om uint32
		s.getOutMode(&om)
		if om&modeVtOutput == modeVtOutput {
			s.vten = true
		} else {
			s.truecolor = false
			s.setOutMode(0)
		}
	} else {
		s.setOutMode(0)
	}

	s.Unlock()

	return s.engage()
}

func (s *cScreen) CharacterSet() string {
	// We are always UTF-16LE on Windows
	return "UTF-16LE"
}

func (s *cScreen) EnableMouse(...MouseFlags) {
	s.Lock()
	s.mouseEnabled = true
	s.enableMouse(true)
	s.Unlock()
}

func (s *cScreen) DisableMouse() {
	s.Lock()
	s.mouseEnabled = false
	s.enableMouse(false)
	s.Unlock()
}

func (s *cScreen) enableMouse(on bool) {
	if on {
		s.setInMode(modeResizeEn | modeMouseEn | modeExtendFlg)
	} else {
		s.setInMode(modeResizeEn | modeExtendFlg)
	}
}

// Windows lacks bracketed paste (for now)

func (s *cScreen) EnablePaste() {}

func (s *cScreen) DisablePaste() {}

func (s *cScreen) EnableFocus() {
	s.Lock()
	s.focusEnable = true
	s.Unlock()
}

func (s *cScreen) DisableFocus() {
	s.Lock()
	s.focusEnable = false
	s.Unlock()
}

func (s *cScreen) Fini() {
	s.finiOnce.Do(func() {
		close(s.quit)
		s.disengage()
	})
}

func (s *cScreen) disengage() {
	s.Lock()
	if !s.running {
		s.Unlock()
		return
	}
	s.running = false
	stopQ := s.stopQ
	_, _, _ = procSetEvent.Call(uintptr(s.cancelflag))
	close(stopQ)
	s.Unlock()

	s.wg.Wait()

	if s.vten {
		s.emitVtString(vtCursorStyles[CursorStyleDefault])
		s.emitVtString(vtCursorColorReset)
		s.emitVtString(vtEnableAm)
		if !s.disableAlt {
			s.emitVtString(vtRestoreTitle)
			s.emitVtString(vtExitCA)
		}
	} else if !s.disableAlt {
		s.clearScreen(StyleDefault, s.vten)
		s.setCursorPos(0, 0, false)
	}
	s.setCursorInfo(&s.ocursor)
	s.setBufferSize(int(s.oscreen.size.x), int(s.oscreen.size.y))
	s.setInMode(s.oimode)
	s.setOutMode(s.oomode)
	_, _, _ = procSetConsoleTextAttribute.Call(
		uintptr(s.out),
		uintptr(s.mapStyle(StyleDefault)))
}

func (s *cScreen) engage() error {
	s.Lock()
	defer s.Unlock()
	if s.running {
		return errors.New("already engaged")
	}
	s.stopQ = make(chan struct{})
	cf, _, e := procCreateEvent.Call(
		uintptr(0),
		uintptr(1),
		uintptr(0),
		uintptr(0))
	if cf == uintptr(0) {
		return e
	}
	s.running = true
	s.cancelflag = syscall.Handle(cf)
	s.enableMouse(s.mouseEnabled)

	if s.vten {
		s.setOutMode(modeVtOutput | modeNoAutoNL | modeCookedOut | modeUnderline)
		if !s.disableAlt {
			s.emitVtString(vtSaveTitle)
			s.emitVtString(vtEnterCA)
		}
		s.emitVtString(vtDisableAm)
		if s.title != "" {
			s.emitVtString(fmt.Sprintf(vtSetTitle, s.title))
		}
	} else {
		s.setOutMode(0)
	}

	s.clearScreen(s.style, s.vten)
	s.hideCursor()

	s.cells.Invalidate()
	s.hideCursor()
	s.resize()
	s.draw()
	s.doCursor()

	s.wg.Add(1)
	go s.scanInput(s.stopQ)
	return nil
}

type cursorInfo struct {
	size    uint32
	visible uint32
}

type coord struct {
	x int16
	y int16
}

func (c coord) uintptr() uintptr {
	// little endian, put x first
	return uintptr(c.x) | (uintptr(c.y) << 16)
}

type rect struct {
	left   int16
	top    int16
	right  int16
	bottom int16
}

func (s *cScreen) emitVtString(vs string) {
	esc := utf16.Encode([]rune(vs))
	_ = syscall.WriteConsole(s.out, &esc[0], uint32(len(esc)), nil, nil)
}

func (s *cScreen) showCursor() {
	if s.vten {
		s.emitVtString(vtShowCursor)
		s.emitVtString(vtCursorStyles[s.cursorStyle])
		if s.cursorColor == ColorReset {
			s.emitVtString(vtCursorColorReset)
		} else if s.cursorColor.Valid() {
			r, g, b := s.cursorColor.RGB()
			s.emitVtString(fmt.Sprintf(vtCursorColorRGB, r, g, b))
		}
	} else {
		s.setCursorInfo(&cursorInfo{size: 100, visible: 1})
	}
}

func (s *cScreen) hideCursor() {
	if s.vten {
		s.emitVtString(vtHideCursor)
	} else {
		s.setCursorInfo(&cursorInfo{size: 1, visible: 0})
	}
}

func (s *cScreen) ShowCursor(x, y int) {
	s.Lock()
	if !s.fini {
		s.curx = x
		s.cury = y
	}
	s.doCursor()
	s.Unlock()
}

func (s *cScreen) SetCursor(cs CursorStyle, cc Color) {
	s.Lock()
	if !s.fini {
		if _, ok := vtCursorStyles[cs]; ok {
			s.cursorStyle = cs
			s.cursorColor = cc
			s.doCursor()
		}
	}
	s.Unlock()
}

func (s *cScreen) doCursor() {
	x, y := s.curx, s.cury

	if x < 0 || y < 0 || x >= s.w || y >= s.h {
		s.hideCursor()
	} else {
		s.setCursorPos(x, y, s.vten)
		s.showCursor()
	}
}

func (s *cScreen) HideCursor() {
	s.ShowCursor(-1, -1)
}

type inputRecord struct {
	typ  uint16
	_    uint16
	data [16]byte
}

const (
	keyEvent    uint16 = 1
	mouseEvent  uint16 = 2
	resizeEvent uint16 = 4
	menuEvent   uint16 = 8 // don't use
	focusEvent  uint16 = 16
)

type mouseRecord struct {
	x     int16
	y     int16
	btns  uint32
	mod   uint32
	flags uint32
}

type focusRecord struct {
	focused int32 // actually BOOL
}

const (
	mouseHWheeled uint32 = 0x8
	mouseVWheeled uint32 = 0x4
	// mouseDoubleClick uint32 = 0x2
	// mouseMoved       uint32 = 0x1
)

type resizeRecord struct {
	x int16
	y int16
}

type keyRecord struct {
	isdown int32
	repeat uint16
	kcode  uint16
	scode  uint16
	ch     uint16
	mod    uint32
}

const (
	// Constants per Microsoft.  We don't put the modifiers
	// here.
	vkCancel = 0x03
	vkBack   = 0x08 // Backspace
	vkTab    = 0x09
	vkClear  = 0x0c
	vkReturn = 0x0d
	vkPause  = 0x13
	vkEscape = 0x1b
	vkSpace  = 0x20
	vkPrior  = 0x21 // PgUp
	vkNext   = 0x22 // PgDn
	vkEnd    = 0x23
	vkHome   = 0x24
	vkLeft   = 0x25
	vkUp     = 0x26
	vkRight  = 0x27
	vkDown   = 0x28
	vkPrint  = 0x2a
	vkPrtScr = 0x2c
	vkInsert = 0x2d
	vkDelete = 0x2e
	vkHelp   = 0x2f
	vkF1     = 0x70
	vkF2     = 0x71
	vkF3     = 0x72
	vkF4     = 0x73
	vkF5     = 0x74
	vkF6     = 0x75
	vkF7     = 0x76
	vkF8     = 0x77
	vkF9     = 0x78
	vkF10    = 0x79
	vkF11    = 0x7a
	vkF12    = 0x7b
	vkF13    = 0x7c
	vkF14    = 0x7d
	vkF15    = 0x7e
	vkF16    = 0x7f
	vkF17    = 0x80
	vkF18    = 0x81
	vkF19    = 0x82
	vkF20    = 0x83
	vkF21    = 0x84
	vkF22    = 0x85
	vkF23    = 0x86
	vkF24    = 0x87
)

var vkKeys = map[uint16]Key{
	vkCancel: KeyCancel,
	vkBack:   KeyBackspace,
	vkTab:    KeyTab,
	vkClear:  KeyClear,
	vkPause:  KeyPause,
	vkPrint:  KeyPrint,
	vkPrtScr: KeyPrint,
	vkPrior:  KeyPgUp,
	vkNext:   KeyPgDn,
	vkReturn: KeyEnter,
	vkEnd:    KeyEnd,
	vkHome:   KeyHome,
	vkLeft:   KeyLeft,
	vkUp:     KeyUp,
	vkRight:  KeyRight,
	vkDown:   KeyDown,
	vkInsert: KeyInsert,
	vkDelete: KeyDelete,
	vkHelp:   KeyHelp,
	vkEscape: KeyEscape,
	vkSpace:  ' ',
	vkF1:     KeyF1,
	vkF2:     KeyF2,
	vkF3:     KeyF3,
	vkF4:     KeyF4,
	vkF5:     KeyF5,
	vkF6:     KeyF6,
	vkF7:     KeyF7,
	vkF8:     KeyF8,
	vkF9:     KeyF9,
	vkF10:    KeyF10,
	vkF11:    KeyF11,
	vkF12:    KeyF12,
	vkF13:    KeyF13,
	vkF14:    KeyF14,
	vkF15:    KeyF15,
	vkF16:    KeyF16,
	vkF17:    KeyF17,
	vkF18:    KeyF18,
	vkF19:    KeyF19,
	vkF20:    KeyF20,
	vkF21:    KeyF21,
	vkF22:    KeyF22,
	vkF23:    KeyF23,
	vkF24:    KeyF24,
}

// NB: All Windows platforms are little endian.  We assume this
// never, ever change.  The following code is endian safe. and does
// not use unsafe pointers.
func getu32(v []byte) uint32 {
	return uint32(v[0]) + (uint32(v[1]) << 8) + (uint32(v[2]) << 16) + (uint32(v[3]) << 24)
}

func geti32(v []byte) int32 {
	return int32(getu32(v))
}

func getu16(v []byte) uint16 {
	return uint16(v[0]) + (uint16(v[1]) << 8)
}

func geti16(v []byte) int16 {
	return int16(getu16(v))
}

// Convert windows dwControlKeyState to modifier mask
func mod2mask(cks uint32) ModMask {
	mm := ModNone
	// Left or right control
	ctrl := (cks & (0x0008 | 0x0004)) != 0
	// Left or right alt
	alt := (cks & (0x0002 | 0x0001)) != 0
	// Filter out ctrl+alt (it means AltGr)
	if !(ctrl && alt) {
		if ctrl {
			mm |= ModCtrl
		}
		if alt {
			mm |= ModAlt
		}
	}
	// Any shift
	if (cks & 0x0010) != 0 {
		mm |= ModShift
	}
	return mm
}

func mrec2btns(mbtns, flags uint32) ButtonMask {
	btns := ButtonNone
	if mbtns&0x1 != 0 {
		btns |= Button1
	}
	if mbtns&0x2 != 0 {
		btns |= Button2
	}
	if mbtns&0x4 != 0 {
		btns |= Button3
	}
	if mbtns&0x8 != 0 {
		btns |= Button4
	}
	if mbtns&0x10 != 0 {
		btns |= Button5
	}
	if mbtns&0x20 != 0 {
		btns |= Button6
	}
	if mbtns&0x40 != 0 {
		btns |= Button7
	}
	if mbtns&0x80 != 0 {
		btns |= Button8
	}

	if flags&mouseVWheeled != 0 {
		if mbtns&0x80000000 == 0 {
			btns |= WheelUp
		} else {
			btns |= WheelDown
		}
	}
	if flags&mouseHWheeled != 0 {
		if mbtns&0x80000000 == 0 {
			btns |= WheelRight
		} else {
			btns |= WheelLeft
		}
	}
	return btns
}

func (s *cScreen) postEvent(ev Event) {
	select {
	case s.eventQ <- ev:
	case <-s.quit:
	}
}

func (s *cScreen) getConsoleInput() error {
	// cancelFlag comes first as WaitForMultipleObjects returns the lowest index
	// in the event that both events are signalled.
	waitObjects := []syscall.Handle{s.cancelflag, s.in}
	// As arrays are contiguous in memory, a pointer to the first object is the
	// same as a pointer to the array itself.
	pWaitObjects := unsafe.Pointer(&waitObjects[0])

	rv, _, er := procWaitForMultipleObjects.Call(
		uintptr(len(waitObjects)),
		uintptr(pWaitObjects),
		uintptr(0),
		w32Infinite)
	// WaitForMultipleObjects returns WAIT_OBJECT_0 + the index.
	switch rv {
	case w32WaitObject0: // s.cancelFlag
		return errors.New("cancelled")
	case w32WaitObject0 + 1: // s.in
		rec := &inputRecord{}
		var nrec int32
		rv, _, er := procReadConsoleInput.Call(
			uintptr(s.in),
			uintptr(unsafe.Pointer(rec)),
			uintptr(1),
			uintptr(unsafe.Pointer(&nrec)))
		if rv == 0 {
			return er
		}
		if nrec != 1 {
			return nil
		}
		switch rec.typ {
		case keyEvent:
			krec := &keyRecord{}
			krec.isdown = geti32(rec.data[0:])
			krec.repeat = getu16(rec.data[4:])
			krec.kcode = getu16(rec.data[6:])
			krec.scode = getu16(rec.data[8:])
			krec.ch = getu16(rec.data[10:])
			krec.mod = getu32(rec.data[12:])

			if krec.isdown == 0 || krec.repeat < 1 {
				// it's a key release event, ignore it
				return nil
			}
			if krec.ch != 0 {
				// synthesized key code
				for krec.repeat > 0 {
					// convert shift+tab to backtab
					if mod2mask(krec.mod) == ModShift && krec.ch == vkTab {
						s.postEvent(NewEventKey(KeyBacktab, 0, ModNone))
					} else {
						s.postEvent(NewEventKey(KeyRune, rune(krec.ch), mod2mask(krec.mod)))
					}
					krec.repeat--
				}
				return nil
			}
			key := KeyNUL // impossible on Windows
			ok := false
			if key, ok = vkKeys[krec.kcode]; !ok {
				return nil
			}
			for krec.repeat > 0 {
				s.postEvent(NewEventKey(key, rune(krec.ch), mod2mask(krec.mod)))
				krec.repeat--
			}

		case mouseEvent:
			var mrec mouseRecord
			mrec.x = geti16(rec.data[0:])
			mrec.y = geti16(rec.data[2:])
			mrec.btns = getu32(rec.data[4:])
			mrec.mod = getu32(rec.data[8:])
			mrec.flags = getu32(rec.data[12:])
			btns := mrec2btns(mrec.btns, mrec.flags)
			// we ignore double click, events are delivered normally
			s.postEvent(NewEventMouse(int(mrec.x), int(mrec.y), btns, mod2mask(mrec.mod)))

		case resizeEvent:
			var rrec resizeRecord
			rrec.x = geti16(rec.data[0:])
			rrec.y = geti16(rec.data[2:])
			s.postEvent(NewEventResize(int(rrec.x), int(rrec.y)))

		case focusEvent:
			var focus focusRecord
			focus.focused = geti32(rec.data[0:])
			s.Lock()
			enabled := s.focusEnable
			s.Unlock()
			if enabled {
				s.postEvent(NewEventFocus(focus.focused != 0))
			}

		default:
		}
	default:
		return er
	}

	return nil
}

func (s *cScreen) scanInput(stopQ chan struct{}) {
	defer s.wg.Done()
	for {
		select {
		case <-stopQ:
			return
		default:
		}
		if e := s.getConsoleInput(); e != nil {
			return
		}
	}
}

func (s *cScreen) Colors() int {
	if s.vten {
		return 1 << 24
	}
	// Windows console can display 8 colors, in either low or high intensity
	return 16
}

var vgaColors = map[Color]uint16{
	ColorBlack:   0,
	ColorMaroon:  0x4,
	ColorGreen:   0x2,
	ColorNavy:    0x1,
	ColorOlive:   0x6,
	ColorPurple:  0x5,
	ColorTeal:    0x3,
	ColorSilver:  0x7,
	ColorGrey:    0x8,
	ColorRed:     0xc,
	ColorLime:    0xa,
	ColorBlue:    0x9,
	ColorYellow:  0xe,
	ColorFuchsia: 0xd,
	ColorAqua:    0xb,
	ColorWhite:   0xf,
}

// Windows uses RGB signals
func mapColor2RGB(c Color) uint16 {
	winLock.Lock()
	if v, ok := winColors[c]; ok {
		c = v
	} else {
		v = FindColor(c, winPalette)
		winColors[c] = v
		c = v
	}
	winLock.Unlock()

	if vc, ok := vgaColors[c]; ok {
		return vc
	}
	return 0
}

// Map a tcell style to Windows attributes
func (s *cScreen) mapStyle(style Style) uint16 {
	f, b, a := style.fg, style.bg, style.attrs
	fa := s.oscreen.attrs & 0xf
	ba := (s.oscreen.attrs) >> 4 & 0xf
	if f != ColorDefault && f != ColorReset {
		fa = mapColor2RGB(f)
	}
	if b != ColorDefault && b != ColorReset {
		ba = mapColor2RGB(b)
	}
	var attr uint16
	// We simulate reverse by doing the color swap ourselves.
	// Apparently windows cannot really do this except in DBCS
	// views.
	if a&AttrReverse != 0 {
		attr = ba
		attr |= fa << 4
	} else {
		attr = fa
		attr |= ba << 4
	}
	if a&AttrBold != 0 {
		attr |= 0x8
	}
	if a&AttrDim != 0 {
		attr &^= 0x8
	}
	if a&AttrUnderline != 0 {
		// Best effort -- doesn't seem to work though.
		attr |= 0x8000
	}
	// Blink is unsupported
	return attr
}

func (s *cScreen) makeVtStyle(style Style) string {
	esc := &strings.Builder{}

	fg, bg, attrs := style.fg, style.bg, style.attrs
	us, uc := style.ulStyle, style.ulColor

	esc.WriteString(vtSgr0)
	if attrs&(AttrBold|AttrDim) == AttrBold {
		esc.WriteString(vtBold)
	}
	if attrs&AttrBlink != 0 {
		esc.WriteString(vtBlink)
	}
	if us != UnderlineStyleNone {
		if uc == ColorReset {
			esc.WriteString(vtUnderColorReset)
		} else if uc.IsRGB() {
			r, g, b := uc.RGB()
			_, _ = fmt.Fprintf(esc, vtUnderColorRGB, int(r), int(g), int(b))
		} else if uc.Valid() {
			_, _ = fmt.Fprintf(esc, vtUnderColor, uc&0xff)
		}

		esc.WriteString(vtUnderline)
		// legacy ConHost does not understand these but Terminal does
		switch us {
		case UnderlineStyleSolid:
		case UnderlineStyleDouble:
			esc.WriteString(vtDoubleUnderline)
		case UnderlineStyleCurly:
			esc.WriteString(vtCurlyUnderline)
		case UnderlineStyleDotted:
			esc.WriteString(vtDottedUnderline)
		case UnderlineStyleDashed:
			esc.WriteString(vtDashedUnderline)
		}
	}

	if attrs&AttrReverse != 0 {
		esc.WriteString(vtReverse)
	}
	if fg.IsRGB() {
		r, g, b := fg.RGB()
		_, _ = fmt.Fprintf(esc, vtSetFgRGB, r, g, b)
	} else if fg.Valid() {
		_, _ = fmt.Fprintf(esc, vtSetFg, fg&0xff)
	}
	if bg.IsRGB() {
		r, g, b := bg.RGB()
		_, _ = fmt.Fprintf(esc, vtSetBgRGB, r, g, b)
	} else if bg.Valid() {
		_, _ = fmt.Fprintf(esc, vtSetBg, bg&0xff)
	}
	// URL string can be long, so don't send it unless we really need to
	if style.url != "" {
		_, _ = fmt.Fprintf(esc, vtEnterUrl, style.urlId, style.url)
	} else {
		esc.WriteString(vtExitUrl)
	}

	return esc.String()
}

func (s *cScreen) sendVtStyle(style Style) {
	s.emitVtString(s.makeVtStyle(style))
}

func (s *cScreen) writeString(x, y int, style Style, vtBuf, ch []uint16) {
	// we assume the caller has hidden the cursor
	if len(ch) == 0 {
		return
	}

	if s.vten {
		vtBuf = append(vtBuf, utf16.Encode([]rune(fmt.Sprintf(vtCursorPos, y+1, x+1)))...)
		styleStr := s.makeVtStyle(style)
		vtBuf = append(vtBuf, utf16.Encode([]rune(styleStr))...)
		vtBuf = append(vtBuf, ch...)
		_ = syscall.WriteConsole(s.out, &vtBuf[0], uint32(len(vtBuf)), nil, nil)
		vtBuf = vtBuf[:0]
	} else {
		s.setCursorPos(x, y, s.vten)
		_, _, _ = procSetConsoleTextAttribute.Call(
			uintptr(s.out),
			uintptr(s.mapStyle(style)))
		_ = syscall.WriteConsole(s.out, &ch[0], uint32(len(ch)), nil, nil)
	}
}

func (s *cScreen) draw() {
	// allocate a scratch line bit enough for no combining chars.
	// if you have combining characters, you may pay for extra allocations.
	buf := make([]uint16, 0, s.w)
	var vtBuf []uint16
	wcs := buf[:]
	lstyle := styleInvalid

	lx, ly := -1, -1
	ra := make([]rune, 1)

	for y := 0; y < s.h; y++ {
		for x := 0; x < s.w; x++ {
			mainc, combc, style, width := s.cells.GetContent(x, y)
			dirty := s.cells.Dirty(x, y)
			if style == StyleDefault {
				style = s.style
			}

			if !dirty || style != lstyle {
				// write out any data queued thus far
				// because we are going to skip over some
				// cells, or because we need to change styles
				s.writeString(lx, ly, lstyle, vtBuf, wcs)
				wcs = buf[0:0]
				lstyle = StyleDefault
				if !dirty {
					continue
				}
			}
			if x > s.w-width {
				mainc = ' '
				combc = nil
				width = 1
			}
			if len(wcs) == 0 {
				lstyle = style
				lx = x
				ly = y
			}
			ra[0] = mainc
			wcs = append(wcs, utf16.Encode(ra)...)
			if len(combc) != 0 {
				wcs = append(wcs, utf16.Encode(combc)...)
			}
			for dx := 0; dx < width; dx++ {
				s.cells.SetDirty(x+dx, y, false)
			}
			x += width - 1
		}
		s.writeString(lx, ly, lstyle, vtBuf, wcs)
		wcs = buf[0:0]
		lstyle = styleInvalid
	}
}

func (s *cScreen) Show() {
	s.Lock()
	if !s.fini {
		s.hideCursor()
		s.resize()
		s.draw()
		s.doCursor()
	}
	s.Unlock()
}

func (s *cScreen) Sync() {
	s.Lock()
	if !s.fini {
		s.cells.Invalidate()
		s.hideCursor()
		s.resize()
		s.draw()
		s.doCursor()
	}
	s.Unlock()
}

type consoleInfo struct {
	size  coord
	pos   coord
	attrs uint16
	win   rect
	maxsz coord
}

func (s *cScreen) getConsoleInfo(info *consoleInfo) {
	_, _, _ = procGetConsoleScreenBufferInfo.Call(
		uintptr(s.out),
		uintptr(unsafe.Pointer(info)))
}

func (s *cScreen) getCursorInfo(info *cursorInfo) {
	_, _, _ = procGetConsoleCursorInfo.Call(
		uintptr(s.out),
		uintptr(unsafe.Pointer(info)))
}

func (s *cScreen) setCursorInfo(info *cursorInfo) {
	_, _, _ = procSetConsoleCursorInfo.Call(
		uintptr(s.out),
		uintptr(unsafe.Pointer(info)))
}

func (s *cScreen) setCursorPos(x, y int, vtEnable bool) {
	if vtEnable {
		// Note that the string is Y first.  Origin is 1,1.
		s.emitVtString(fmt.Sprintf(vtCursorPos, y+1, x+1))
	} else {
		_, _, _ = procSetConsoleCursorPosition.Call(
			uintptr(s.out),
			coord{int16(x), int16(y)}.uintptr())
	}
}

func (s *cScreen) setBufferSize(x, y int) {
	_, _, _ = procSetConsoleScreenBufferSize.Call(
		uintptr(s.out),
		coord{int16(x), int16(y)}.uintptr())
}

func (s *cScreen) Size() (int, int) {
	s.Lock()
	w, h := s.w, s.h
	s.Unlock()

	return w, h
}

func (s *cScreen) SetSize(w, h int) {
	xy, _, _ := procGetLargestConsoleWindowSize.Call(uintptr(s.out))

	// xy is little endian packed
	y := int(xy >> 16)
	x := int(xy & 0xffff)

	if x == 0 || y == 0 {
		return
	}

	// This is a hacky workaround for Windows Terminal.
	// Essentially Windows Terminal (Windows 11) does not support application
	// initiated resizing.  To detect this, we look for an extremely large size
	// for the maximum width.  If it is > 500, then this is almost certainly
	// Windows Terminal, and won't support this.  (Note that the legacy console
	// does support application resizing.)
	if x >= 500 {
		return
	}

	s.setBufferSize(x, y)
	r := rect{0, 0, int16(w - 1), int16(h - 1)}
	_, _, _ = procSetConsoleWindowInfo.Call(
		uintptr(s.out),
		uintptr(1),
		uintptr(unsafe.Pointer(&r)))

	s.resize()
}

func (s *cScreen) resize() {
	info := consoleInfo{}
	s.getConsoleInfo(&info)

	w := int((info.win.right - info.win.left) + 1)
	h := int((info.win.bottom - info.win.top) + 1)

	if s.w == w && s.h == h {
		return
	}

	s.cells.Resize(w, h)
	s.w = w
	s.h = h

	s.setBufferSize(w, h)

	r := rect{0, 0, int16(w - 1), int16(h - 1)}
	_, _, _ = procSetConsoleWindowInfo.Call(
		uintptr(s.out),
		uintptr(1),
		uintptr(unsafe.Pointer(&r)))
	select {
	case s.eventQ <- NewEventResize(w, h):
	default:
	}
}

func (s *cScreen) clearScreen(style Style, vtEnable bool) {
	if vtEnable {
		s.sendVtStyle(style)
		row := strings.Repeat(" ", s.w)
		for y := 0; y < s.h; y++ {
			s.setCursorPos(0, y, vtEnable)
			s.emitVtString(row)
		}
		s.setCursorPos(0, 0, vtEnable)

	} else {
		pos := coord{0, 0}
		attr := s.mapStyle(style)
		x, y := s.w, s.h
		scratch := uint32(0)
		count := uint32(x * y)

		_, _, _ = procFillConsoleOutputAttribute.Call(
			uintptr(s.out),
			uintptr(attr),
			uintptr(count),
			pos.uintptr(),
			uintptr(unsafe.Pointer(&scratch)))
		_, _, _ = procFillConsoleOutputCharacter.Call(
			uintptr(s.out),
			uintptr(' '),
			uintptr(count),
			pos.uintptr(),
			uintptr(unsafe.Pointer(&scratch)))
	}
}

const (
	// Input modes
	modeExtendFlg uint32 = 0x0080
	modeMouseEn          = 0x0010
	modeResizeEn         = 0x0008
	// modeCooked          = 0x0001
	// modeVtInput         = 0x0200

	// Output modes
	modeCookedOut uint32 = 0x0001
	modeVtOutput         = 0x0004
	modeNoAutoNL         = 0x0008
	modeUnderline        = 0x0010 // ENABLE_LVB_GRID_WORLDWIDE, needed for underlines
	// modeWrapEOL          = 0x0002
)

func (s *cScreen) setInMode(mode uint32) {
	_, _, _ = procSetConsoleMode.Call(
		uintptr(s.in),
		uintptr(mode))
}

func (s *cScreen) setOutMode(mode uint32) {
	_, _, _ = procSetConsoleMode.Call(
		uintptr(s.out),
		uintptr(mode))
}

func (s *cScreen) getInMode(v *uint32) {
	_, _, _ = procGetConsoleMode.Call(
		uintptr(s.in),
		uintptr(unsafe.Pointer(v)))
}

func (s *cScreen) getOutMode(v *uint32) {
	_, _, _ = procGetConsoleMode.Call(
		uintptr(s.out),
		uintptr(unsafe.Pointer(v)))
}

func (s *cScreen) SetStyle(style Style) {
	s.Lock()
	s.style = style
	s.Unlock()
}

func (s *cScreen) SetTitle(title string) {
	s.Lock()
	s.title = title
	if s.vten {
		s.emitVtString(fmt.Sprintf(vtSetTitle, title))
	}
	s.Unlock()
}

// No fallback rune support, since we have Unicode.  Yay!

func (s *cScreen) RegisterRuneFallback(_ rune, _ string) {
}

func (s *cScreen) UnregisterRuneFallback(_ rune) {
}

func (s *cScreen) CanDisplay(_ rune, _ bool) bool {
	// We presume we can display anything -- we're Unicode.
	// (Sadly this not precisely true.  Combining characters are especially
	// poorly supported under Windows.)
	return true
}

func (s *cScreen) HasMouse() bool {
	return true
}

func (s *cScreen) SetClipboard(_ []byte) {
}

func (s *cScreen) GetClipboard() {
}

func (s *cScreen) Resize(int, int, int, int) {}

func (s *cScreen) HasKey(k Key) bool {
	// Microsoft has codes for some keys, but they are unusual,
	// so we don't include them.  We include all the typical
	// 101, 105 key layout keys.
	valid := map[Key]bool{
		KeyBackspace: true,
		KeyTab:       true,
		KeyEscape:    true,
		KeyPause:     true,
		KeyPrint:     true,
		KeyPgUp:      true,
		KeyPgDn:      true,
		KeyEnter:     true,
		KeyEnd:       true,
		KeyHome:      true,
		KeyLeft:      true,
		KeyUp:        true,
		KeyRight:     true,
		KeyDown:      true,
		KeyInsert:    true,
		KeyDelete:    true,
		KeyF1:        true,
		KeyF2:        true,
		KeyF3:        true,
		KeyF4:        true,
		KeyF5:        true,
		KeyF6:        true,
		KeyF7:        true,
		KeyF8:        true,
		KeyF9:        true,
		KeyF10:       true,
		KeyF11:       true,
		KeyF12:       true,
		KeyRune:      true,
	}

	return valid[k]
}

func (s *cScreen) Beep() error {
	// A simple beep. If the sound card is not available, the sound is generated
	// using the speaker.
	//
	// Reference:
	// https://docs.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-messagebeep
	const simpleBeep = 0xffffffff
	if rv, _, err := procMessageBeep.Call(simpleBeep); rv == 0 {
		return err
	}
	return nil
}

func (s *cScreen) Suspend() error {
	s.disengage()
	return nil
}

func (s *cScreen) Resume() error {
	return s.engage()
}

func (s *cScreen) Tty() (Tty, bool) {
	return nil, false
}

func (s *cScreen) GetCells() *CellBuffer {
	return &s.cells
}

func (s *cScreen) EventQ() chan Event {
	return s.eventQ
}

func (s *cScreen) StopQ() <-chan struct{} {
	return s.quit
}
