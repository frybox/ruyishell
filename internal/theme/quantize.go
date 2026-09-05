package theme

import "fmt"

// xterm256 returns the standard xterm 256-color RGB table: indices 0-15 are
// the basic ANSI colors, 16-231 the 6x6x6 color cube, 232-255 a grayscale
// ramp. It is built once.
func xterm256() [256]RGB {
	var p [256]RGB
	basic := [16]RGB{
		{0, 0, 0}, {128, 0, 0}, {0, 128, 0}, {128, 128, 0}, {0, 0, 128},
		{128, 0, 128}, {0, 128, 128}, {192, 192, 192},
		{128, 128, 128}, {255, 0, 0}, {0, 255, 0}, {255, 255, 0}, {0, 0, 255},
		{255, 0, 255}, {0, 255, 255}, {255, 255, 255},
	}
	for i := 0; i < 16; i++ {
		p[i] = basic[i]
	}
	levels := []uint8{0, 95, 135, 175, 215, 255}
	i := 16
	for _, r := range levels {
		for _, g := range levels {
			for _, b := range levels {
				p[i] = RGB{r, g, b}
				i++
			}
		}
	}
	for i := 0; i < 24; i++ {
		v := uint8(8 + i*10)
		p[232+i] = RGB{v, v, v}
	}
	return p
}

var x256 = xterm256()

// nearest256 returns the index 0-255 of the table entry nearest to c, using
// a perceptually-weighted squared distance (green contributes most, red next,
// blue least).
func nearest256(c RGB) int {
	best, bestD := 0, 1<<31-1
	for i, e := range x256 {
		d := dist(c, e)
		if d < bestD {
			best, bestD = i, d
		}
	}
	return best
}

// dist is a weighted squared RGB distance (2:4:1 r:g:b).
func dist(a, b RGB) int {
	r := int(a.R) - int(b.R)
	g := int(a.G) - int(b.G)
	bl := int(a.B) - int(b.B)
	return 2*r*r + 4*g*g + bl*bl
}

// nearest16 returns the index 0-15 of the basic ANSI color nearest to c.
func nearest16(c RGB) int {
	return nearest256(c) % 16 // 0-15 of the table are the basic colors
}

// sgrFg renders c as a foreground SGR fragment for level.
func sgrFg(level Level, c RGB) string {
	switch level {
	case LTrueColor:
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.R, c.G, c.B)
	case LAnsi256:
		return fmt.Sprintf("\x1b[38;5;%dm", nearest256(c))
	case LBasic:
		return fmt.Sprintf("\x1b[%dm", 30+nearest16(c))
	default:
		return ""
	}
}

// sgrBg renders c as a background SGR fragment for level.
func sgrBg(level Level, c RGB) string {
	switch level {
	case LTrueColor:
		return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", c.R, c.G, c.B)
	case LAnsi256:
		return fmt.Sprintf("\x1b[48;5;%dm", nearest256(c))
	case LBasic:
		return fmt.Sprintf("\x1b[%dm", 40+nearest16(c))
	default:
		return ""
	}
}
