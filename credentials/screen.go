package credentials

// A login's output is read as a terminal shows it, not as the bytes it
// writes. claude setup-token 2.1.321 (Linux, 2026-11-15) draws its screen
// with cursor moves: words are placed by column (`\x1b[7G`) rather than
// separated by spaces, and a cell that already shows the right character is
// skipped, so the token it prints jumps over the characters an earlier frame
// left there. Taking the escapes out of the bytes loses those characters;
// reading the screen keeps them.

import "unicode/utf8"

// ptyRows is the login terminal's height.
const ptyRows = 50

// scrollback is how many rows above the screen are kept, so a long line that
// wrapped and scrolled is still read whole.
const scrollback = 500

// maxParams bounds an escape sequence's parameters: past it, the sequence
// is read to its end and ignored.
const maxParams = 64

// Parser states.
const (
	ground = iota
	escape
	escInter // ESC and intermediates, such as ESC ( B
	csi
	str    // OSC, DCS, APC, PM, SOS: up to BEL or ESC \
	strEsc // ESC inside a string
)

// wideTail is the cell a wide character's second half takes.
const wideTail = -1

type row struct {
	cells []rune // 0: blank
	cont  bool   // continues the row above: the line wrapped at the edge
	dirty bool   // changed since it was last read
}

// screen is a small terminal: it keeps the characters a program's output
// leaves on a grid of w columns and h rows, and the rows above it that
// scrolled away. It calls seen with each line that changed: on each carriage
// return and line feed, and when report is called.
type screen struct {
	w, h   int
	rows   []*row // the last h are the screen
	x, y   int    // the cursor, on the screen
	sx, sy int    // the saved cursor
	wrap   bool   // the last column was written: the next character wraps
	state  int
	params []byte
	utf    []byte // a character's bytes read so far
	// paste: the program turned bracketed paste on (CSI ? 2004 h) and not
	// off since: it tells a paste from typing.
	paste bool
	seen  func(line string)
}

func newScreen(w, h int, seen func(string)) *screen {
	s := &screen{w: w, h: h, seen: seen}
	for range h {
		s.rows = append(s.rows, s.newRow())
	}
	return s
}

func (s *screen) newRow() *row { return &row{cells: make([]rune, s.w)} }

func (s *screen) top() int { return len(s.rows) - s.h }

func (s *screen) at(y int) *row { return s.rows[s.top()+y] }

// Write takes what the program printed.
func (s *screen) Write(p []byte) (int, error) {
	for _, b := range p {
		s.byte(b)
	}
	return len(p), nil
}

func (s *screen) byte(b byte) {
	switch s.state {
	case ground:
		s.ground(b)
	case escape:
		s.escape(b)
	case escInter:
		if b >= 0x30 && b <= 0x7e {
			s.state = ground
		} else if b == 0x1b {
			s.state = escape
		} else if b < 0x20 || b > 0x2f {
			s.state = ground
			s.ground(b)
		}
	case csi:
		switch {
		case b >= 0x40 && b <= 0x7e:
			s.state = ground
			if len(s.params) <= maxParams {
				s.csi(b)
			}
		case b >= 0x20 && b <= 0x3f:
			if len(s.params) <= maxParams {
				s.params = append(s.params, b)
			}
		case b == 0x1b:
			s.state = escape
		case b < 0x20:
			s.control(b)
		default:
			s.state = ground
		}
	case str:
		switch b {
		case 0x07:
			s.state = ground
		case 0x1b:
			s.state = strEsc
		}
	case strEsc:
		// ESC \ ends the string; any other ESC ends it and starts a new one.
		s.state = escape
		if b == '\\' {
			s.state = ground
		} else {
			s.escape(b)
		}
	}
}

func (s *screen) ground(b byte) {
	if b == 0x1b {
		s.utf = s.utf[:0]
		s.state = escape
		return
	}
	if b < 0x20 || b == 0x7f {
		s.utf = s.utf[:0]
		s.control(b)
		return
	}
	if b < 0x80 {
		s.utf = s.utf[:0] // a character cut short is dropped
		s.put(rune(b))
		return
	}
	s.utf = append(s.utf, b)
	if !utf8.FullRune(s.utf) {
		return
	}
	r, _ := utf8.DecodeRune(s.utf)
	s.utf = s.utf[:0]
	s.put(r)
}

func (s *screen) control(b byte) {
	switch b {
	case '\r':
		s.x, s.wrap = 0, false
		s.report(false)
	case '\n', 0x0b, 0x0c:
		// A terminal's output processing (ONLCR, which libuv keeps in raw
		// mode) makes a line feed a new line.
		s.report(false)
		s.lineFeed()
		s.x = 0
	case '\b':
		if s.x > 0 {
			s.x--
		}
		s.wrap = false
	case '\t':
		s.x = min((s.x/8+1)*8, s.w-1)
		s.wrap = false
	}
}

func (s *screen) escape(b byte) {
	s.state = ground
	switch {
	case b == '[':
		s.state, s.params = csi, s.params[:0]
	case b == ']' || b == 'P' || b == '_' || b == '^' || b == 'X':
		s.state = str
	case b == '7':
		s.sx, s.sy = s.x, s.y
	case b == '8':
		s.x, s.y, s.wrap = s.sx, s.sy, false
	case b == 'D':
		s.lineFeed()
	case b == 'E':
		s.lineFeed()
		s.x = 0
	case b == 'M':
		s.y = max(s.y-1, 0)
		s.wrap = false
	case b == 'c':
		s.erase(0, 0, s.h-1, s.w-1)
		s.x, s.y, s.wrap = 0, 0, false
	case b >= 0x20 && b <= 0x2f:
		s.state = escInter
	case b == 0x1b:
		s.state = escape
	}
}

// csi runs a control sequence: params, then its final byte f.
func (s *screen) csi(f byte) {
	p := s.params
	if len(p) > 0 && (p[0] == '?' || p[0] == '<' || p[0] == '=' || p[0] == '>') {
		if p[0] == '?' && (f == 'h' || f == 'l') {
			for _, n := range numbers(p[1:]) {
				if n == 2004 {
					s.paste = f == 'h'
				}
			}
		}
		return
	}
	for _, b := range p {
		if b >= 0x20 && b <= 0x2f { // an intermediate: not one of these
			return
		}
	}
	n := numbers(p)
	arg := func(i, def int) int {
		if i < len(n) && n[i] > 0 {
			return n[i]
		}
		return def
	}
	s.wrap = false
	switch f {
	case 'A':
		s.y = max(s.y-arg(0, 1), 0)
	case 'B', 'e':
		s.y = min(s.y+arg(0, 1), s.h-1)
	case 'C', 'a':
		s.x = min(s.x+arg(0, 1), s.w-1)
	case 'D':
		s.x = max(s.x-arg(0, 1), 0)
	case 'E':
		s.y, s.x = min(s.y+arg(0, 1), s.h-1), 0
	case 'F':
		s.y, s.x = max(s.y-arg(0, 1), 0), 0
	case 'G', '`':
		s.x = min(arg(0, 1), s.w) - 1
	case 'd':
		s.y = min(arg(0, 1), s.h) - 1
	case 'H', 'f':
		s.y, s.x = min(arg(0, 1), s.h)-1, min(arg(1, 1), s.w)-1
	case 'K':
		switch arg(0, 0) {
		case 0:
			s.erase(s.y, s.x, s.y, s.w-1)
		case 1:
			s.erase(s.y, 0, s.y, s.x)
		case 2:
			s.erase(s.y, 0, s.y, s.w-1)
		}
	case 'J':
		switch arg(0, 0) {
		case 0:
			s.erase(s.y, s.x, s.h-1, s.w-1)
		case 1:
			s.erase(0, 0, s.y, s.x)
		case 2, 3:
			s.erase(0, 0, s.h-1, s.w-1)
		}
	case 'X':
		s.erase(s.y, s.x, s.y, min(s.x+arg(0, 1), s.w)-1)
	case 's':
		s.sx, s.sy = s.x, s.y
	case 'u':
		s.x, s.y = s.sx, s.sy
	}
}

// numbers are a sequence's parameters, separated by ; or :. One left out
// is 0.
func numbers(p []byte) []int {
	n := []int{0}
	for _, b := range p {
		switch {
		case b >= '0' && b <= '9':
			if v := n[len(n)-1]; v < 100000 {
				n[len(n)-1] = v*10 + int(b-'0')
			}
		case b == ';' || b == ':':
			n = append(n, 0)
		}
	}
	return n
}

// erase blanks the screen's cells from (y0, x0) to (y1, x1), in reading
// order. A row blanked whole no longer continues the one above.
func (s *screen) erase(y0, x0, y1, x1 int) {
	for y := y0; y <= y1; y++ {
		r := s.at(y)
		from, to := 0, s.w-1
		if y == y0 {
			from = x0
		}
		if y == y1 {
			to = x1
		}
		for x := from; x <= to; x++ {
			if r.cells[x] != 0 {
				r.cells[x], r.dirty = 0, true
			}
		}
		if from == 0 && to == s.w-1 {
			r.cont = false
		}
	}
}

// lineFeed moves the cursor down a row, scrolling at the bottom.
func (s *screen) lineFeed() {
	s.wrap = false
	if s.y < s.h-1 {
		s.y++
		return
	}
	s.rows = append(s.rows, s.newRow())
	if extra := len(s.rows) - s.h - scrollback; extra > 0 {
		s.rows = s.rows[extra:]
	}
}

// put writes r at the cursor and moves it on.
func (s *screen) put(r rune) {
	wd := width(r)
	if wd == 0 {
		return
	}
	if s.wrap || s.x+wd > s.w {
		s.lineFeed()
		s.x = 0
		s.at(s.y).cont = true
	}
	cur := s.at(s.y)
	if cur.cells[s.x] != r {
		cur.cells[s.x], cur.dirty = r, true
	}
	if wd == 2 {
		cur.cells[s.x+1] = wideTail
	}
	s.wrap = s.x+wd >= s.w
	if !s.wrap {
		s.x += wd
	}
}

// width is how many cells r takes: 0 for a control or combining mark, 2 for
// an East Asian wide character or an emoji, 1 otherwise.
func width(r rune) int {
	switch {
	case r < 0x20 || (r >= 0x7f && r < 0xa0):
		return 0
	case r >= 0x300 && r <= 0x36f, r >= 0x200b && r <= 0x200f, r >= 0xfe00 && r <= 0xfe0f:
		return 0
	case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf, r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f, r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6, r >= 0x1f300 && r <= 0x1f64f, r >= 0x1f900 && r <= 0x1f9ff,
		r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	return 1
}

// report passes each line with a changed row to seen, once: a line is a row
// and the rows that continue it. partial leaves the cursor's line for later,
// as it may be half drawn.
func (s *screen) report(partial bool) {
	cursor := s.top() + s.y
	for start := 0; start < len(s.rows); {
		end := start + 1
		for end < len(s.rows) && s.rows[end].cont {
			end++
		}
		dirty := false
		for _, r := range s.rows[start:end] {
			dirty = dirty || r.dirty
		}
		if dirty && !(partial && cursor >= start && cursor < end) {
			var line []rune
			for i, r := range s.rows[start:end] {
				r.dirty = false
				for _, c := range r.cells {
					switch c {
					case wideTail:
					case 0:
						line = append(line, ' ')
					default:
						line = append(line, c)
					}
				}
				if i == end-start-1 {
					for len(line) > 0 && line[len(line)-1] == ' ' {
						line = line[:len(line)-1]
					}
				}
			}
			s.seen(string(line))
		}
		start = end
	}
}
